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

	// afterSeen / lineBase：最后已应用的服务端快照里各正式行当时的后继与正文。乐观本地改动不写这里。
	afterSeen map[string]string
	lineBase  map[string]string

	stateDir   string // 空则用 UserConfigDir；测试可注入临时目录
	sessions   map[string]*persistSession
	lastArt    string
	serverSnap *protocol.Snapshot
	rejected   []rejectedOp
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

func (a *App) SetServer(raw string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return
	}
	a.serverURL = strings.TrimRight(raw, "/")
	_ = a.savePersistLocked()
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
		a.queue = nil
		a.rejected = nil
		a.serverSnap = nil
		a.sentCount = 0
		a.sentSeq = 0
		a.deferred = nil
		a.doc = nil
		a.afterSeen = nil
		a.lineBase = nil
		if a.conn != nil {
			_ = a.conn.Close()
			a.conn = nil
		}
		a.joined = false
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
	_ = a.savePersistLocked()
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
	return a.serverSnap != nil || len(a.queue) > 0
}

func (a *App) emitOfflineLocked(offline bool) {
	if a.ctx == nil {
		return
	}
	runtime.EventsEmit(a.ctx, "offline", offline)
}

func (a *App) SubmitEdit(lineID, content string) error {
	return a.enqueueSubmit(lineID, model.ActionEdit, []string{content})
}

func (a *App) SubmitPaste(lineID string, lines []string) error {
	if len(lines) == 0 {
		return fmt.Errorf("粘贴内容为空")
	}
	return a.enqueueSubmit(lineID, model.ActionEdit, lines)
}

// SubmitSpanEdit 跨多条正式行整段替换。基准只取 serverSnap，一个范围一个 Op。
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
	baseIDs, baseTexts, afterSeen, err := a.spanBasesFromServerSnapLocked(startLineID, endLineID)
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
	return a.enqueueSubmit(lineID, model.ActionInsert, lines)
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
	holders := a.holdersLocked(lineID)
	if err := a.doc.DeleteIfIdle(a.personID, id, holders); err != nil {
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
	holders := a.holdersLocked(lineID)
	if err := a.doc.MergeUp(a.personID, id, holders); err != nil {
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
	a.mu.Lock()
	defer a.mu.Unlock()
	msg := protocol.CursorMsg{
		Type: protocol.TypeCursor,
		Cursor: protocol.Cursor{
			PersonID:  a.personID,
			Name:      a.name,
			LineID:    lineID,
			DisputeID: disputeID,
			Offset:    offset,
			SelEnd:    selEnd,
		},
	}
	return a.sendAfterEditsLocked(msg)
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
	if err := a.doc.SetSuspended(a.personID, id, action, on); err != nil {
		return err
	}
	if err := a.pushOpLocked(protocol.Op{
		Kind: protocol.TypeSuspend,
		Suspend: &protocol.Suspend{
			Type:      protocol.TypeSuspend,
			PersonID:  a.personID,
			LineID:    lineID,
			Action:    action,
			Suspended: on,
		},
	}); err != nil {
		return err
	}
	a.forceFlushLocked()
	a.emitSnapshotLocked()
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
	ts := time.Now().UnixMilli()
	if _, err := a.doc.RequestFollow(a.personID, id, ts); err != nil {
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

func (a *App) enqueueSubmit(lineID, action string, content []string) error {
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
	opts, afterSeen, baseContent, lineIDs := a.buildSubmitExtrasLocked(lineID, action, content)
	if err := a.doc.SubmitWith(a.personID, id, action, content, opts); err != nil {
		return err
	}
	op := protocol.Op{
		Kind: protocol.TypeSubmit,
		Submit: &protocol.Submit{
			Type:        protocol.TypeSubmit,
			PersonID:    a.personID,
			LineID:      lineID,
			Action:      action,
			Content:     content,
			ClientTs:    time.Now().UnixMilli(),
			AfterSeen:   afterSeen,
			BaseContent: baseContent,
			LineIDs:     lineIDs,
		},
	}
	if err := a.pushOpLocked(op); err != nil {
		return err
	}
	a.emitSnapshotLocked()
	return nil
}

// spanBasesFromServerSnapLocked：两端及中间基准只来自最后已应用 serverSnap，不看乐观 doc。
func (a *App) spanBasesFromServerSnapLocked(startHex, endHex string) ([]model.ID, []string, model.ID, error) {
	if a.serverSnap == nil {
		return nil, nil, model.ID{}, document.ErrSpanBase
	}
	lines := a.serverSnap.Lines
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

// buildSubmitExtrasLocked：基准后继/正文只取服务端快照；新行 ID 客户端预生。
func (a *App) buildSubmitExtrasLocked(lineID, action string, content []string) (document.SubmitOpts, *string, *string, []string) {
	var opts document.SubmitOpts
	var afterPtr *string
	var basePtr *string
	var idStrs []string
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
	if action == model.ActionEdit && a.lineBase != nil {
		if text, ok := a.lineBase[lineID]; ok {
			cp := text
			basePtr = &cp
			opts.BaseContent = &cp
		}
	}
	n := 0
	if action == model.ActionInsert {
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
	return opts, afterPtr, basePtr, idStrs
}

func (a *App) rememberAfterSeenLocked(lines []model.Line) {
	m := make(map[string]string, len(lines))
	base := make(map[string]string, len(lines))
	for _, ln := range lines {
		m[ln.ID.Hex()] = ln.Next.Hex()
		base[ln.ID.Hex()] = ln.Content
	}
	a.afterSeen = m
	a.lineBase = base
}

func (a *App) httpDo(method, path string, body []byte) (*http.Response, error) {
	a.mu.Lock()
	base := a.serverURL
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
	resp, err := http.DefaultClient.Do(req)
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
