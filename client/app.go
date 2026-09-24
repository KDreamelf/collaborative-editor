package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/KDreamelf/collaborative-editor/internal/document"
	"github.com/KDreamelf/collaborative-editor/internal/model"
	"github.com/KDreamelf/collaborative-editor/internal/protocol"
	"github.com/gorilla/websocket"
	"github.com/wailsapp/wails/v2/pkg/runtime"
)

type ArticleBrief struct {
	ID    string `json:"id"`
	Title string `json:"title"`
}

type App struct {
	ctx context.Context

	mu        sync.Mutex
	serverURL string
	personID  string
	name      string
	articleID string

	doc     *document.Doc
	cursors []protocol.Cursor
	people  []protocol.Person

	conn          *websocket.Conn
	connGen       uint64 // 切文档/重连递增；旧 readLoop 消息丢弃
	dialing       bool
	joined        bool
	reconnectWait time.Duration
	stop          chan struct{}

	queue      []protocol.Op
	deferred   []any
	sentCount  int
	sentSeq    int64
	nextSeq    int64
	oldestAt   time.Time
	lastEnqAt  time.Time
	flushTimer *time.Timer

	// afterSeen / beforeSeen / lineBase：仅旧服务模式用——最后已应用 serverSnap 的后继/前驱/正文；乐观本地改动不写这里。
	// 中立转发（relayReady）发送基准改读本地 Doc 正式链，这三张表之后会删。
	afterSeen  map[string]string
	beforeSeen map[string]string
	lineBase   map[string]string

	stateDir string // 空则用 UserConfigDir；测试可注入临时目录
	sessions map[string]*persistSession
	lastArt  string
	// serverSnap：最近共享链缓存（Bootstrap.Base）；本端正文权威在 doc / persist Relay。
	serverSnap     *protocol.Snapshot
	rejected       []rejectedOp
	saveWarning    string
	saveRetryTimer *time.Timer

	// httpTimeout：HTTP/WS 握手时限；0 表示 10s。测试可注入短值。
	httpTimeout time.Duration

	// 一个人只有一个输入光标。attentionLine 是这处，attentionActive 表示还在写。
	// attending 只留这一行，挪到别处时清掉上一处。
	attentionLine   string
	attentionActive bool
	attending       map[string]bool
	// localText：接受后本端显示的句子。正式行要等大家都追随才改。
	localText map[string]string

	// 在线中立转发（Bootstrap / Relay）状态；connect 发 Join 前清 relayReady/preboot。
	relayReady bool
	preboot    []protocol.RelayEvent
	relaySeen  map[string]bool
	claimIDs   map[string]model.ID      // 本人+行+Action 稳定主张 ID（key 已带 Action）
	ownSent    map[string]model.Dispute // 已抄送的本人主张；收到他人主张时才放入争议视图
	yourLine   string
	// relayBooted：已收到过 relay Bootstrap，或从 persist Relay 恢复。切文档/切服务清掉。
	relayBooted bool
}

func NewApp() *App {
	a := &App{
		serverURL: "http://127.0.0.1:8787",
		nextSeq:   1,
		stop:      make(chan struct{}),
		sessions:  map[string]*persistSession{},
	}
	a.stateDir = defaultStateDir()
	a.loadPersist()
	return a
}

// NewAppWithStateDir 测试用：指定持久目录，跳过默认 UserConfigDir。
func NewAppWithStateDir(dir string) *App {
	a := &App{
		serverURL: "http://127.0.0.1:8787",
		nextSeq:   1,
		stop:      make(chan struct{}),
		sessions:  map[string]*persistSession{},
		stateDir:  dir,
	}
	a.loadPersist()
	return a
}

func (a *App) startup(ctx context.Context) {
	a.ctx = ctx
	a.mu.Lock()
	defer a.mu.Unlock()
	_ = a.savePersistLocked()
	a.emitUnsyncedLocked()
}

func (a *App) PersonID() string {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.personID
}

// ReportError 把前端未知错误按编号追加到本机 client.log，供事后按编号检索。
// 失败静默：不得遮蔽原错误。id/message 均压掉 CR/LF，避免一条伪造多行。
func (a *App) ReportError(id, message string) {
	id = strings.TrimSpace(id)
	id = strings.ReplaceAll(id, "\r", "")
	id = strings.ReplaceAll(id, "\n", "")
	if id == "" {
		return
	}
	message = strings.ReplaceAll(message, "\r", " ")
	message = strings.ReplaceAll(message, "\n", " ")
	dir := a.stateDir
	if dir == "" {
		dir = defaultStateDir()
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return
	}
	path := filepath.Join(dir, clientLogName)
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return
	}
	defer f.Close()
	_, _ = fmt.Fprintf(f, "[%s] %s\n", id, message)
}

func (a *App) GetServer() string {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.serverURL
}

// SetServer 校验服务根地址；有任意待送/未同步时拒绝。切换成功清空旧服务缓存，保留个人身份。
func (a *App) SetServer(raw string) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	normalized, err := normalizeServerURL(raw)
	if err != nil {
		return err
	}
	if normalized == a.serverURL {
		return nil
	}
	if a.hasPendingAnywhereLocked() {
		return fmt.Errorf("还有未发送或未同步的修改，请处理后再切换服务器")
	}
	a.serverURL = normalized
	a.connGen++
	if a.conn != nil {
		_ = a.conn.Close()
		a.conn = nil
	}
	a.joined = false
	a.dialing = false
	a.sentCount = 0
	a.sentSeq = 0
	a.clearSaveWarningLocked()
	a.clearServiceCacheLocked()
	a.emitOfflineLocked(true)
	a.persistAfterMutationLocked()
	return nil
}

// normalizeServerURL 只接受 http(s) 服务根（无 userinfo/query/fragment/path）。
func normalizeServerURL(raw string) (string, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return "", fmt.Errorf("服务器地址不能为空")
	}
	u, err := url.Parse(raw)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") {
		return "", fmt.Errorf("请输入有效的 http 或 https 地址")
	}
	if u.Host == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" {
		return "", fmt.Errorf("请输入有效的 http 或 https 地址")
	}
	if path := strings.TrimRight(u.Path, "/"); path != "" {
		return "", fmt.Errorf("请输入有效的 http 或 https 地址")
	}
	if u.Hostname() == "" {
		return "", fmt.Errorf("请输入有效的 http 或 https 地址")
	}
	if p := u.Port(); p != "" {
		n, err := strconv.Atoi(p)
		if err != nil || n < 1 || n > 65535 {
			return "", fmt.Errorf("请输入有效的 http 或 https 地址")
		}
	}
	return u.Scheme + "://" + u.Host, nil
}

// clearServiceCacheLocked 丢掉旧服务干净缓存/当前文档/会话快照；保留 personID。
func (a *App) clearServiceCacheLocked() {
	a.sessions = map[string]*persistSession{}
	a.queue = nil
	a.rejected = nil
	a.serverSnap = nil
	a.doc = nil
	a.articleID = ""
	a.lastArt = ""
	a.name = ""
	a.afterSeen = nil
	a.beforeSeen = nil
	a.lineBase = nil
	a.people = nil
	a.cursors = nil
	a.deferred = nil
	a.nextSeq = 1
	a.oldestAt = time.Time{}
	a.lastEnqAt = time.Time{}
	a.relayReady = false
	a.preboot = nil
	a.relaySeen = nil
	a.claimIDs = nil
	a.ownSent = nil
	a.yourLine = ""
	a.relayBooted = false
}

func (a *App) hasPendingAnywhereLocked() bool {
	if len(a.queue) > 0 || len(a.rejected) > 0 {
		return true
	}
	for _, sess := range a.sessions {
		if sess == nil {
			continue
		}
		if len(sess.Queue) > 0 || len(sess.Rejected) > 0 {
			return true
		}
	}
	return false
}

// GetSaveWarning 本地写盘失败且正在重试时的提示；空串表示正常。
func (a *App) GetSaveWarning() string {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.saveWarning
}

// GetCachedArticle 最近一份可继续的本地会话（含未发队列或未同步原文）。
func (a *App) GetCachedArticle() *CachedArticle {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.cachedArticleLocked()
}

// ListUnsynced 被服务端拒绝、仍保留原文的修改。
func (a *App) ListUnsynced() []UnsyncedItem {
	a.mu.Lock()
	defer a.mu.Unlock()
	items := a.unsyncedItemsLocked()
	if items == nil {
		return []UnsyncedItem{}
	}
	return items
}

// DismissUnsynced 用户看过/复制后可清掉一条未同步提醒。
func (a *App) DismissUnsynced(id string) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	id = strings.TrimSpace(id)
	if id == "" {
		return nil
	}
	out := a.rejected[:0]
	for _, r := range a.rejected {
		rid := r.ID
		if rid == "" {
			rid = r.Op.ID
		}
		if rid == id {
			continue
		}
		out = append(out, r)
	}
	a.rejected = out
	return a.savePersistLocked()
}

func (a *App) CreateArticle(title string) (string, error) {
	title = strings.TrimSpace(title)
	if title == "" {
		title = "未命名"
	}
	body, _ := json.Marshal(map[string]string{"title": title})
	resp, err := a.httpDo(http.MethodPost, "/api/articles", body)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", err
	}
	if resp.StatusCode >= 300 {
		return "", fmt.Errorf("%s", strings.TrimSpace(string(data)))
	}
	var out struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(data, &out); err != nil {
		return "", err
	}
	if out.ID == "" {
		return "", fmt.Errorf("服务端未返回文章 id")
	}
	return out.ID, nil
}

func (a *App) ListArticles() ([]ArticleBrief, error) {
	resp, err := a.httpDo(http.MethodGet, "/api/articles", nil)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode >= 300 {
		return nil, fmt.Errorf("%s", strings.TrimSpace(string(data)))
	}
	var list []ArticleBrief
	if err := json.Unmarshal(data, &list); err != nil {
		return nil, err
	}
	if list == nil {
		list = []ArticleBrief{}
	}
	return list, nil
}

func (a *App) Join(articleID, name string) error {
	articleID = strings.TrimSpace(articleID)
	name = strings.TrimSpace(name)
	if articleID == "" {
		return fmt.Errorf("缺少文章 id")
	}
	if name == "" {
		name = "未命名"
	}

	a.mu.Lock()
	if a.articleID != "" && a.articleID != articleID {
		if err := a.savePersistLocked(); err != nil {
			a.mu.Unlock()
			return fmt.Errorf("本地保存失败，请检查磁盘后重试")
		}
		a.connGen++
		a.queue = nil
		a.rejected = nil
		a.serverSnap = nil
		a.sentCount = 0
		a.sentSeq = 0
		a.deferred = nil
		a.doc = nil
		a.afterSeen = nil
		a.beforeSeen = nil
		a.lineBase = nil
		a.relayReady = false
		a.preboot = nil
		a.relaySeen = nil
		a.claimIDs = nil
		a.ownSent = nil
		a.yourLine = ""
		a.relayBooted = false
		if a.conn != nil {
			_ = a.conn.Close()
			a.conn = nil
		}
		a.joined = false
		a.emitOfflineLocked(true)
	}
	a.articleID = articleID
	a.name = name
	a.restoreSessionLocked(articleID)
	if a.name == "" || name != "未命名" {
		a.name = name
	}
	if a.doc == nil {
		a.doc = document.New("")
	}
	a.persistAfterMutationLocked()
	a.emitSnapshotLocked()
	a.emitUnsyncedLocked()
	a.mu.Unlock()

	for {
		err := a.connect()
		if err == errDialBusy {
			time.Sleep(50 * time.Millisecond)
			continue
		}
		if err != nil {
			a.mu.Lock()
			if !a.joined {
				if a.hasTrustedLocalLocked() {
					a.joined = true
					_ = a.savePersistLocked()
					a.emitOfflineLocked(true)
					a.mu.Unlock()
					go a.reconnectLater()
					return nil
				}
				a.articleID = ""
			}
			a.mu.Unlock()
			return err
		}
		return nil
	}
}

func (a *App) hasTrustedLocalLocked() bool {
	return a.serverSnap != nil || len(a.queue) > 0 || a.relayBooted
}

func (a *App) emitOfflineLocked(offline bool) {
	if a.ctx == nil {
		return
	}
	runtime.EventsEmit(a.ctx, "offline", offline)
}

func (a *App) SubmitEdit(lineID, content string) error {
	return a.enqueueSubmit(lineID, model.ActionEdit, []string{content}, false)
}

func (a *App) SubmitPaste(lineID string, lines []string) error {
	if len(lines) == 0 {
		return fmt.Errorf("粘贴内容为空")
	}
	return a.enqueueSubmit(lineID, model.ActionEdit, lines, false)
}

// SubmitEditClaim 明确更新本人整份候选（含临时多行缩成一行）。WholeClaim=true。
func (a *App) SubmitEditClaim(lineID string, lines []string) error {
	if len(lines) == 0 {
		return fmt.Errorf("内容为空")
	}
	return a.enqueueSubmit(lineID, model.ActionEdit, lines, true)
}

// SubmitSpanEdit 跨多条正式行整段替换。一个范围一个 Op。
// 中立转发取调用前本地 View；旧模式仍取 serverSnap（兼容，之后删）。
func (a *App) SubmitSpanEdit(startLineID, endLineID string, replacement []string) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.doc == nil {
		return fmt.Errorf("尚未加入文档")
	}
	if len(replacement) == 0 {
		return fmt.Errorf("粘贴内容为空")
	}
	replacement = append([]string(nil), replacement...)
	var (
		baseIDs   []model.ID
		baseTexts []string
		afterSeen model.ID
		err       error
	)
	if a.relayReady || a.relayBooted {
		baseIDs, baseTexts, afterSeen, err = a.spanBasesFromLocalViewLocked(startLineID, endLineID)
	} else {
		baseIDs, baseTexts, afterSeen, err = a.spanBasesFromServerSnapLocked(startLineID, endLineID)
	}
	if err != nil {
		// 乐观新行常不在 serverSnap；保原文进 rejected，避免前端回滚丢字。
		return a.saveLocalSpanRejectLocked(spanRejectDraftOp(a.personID, startLineID, endLineID, replacement))
	}
	var lineIDs []model.ID
	var lineIDStrs []string
	if len(replacement) > 1 {
		n := len(replacement) - 1
		lineIDs = make([]model.ID, n)
		lineIDStrs = make([]string, n)
		for i := 0; i < n; i++ {
			id := model.NewID()
			lineIDs[i] = id
			lineIDStrs[i] = id.Hex()
		}
	}
	baseIDStrs := make([]string, len(baseIDs))
	for i, id := range baseIDs {
		baseIDStrs[i] = id.Hex()
	}
	op := protocol.Op{
		Kind: protocol.TypeSpanEdit,
		SpanEdit: &protocol.SpanEdit{
			Type:        protocol.TypeSpanEdit,
			PersonID:    a.personID,
			BaseIDs:     baseIDStrs,
			BaseTexts:   append([]string(nil), baseTexts...),
			AfterSeen:   afterSeen.Hex(),
			Replacement: replacement,
			LineIDs:     lineIDStrs,
			ClientTs:    time.Now().UnixMilli(),
		},
	}
	opts := document.SpanEditOpts{
		BaseIDs:     append([]model.ID(nil), baseIDs...),
		BaseTexts:   append([]string(nil), baseTexts...),
		AfterSeen:   afterSeen,
		Replacement: replacement,
		LineIDs:     lineIDs,
	}
	if a.relayBooted || a.relayReady {
		v, err := a.doc.View()
		if err != nil {
			return err
		}
		var foreign *model.Dispute
		for _, claim := range v.Disputes {
			if claim.Person == a.personID {
				continue
			}
			for i, baseID := range baseIDs {
				if claim.RealLine != baseID {
					continue
				}
				if i != 0 || claimSlotKey(claim.RealLine, claim.Action) != claimSlotKey(baseIDs[0], model.ActionEdit) {
					return a.saveLocalSpanRejectLocked(op)
				}
				foreign = &claim
				break
			}
		}
		if foreign != nil {
			working := a.doc.Clone()
			if err := working.SubmitSpanEdit(a.personID, opts); err != nil {
				return a.saveLocalSpanRejectLocked(op)
			}
			view, err := working.View()
			if err != nil {
				return err
			}
			for _, own := range view.Disputes {
				if own.RealLine == baseIDs[0] && own.Person == a.personID && own.Action == model.ActionEdit {
					a.doc = working
					a.rememberClaimIDLocked(baseIDs[0].Hex(), own.Action, own.ID)
					if err := a.pushOpLocked(protocol.Op{Kind: protocol.TypeDisputeCC,
						DisputeCC: &protocol.DisputeCC{TargetPersonID: foreign.Person, Claim: own}}); err != nil {
						return err
					}
					a.emitSnapshotLocked()
					return nil
				}
			}
			return a.saveLocalSpanRejectLocked(op)
		}
	}
	if err := a.doc.SubmitSpanEdit(a.personID, opts); err != nil {
		if errors.Is(err, document.ErrSpanBlocked) ||
			errors.Is(err, document.ErrSpanBase) ||
			errors.Is(err, document.ErrStaleEdit) {
			return a.saveLocalSpanRejectLocked(op)
		}
		// 未知错：尽量保原文，仍抛原错给 UI 日志 ID。
		_ = a.storeLocalSpanRejectLocked(op)
		return err
	}
	if err := a.pushOpLocked(op); err != nil {
		return err
	}
	a.emitSnapshotLocked()
	return nil
}

func (a *App) SubmitInsert(lineID string, lines []string) error {
	if lines == nil {
		lines = []string{""}
	}
	if len(lines) == 0 {
		lines = []string{""}
	}
	return a.enqueueSubmit(lineID, model.ActionInsert, lines, false)
}

// SubmitInsertBefore 行首 Enter：在当前正式行前插入。争议 RealLine 仍指该原行。
func (a *App) SubmitInsertBefore(lineID string, lines []string) error {
	if lines == nil {
		lines = []string{""}
	}
	if len(lines) == 0 {
		lines = []string{""}
	}
	return a.enqueueSubmit(lineID, model.ActionInsertBefore, lines, false)
}

func (a *App) DeleteLine(lineID string) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.doc == nil {
		return fmt.Errorf("尚未加入文档")
	}
	id, err := model.ParseID(lineID)
	if err != nil {
		return err
	}
	if a.relayBooted || a.relayReady {
		foreign, err := a.foreignClaimAtLocked(id)
		if err != nil {
			return err
		}
		if foreign != nil {
			if err := a.doc.DeleteIfIdle(a.personID, id, nil); err != nil {
				return err
			}
			v, err := a.doc.View()
			if err != nil {
				return err
			}
			for _, claim := range v.Disputes {
				if claim.RealLine == id && claim.Person == a.personID && claim.Action == model.ActionDelete {
					a.rememberClaimIDLocked(lineID, claim.Action, claim.ID)
					if err := a.pushOpLocked(protocol.Op{Kind: protocol.TypeDisputeCC, DisputeCC: &protocol.DisputeCC{
						TargetPersonID: foreign.Person, Claim: claim,
					}}); err != nil {
						return err
					}
					a.emitSnapshotLocked()
					return nil
				}
			}
			return document.ErrBroken
		}
		if err := a.doc.ApplyPlainDelete(id); err != nil {
			return err
		}
	} else if err := a.doc.DeleteIfIdle(a.personID, id, a.holdersLocked(lineID)); err != nil {
		return err
	}
	op := protocol.Op{
		Kind: protocol.TypeDelete,
		Delete: &protocol.Delete{
			Type:     protocol.TypeDelete,
			PersonID: a.personID,
			LineID:   lineID,
		},
	}
	if err := a.pushOpLocked(op); err != nil {
		return err
	}
	a.emitSnapshotLocked()
	return nil
}

func (a *App) MergeUp(lineID string) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.doc == nil {
		return fmt.Errorf("尚未加入文档")
	}
	id, err := model.ParseID(lineID)
	if err != nil {
		return err
	}
	if a.relayBooted || a.relayReady {
		if err := a.doc.ApplyPlainMerge(id); err != nil {
			return err
		}
	} else if err := a.doc.MergeUp(a.personID, id, a.holdersLocked(lineID)); err != nil {
		return err
	}
	op := protocol.Op{
		Kind: protocol.TypeMerge,
		Merge: &protocol.Merge{
			Type:     protocol.TypeMerge,
			PersonID: a.personID,
			LineID:   lineID,
		},
	}
	if err := a.pushOpLocked(op); err != nil {
		return err
	}
	a.emitSnapshotLocked()
	return nil
}

func (a *App) MoveCaret(lineID, disputeID string, offset, selEnd int) error {
	return a.MoveCaretRange(lineID, disputeID, 0, offset, "", "", 0, selEnd)
}

// MoveCaretRange 跨行/分段选区光标；零值字段兼容旧 Cursor。
// 本机注意力按起点行立即更新，不依赖光标报文是否发出。
func (a *App) MoveCaretRange(lineID, disputeID string, partIndex, offset int, selEndLineID, selEndDisputeID string, selEndPartIndex, selEnd int) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.setAttentionFromCaretLocked(lineID, disputeID)
	msg := protocol.CursorMsg{
		Type: protocol.TypeCursor,
		Cursor: protocol.Cursor{
			PersonID:        a.personID,
			Name:            a.name,
			LineID:          lineID,
			DisputeID:       disputeID,
			Offset:          offset,
			SelEnd:          selEnd,
			PartIndex:       partIndex,
			SelEndLineID:    selEndLineID,
			SelEndDisputeID: selEndDisputeID,
			SelEndPartIndex: selEndPartIndex,
		},
	}
	return a.sendAfterEditsLocked(msg)
}

func (a *App) setAttentionFromCaretLocked(lineID, disputeID string) {
	if lineID == "" {
		a.attentionLine = ""
		a.attentionActive = false
		return
	}
	on := disputeID == "" || a.selfOwnsDisputeLocked(disputeID)
	a.attentionLine = lineID
	a.setLineAttendingLocked(lineID, on)
	a.attentionLine = lineID
	a.attentionActive = on
}

func (a *App) lineAttendingLocked(line string) bool {
	if line == "" {
		return false
	}
	if a.attending != nil {
		return a.attending[line]
	}
	return a.attentionActive && a.attentionLine == line
}

func (a *App) setLineAttendingLocked(line string, on bool) {
	if line == "" {
		return
	}
	if on {
		a.attending = map[string]bool{line: true}
		a.attentionLine = line
		a.attentionActive = true
		return
	}
	if a.attending != nil {
		for k := range a.attending {
			delete(a.attending, k)
		}
	}
	a.attentionLine = line
	a.attentionActive = false
}

// disputeRealLineHexLocked 取主张 RealLine；追随 apply 前调用（收口后争议可能已消失）。
func (a *App) disputeRealLineHexLocked(disputeID model.ID) string {
	if a.doc == nil || disputeID.IsZero() {
		return ""
	}
	v, err := a.doc.View()
	if err != nil {
		return ""
	}
	for _, d := range v.Disputes {
		if d.ID == disputeID {
			return d.RealLine.Hex()
		}
	}
	return ""
}

// deactivateAttentionIfOnLineLocked 追随放下主张：同行仅失活注意力，不清空光标行。
func (a *App) deactivateAttentionIfOnLineLocked(lineHex string) {
	if lineHex == "" {
		return
	}
	a.setLineAttendingLocked(lineHex, false)
	if a.attentionLine == lineHex {
		a.attentionActive = false
	}
}

func (a *App) selfOwnsDisputeLocked(disputeID string) bool {
	if a.doc == nil || a.personID == "" {
		return false
	}
	id, err := model.ParseID(disputeID)
	if err != nil || id.IsZero() {
		return false
	}
	v, err := a.doc.View()
	if err != nil {
		return false
	}
	for _, d := range v.Disputes {
		if d.ID == id && d.Person == a.personID {
			return true
		}
	}
	return false
}

func (a *App) hasForeignDisputeLocked(line model.ID, action string) bool {
	if a.doc == nil {
		return false
	}
	v, err := a.doc.View()
	if err != nil {
		return false
	}
	for _, d := range v.Disputes {
		if d.RealLine == line && d.Action == action && d.Person != a.personID {
			return true
		}
	}
	return false
}

func (a *App) foreignClaimAtLocked(line model.ID) (*model.Dispute, error) {
	v, err := a.doc.View()
	if err != nil {
		return nil, err
	}
	for _, claim := range v.Disputes {
		if claim.RealLine == line && claim.Person != a.personID {
			return &claim, nil
		}
	}
	return nil, nil
}

func (a *App) foreignClaimForSlotLocked(line model.ID, action string) (*model.Dispute, error) {
	v, err := a.doc.View()
	if err != nil {
		return nil, err
	}
	slot := claimSlotKey(line, action)
	for _, claim := range v.Disputes {
		if claim.Person != a.personID && claimSlotKey(claim.RealLine, claim.Action) == slot {
			return &claim, nil
		}
	}
	return nil, nil
}

func (a *App) Suspend(lineID, action string, on bool) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.doc == nil {
		return fmt.Errorf("尚未加入文档")
	}
	id, err := model.ParseID(lineID)
	if err != nil {
		return err
	}
	if id.IsZero() {
		return fmt.Errorf("无效行ID")
	}
	// 停笔只放下这一行的注意力，不向服务器发挂起，也不改主张。
	// action 仍留给界面传入，服务器不再接收挂起包。
	_ = action
	if on {
		a.setLineAttendingLocked(lineID, false)
		if a.attentionLine == lineID {
			a.attentionActive = false
		}
		return nil
	}
	a.attentionLine = lineID
	a.setLineAttendingLocked(lineID, true)
	a.attentionActive = true
	return nil
}

func (a *App) RequestFollow(disputeID string) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.doc == nil {
		return fmt.Errorf("尚未加入文档")
	}
	id, err := model.ParseID(disputeID)
	if err != nil {
		return err
	}
	var lineHex, body string
	if view, viewErr := a.doc.View(); viewErr == nil {
		for _, d := range view.Disputes {
			if d.ID == id && len(d.Content) > 0 {
				lineHex = d.RealLine.Hex()
				body = d.Content[0]
				break
			}
		}
	}
	ts := time.Now().UnixMilli()
	out, err := a.doc.RequestFollow(a.personID, id, ts)
	if err != nil {
		return err
	}
	if err := a.pushOpLocked(protocol.Op{
		Kind: protocol.TypeFollow,
		Follow: &protocol.Follow{
			Type:      protocol.TypeFollow,
			PersonID:  a.personID,
			DisputeID: disputeID,
			ClientTs:  ts,
		},
	}); err != nil {
		return err
	}
	a.forceFlushLocked()
	if lineHex != "" {
		if a.localText == nil {
			a.localText = map[string]string{}
		}
		a.localText[lineHex] = body
		a.deactivateAttentionIfOnLineLocked(lineHex)
	}
	a.emitSnapshotLocked()
	a.emitFollowResultLocked(out.Status, disputeID)
	return nil
}

// RejectClaim 不追随这份主张，把本端当前句子再抄送给对方。正式行不动。
func (a *App) RejectClaim(disputeID string) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.doc == nil {
		return fmt.Errorf("尚未加入文档")
	}
	id, err := model.ParseID(disputeID)
	if err != nil {
		return err
	}
	view, err := a.doc.View()
	if err != nil {
		return err
	}
	var claim *model.Dispute
	for i := range view.Disputes {
		if view.Disputes[i].ID == id {
			claim = &view.Disputes[i]
			break
		}
	}
	if claim == nil || claim.Person == "" || claim.Person == a.personID {
		return fmt.Errorf("没有这份他人主张")
	}
	lineHex := claim.RealLine.Hex()
	body := ""
	if a.localText != nil {
		body = a.localText[lineHex]
	}
	if body == "" {
		for _, ln := range view.Lines {
			if ln.ID == claim.RealLine {
				body = ln.Content
				break
			}
		}
	}
	a.attentionLine = lineHex
	a.setLineAttendingLocked(lineHex, true)
	a.attentionActive = true
	if body == "" {
		a.emitSnapshotLocked()
		return nil
	}
	ownID := a.stableClaimIDLocked(lineHex, model.ActionEdit)
	if err := a.pushOpLocked(protocol.Op{
		Kind: protocol.TypeDisputeCC,
		DisputeCC: &protocol.DisputeCC{
			TargetPersonID: claim.Person,
			Claim: model.Dispute{
				ID:        ownID,
				RealLine:  claim.RealLine,
				Action:    model.ActionEdit,
				Person:    a.personID,
				Content:   []string{body},
				Followers: []string{},
			},
		},
	}); err != nil {
		return err
	}
	a.forceFlushLocked()
	a.emitSnapshotLocked()
	return nil
}

func (a *App) AnswerFollow(fromID, disputeID string, accept bool) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.doc == nil {
		return fmt.Errorf("尚未加入文档")
	}
	id, err := model.ParseID(disputeID)
	if err != nil {
		return err
	}
	if _, err := a.doc.AnswerFollow(a.personID, fromID, id, accept); err != nil {
		return err
	}
	if err := a.pushOpLocked(protocol.Op{
		Kind: protocol.TypeFollowAnswer,
		FollowAnswer: &protocol.FollowAnswer{
			Type:      protocol.TypeFollowAnswer,
			PersonID:  a.personID,
			FromID:    fromID,
			DisputeID: disputeID,
			Accept:    accept,
		},
	}); err != nil {
		return err
	}
	a.forceFlushLocked()
	a.emitSnapshotLocked()
	return nil
}

func (a *App) enqueueSubmit(lineID, action string, content []string, wholeClaim bool) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.doc == nil {
		return fmt.Errorf("尚未加入文档")
	}
	id, err := model.ParseID(lineID)
	if err != nil {
		return err
	}
	content = append([]string(nil), content...)
	opts, afterSeen, beforeSeen, baseContent, lineIDs := a.buildSubmitExtrasLocked(lineID, action, content)
	opts.WholeClaim = wholeClaim
	// 先构造 Op：领域拒绝时仍能保留可复制原文（与 span 一致）。
	op := protocol.Op{
		ID:   model.NewID().Hex(),
		Kind: protocol.TypeSubmit,
		Submit: &protocol.Submit{
			Type:        protocol.TypeSubmit,
			PersonID:    a.personID,
			LineID:      lineID,
			Action:      action,
			Content:     content,
			ClientTs:    time.Now().UnixMilli(),
			AfterSeen:   afterSeen,
			BeforeSeen:  beforeSeen,
			BaseContent: baseContent,
			LineIDs:     lineIDs,
			WholeClaim:  wholeClaim,
		},
	}
	if a.relayBooted || a.relayReady {
		ids := make([]model.ID, len(lineIDs))
		for i, raw := range lineIDs {
			parsed, err := model.ParseID(raw)
			if err != nil {
				return err
			}
			ids[i] = parsed
		}
		if err := a.doc.ApplyPlainSubmit(a.personID, id, action, content, ids); err != nil {
			return a.saveLocalSpanRejectLocked(op)
		}
		a.attentionLine = lineID
		a.setLineAttendingLocked(lineID, true)
		a.attentionActive = true
		delete(a.localText, lineID)
		if sent, ok := a.ownSent[claimSlotKey(id, action)]; ok {
			sent.Content = append([]string(nil), content...)
			a.ownSent[claimSlotKey(id, action)] = sent
			if view, viewErr := a.doc.View(); viewErr == nil {
				for _, item := range view.Disputes {
					if item.ID == sent.ID {
						_ = a.doc.StoreExplicitClaim(sent)
						break
					}
				}
			}
		}
		if err := a.pushOpLocked(op); err != nil {
			return err
		}
		a.emitSnapshotLocked()
		return nil
	}
	if err := a.doc.SubmitWith(a.personID, id, action, content, opts); err != nil {
		return a.saveLocalSpanRejectLocked(op)
	}
	if err := a.pushOpLocked(op); err != nil {
		return err
	}
	a.emitSnapshotLocked()
	return nil
}

// spanBasesFromLinesLocked：从已排序正式行切片取跨度基准（调用前快照，不含本次乐观结果）。
func spanBasesFromLinesLocked(lines []model.Line, startHex, endHex string) ([]model.ID, []string, model.ID, error) {
	startIdx, endIdx := -1, -1
	for i, ln := range lines {
		h := ln.ID.Hex()
		if h == startHex {
			startIdx = i
		}
		if h == endHex {
			endIdx = i
		}
	}
	if startIdx < 0 || endIdx < 0 || endIdx < startIdx || endIdx-startIdx+1 < 2 {
		return nil, nil, model.ID{}, document.ErrSpanBase
	}
	n := endIdx - startIdx + 1
	baseIDs := make([]model.ID, n)
	baseTexts := make([]string, n)
	for i := 0; i < n; i++ {
		ln := lines[startIdx+i]
		if ln.ID.IsZero() {
			return nil, nil, model.ID{}, document.ErrSpanBase
		}
		baseIDs[i] = ln.ID
		baseTexts[i] = ln.Content
	}
	return baseIDs, baseTexts, lines[endIdx].Next, nil
}

// spanBasesFromLocalViewLocked：中立转发——跨度基准取调用前本地 Doc 正式链。
func (a *App) spanBasesFromLocalViewLocked(startHex, endHex string) ([]model.ID, []string, model.ID, error) {
	if a.doc == nil {
		return nil, nil, model.ID{}, document.ErrSpanBase
	}
	v, err := a.doc.View()
	if err != nil {
		return nil, nil, model.ID{}, document.ErrSpanBase
	}
	return spanBasesFromLinesLocked(v.Lines, startHex, endHex)
}

// spanBasesFromServerSnapLocked：旧模式兼容——两端及中间基准只来自最后已应用 serverSnap，不看乐观 doc。之后删。
func (a *App) spanBasesFromServerSnapLocked(startHex, endHex string) ([]model.ID, []string, model.ID, error) {
	if a.serverSnap == nil {
		return nil, nil, model.ID{}, document.ErrSpanBase
	}
	return spanBasesFromLinesLocked(a.serverSnap.Lines, startHex, endHex)
}

// buildSubmitExtrasLocked：组装 SubmitOpts 与包字段。新行 ID 客户端预生。
// 中立转发（relayReady）：AfterSeen/BeforeSeen/BaseContent 取调用前本地 View 正式链。
// 旧模式：仍取 serverSnap 映射（兼容旧服务测试，之后删）。
func (a *App) buildSubmitExtrasLocked(lineID, action string, content []string) (document.SubmitOpts, *string, *string, *string, []string) {
	var opts document.SubmitOpts
	var afterPtr *string
	var beforePtr *string
	var basePtr *string
	var idStrs []string
	if a.relayReady || a.relayBooted {
		if a.doc != nil {
			if v, err := a.doc.View(); err == nil {
				for _, ln := range v.Lines {
					if ln.ID.Hex() != lineID {
						continue
					}
					switch action {
					case model.ActionInsert:
						next := ln.Next.Hex()
						afterPtr = &next
						seen := ln.Next
						opts.AfterSeen = &seen
					case model.ActionInsertBefore:
						prev := ln.Prev.Hex()
						beforePtr = &prev
						seen := ln.Prev
						opts.BeforeSeen = &seen
					case model.ActionEdit:
						cp := ln.Content
						basePtr = &cp
						opts.BaseContent = &cp
					}
					break
				}
			}
		}
	} else {
		// 旧模式兼容：基准仍取最后已应用 serverSnap 映射。
		if action == model.ActionInsert && a.afterSeen != nil {
			if next, ok := a.afterSeen[lineID]; ok {
				cp := next
				afterPtr = &cp
				seen, err := model.ParseID(next)
				if err == nil {
					opts.AfterSeen = &seen
				}
			}
		}
		if action == model.ActionInsertBefore && a.beforeSeen != nil {
			if prev, ok := a.beforeSeen[lineID]; ok {
				cp := prev
				beforePtr = &cp
				seen, err := model.ParseID(prev)
				if err == nil {
					opts.BeforeSeen = &seen
				}
			}
		}
		if action == model.ActionEdit && a.lineBase != nil {
			if text, ok := a.lineBase[lineID]; ok {
				cp := text
				basePtr = &cp
				opts.BaseContent = &cp
			}
		}
	}
	n := 0
	if action == model.ActionInsert || action == model.ActionInsertBefore {
		n = len(content)
	} else if action == model.ActionEdit && len(content) > 1 {
		n = len(content) - 1
	}
	if n > 0 {
		opts.LineIDs = make([]model.ID, n)
		idStrs = make([]string, n)
		for i := 0; i < n; i++ {
			id := model.NewID()
			opts.LineIDs[i] = id
			idStrs[i] = id.Hex()
		}
	}
	return opts, afterPtr, beforePtr, basePtr, idStrs
}

func (a *App) rememberAfterSeenLocked(lines []model.Line) {
	m := make(map[string]string, len(lines))
	before := make(map[string]string, len(lines))
	base := make(map[string]string, len(lines))
	for _, ln := range lines {
		m[ln.ID.Hex()] = ln.Next.Hex()
		before[ln.ID.Hex()] = ln.Prev.Hex()
		base[ln.ID.Hex()] = ln.Content
	}
	a.afterSeen = m
	a.beforeSeen = before
	a.lineBase = base
}

func (a *App) requestTimeout() time.Duration {
	if a.httpTimeout > 0 {
		return a.httpTimeout
	}
	return 10 * time.Second
}

func (a *App) httpDo(method, path string, body []byte) (*http.Response, error) {
	a.mu.Lock()
	base := a.serverURL
	timeout := a.requestTimeout()
	a.mu.Unlock()
	var rdr io.Reader
	if body != nil {
		rdr = bytes.NewReader(body)
	}
	req, err := http.NewRequest(method, base+path, rdr)
	if err != nil {
		return nil, fmt.Errorf("无法连接服务器")
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	client := &http.Client{Timeout: timeout}
	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("无法连接服务器")
	}
	return resp, nil
}

func (a *App) wsURL() (string, error) {
	u, err := url.Parse(a.serverURL)
	if err != nil {
		return "", err
	}
	switch u.Scheme {
	case "https":
		u.Scheme = "wss"
	default:
		u.Scheme = "ws"
	}
	u.Path = "/ws"
	u.RawQuery = ""
	u.Fragment = ""
	return u.String(), nil
}

func (a *App) holdersLocked(lineID string) []document.Presence {
	out := make([]document.Presence, 0, len(a.cursors))
	for _, c := range a.cursors {
		if c.PersonID == a.personID || c.LineID != lineID {
			continue
		}
		out = append(out, document.Presence{Person: c.PersonID, Active: true})
	}
	return out
}
