package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/KDreamelf/collaborative-editor/internal/document"
	"github.com/KDreamelf/collaborative-editor/internal/model"
	"github.com/KDreamelf/collaborative-editor/internal/protocol"
	"github.com/wailsapp/wails/v2/pkg/runtime"
)

const persistAppDir = "collaborative-editor"
const persistFileName = "state.json"
const clientLogName = "client.log"

// persistRoot 本机应用状态：身份 + 按文章隔离的队列/快照/被拒操作。
type persistRoot struct {
	PersonID  string                     `json:"personId"`
	ServerURL string                     `json:"serverUrl"`
	LastArt   string                     `json:"lastArticleId,omitempty"`
	Sessions  map[string]*persistSession `json:"sessions"`
}

type persistSession struct {
	Name     string             `json:"name"`
	NextSeq  int64              `json:"nextSeq"`
	Queue    []protocol.Op      `json:"queue"`
	Snapshot *protocol.Snapshot `json:"snapshot,omitempty"`
	Rejected []rejectedOp       `json:"rejected,omitempty"`
	// Relay：中立客户端本端主观视图；旧 Snapshot 仍是服务器 Base。
	Relay *persistRelay `json:"relay,omitempty"`
}

// persistRelay 本端已收到 relay Bootstrap 后才写入。
type persistRelay struct {
	View     document.View     `json:"view"`
	Seen     []string          `json:"seen,omitempty"`
	ClaimIDs map[string]string `json:"claimIds,omitempty"`
	YourLine string            `json:"yourLine,omitempty"`
}

// rejectedOp 服务端明确拒绝、保留原文的未同步修改。
type rejectedOp struct {
	ID      string      `json:"id"`
	Op      protocol.Op `json:"op"`
	Message string      `json:"message"`
	At      int64       `json:"at"`
}

// UnsyncedItem 给前端的自然文案条目。
type UnsyncedItem struct {
	ID      string `json:"id"`
	Text    string `json:"text"`
	Summary string `json:"summary"`
}

// CachedArticle 可继续的本地会话提示。
type CachedArticle struct {
	ID            string `json:"id"`
	Title         string `json:"title"`
	Name          string `json:"name"`
	PendingCount  int    `json:"pendingCount"`
	UnsyncedCount int    `json:"unsyncedCount"`
}

func defaultStateDir() string {
	base, err := os.UserConfigDir()
	if err != nil || base == "" {
		base = os.TempDir()
	}
	return filepath.Join(base, persistAppDir)
}

func (a *App) persistPath() string {
	dir := a.stateDir
	if dir == "" {
		dir = defaultStateDir()
	}
	return filepath.Join(dir, persistFileName)
}

func (a *App) loadPersist() {
	path := a.persistPath()
	data, err := os.ReadFile(path)
	if err != nil {
		if a.personID == "" {
			a.personID = model.NewID().Hex()
		}
		if a.nextSeq <= 0 {
			a.nextSeq = 1
		}
		a.sessions = map[string]*persistSession{}
		return
	}
	var root persistRoot
	if err := json.Unmarshal(data, &root); err != nil {
		if a.personID == "" {
			a.personID = model.NewID().Hex()
		}
		if a.nextSeq <= 0 {
			a.nextSeq = 1
		}
		a.sessions = map[string]*persistSession{}
		return
	}
	if root.PersonID != "" {
		a.personID = root.PersonID
	} else if a.personID == "" {
		a.personID = model.NewID().Hex()
	}
	if root.ServerURL != "" {
		a.serverURL = root.ServerURL
	}
	a.lastArt = root.LastArt
	if root.Sessions == nil {
		root.Sessions = map[string]*persistSession{}
	}
	a.sessions = root.Sessions
	if a.nextSeq <= 0 {
		a.nextSeq = 1
	}
}

const msgSaveWarning = "暂时无法保存到本机，正在重试。请保持窗口打开。"
const saveRetryDelay = time.Second

// savePersistLocked 原子写盘。失败返回错误，调用方不得当成功。
func (a *App) savePersistLocked() error {
	if a.sessions == nil {
		a.sessions = map[string]*persistSession{}
	}
	if a.articleID != "" {
		a.stashSessionLocked()
		a.lastArt = a.articleID
	}
	root := persistRoot{
		PersonID:  a.personID,
		ServerURL: a.serverURL,
		LastArt:   a.lastArt,
		Sessions:  a.sessions,
	}
	data, err := json.MarshalIndent(root, "", "  ")
	if err != nil {
		return err
	}
	dir := filepath.Dir(a.persistPath())
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(dir, "state-*.tmp")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		_ = os.Remove(tmpName)
		return err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		_ = os.Remove(tmpName)
		return err
	}
	if err := tmp.Close(); err != nil {
		_ = os.Remove(tmpName)
		return err
	}
	if err := os.Rename(tmpName, a.persistPath()); err != nil {
		_ = os.Remove(tmpName)
		return err
	}
	return nil
}

// persistAfterMutationLocked 写盘；失败保留内存 queue/Doc，发 saveWarning 并后台重试。
func (a *App) persistAfterMutationLocked() {
	if err := a.savePersistLocked(); err != nil {
		a.notePersistFailLocked()
		return
	}
	a.clearSaveWarningLocked()
}

func (a *App) notePersistFailLocked() {
	a.saveWarning = msgSaveWarning
	a.emitSaveWarningLocked()
	a.scheduleSaveRetryLocked()
}

func (a *App) emitSaveWarningLocked() {
	if a.ctx == nil {
		return
	}
	runtime.EventsEmit(a.ctx, "saveWarning", a.saveWarning)
}

func (a *App) clearSaveWarningLocked() {
	if a.saveRetryTimer != nil {
		a.saveRetryTimer.Stop()
		a.saveRetryTimer = nil
	}
	if a.saveWarning == "" {
		return
	}
	a.saveWarning = ""
	a.emitSaveWarningLocked()
}

func (a *App) scheduleSaveRetryLocked() {
	if a.saveRetryTimer != nil {
		return
	}
	a.saveRetryTimer = time.AfterFunc(saveRetryDelay, func() {
		a.mu.Lock()
		defer a.mu.Unlock()
		a.saveRetryTimer = nil
		if err := a.savePersistLocked(); err != nil {
			a.scheduleSaveRetryLocked()
			return
		}
		a.clearSaveWarningLocked()
		// ack 写盘失败曾清 sent；恢复后无需新输入即可继续发待确认队列。
		if a.conn != nil && a.sentCount == 0 && len(a.queue) > 0 {
			a.forceFlushLocked()
		}
	})
}

func (a *App) stashSessionLocked() {
	if a.articleID == "" {
		return
	}
	if a.sessions == nil {
		a.sessions = map[string]*persistSession{}
	}
	q := append([]protocol.Op(nil), a.queue...)
	rej := append([]rejectedOp(nil), a.rejected...)
	var snap *protocol.Snapshot
	if a.serverSnap != nil {
		cp := *a.serverSnap
		snap = &cp
	}
	seq := a.nextSeq
	if seq <= 0 {
		seq = 1
	}
	sess := &persistSession{
		Name:     a.name,
		NextSeq:  seq,
		Queue:    q,
		Snapshot: snap,
		Rejected: rej,
	}
	if a.relayBooted && a.doc != nil {
		if v, err := a.doc.View(); err == nil {
			sess.Relay = &persistRelay{
				View:     v,
				Seen:     relaySeenList(a.relaySeen),
				ClaimIDs: claimIDsToHex(a.claimIDs),
				YourLine: a.yourLine,
			}
		}
	}
	a.sessions[a.articleID] = sess
}

func relaySeenList(m map[string]bool) []string {
	if len(m) == 0 {
		return nil
	}
	out := make([]string, 0, len(m))
	for id, ok := range m {
		if ok && id != "" {
			out = append(out, id)
		}
	}
	sort.Strings(out)
	return out
}

func relaySeenMap(ids []string) map[string]bool {
	if len(ids) == 0 {
		return nil
	}
	out := make(map[string]bool, len(ids))
	for _, id := range ids {
		if id != "" {
			out[id] = true
		}
	}
	return out
}

func claimIDsToHex(m map[string]model.ID) map[string]string {
	if len(m) == 0 {
		return nil
	}
	out := make(map[string]string, len(m))
	for k, id := range m {
		if id.IsZero() {
			continue
		}
		out[k] = id.Hex()
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

func claimIDsFromHex(m map[string]string) map[string]model.ID {
	if len(m) == 0 {
		return nil
	}
	out := make(map[string]model.ID, len(m))
	for k, hex := range m {
		id, err := model.ParseID(hex)
		if err != nil || id.IsZero() {
			continue
		}
		out[k] = id
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

func applySuspended(doc *document.Doc, suspended []string) {
	for _, id := range suspended {
		did, err := model.ParseID(id)
		if err != nil || did.IsZero() {
			continue
		}
		if d := findDispute(doc, did); d != nil {
			_ = doc.SetSuspended(d.Person, d.RealLine, d.Action, true)
		}
	}
}

func (a *App) restoreSessionLocked(articleID string) {
	sess := a.sessions[articleID]
	a.relayBooted = false
	a.relaySeen = nil
	a.claimIDs = nil
	a.ownSent = nil
	a.yourLine = ""
	if sess == nil {
		a.queue = nil
		a.rejected = nil
		a.serverSnap = nil
		a.nextSeq = 1
		a.sentCount = 0
		a.sentSeq = 0
		a.afterSeen = nil
		a.beforeSeen = nil
		a.lineBase = nil
		return
	}
	a.queue = append([]protocol.Op(nil), sess.Queue...)
	a.rejected = append([]rejectedOp(nil), sess.Rejected...)
	a.nextSeq = sess.NextSeq
	if a.nextSeq <= 0 {
		a.nextSeq = 1
	}
	a.sentCount = 0
	a.sentSeq = 0
	a.deferred = nil
	if sess.Name != "" && a.name == "" {
		a.name = sess.Name
	}
	a.serverSnap = nil
	a.doc = nil
	a.afterSeen = nil
	a.beforeSeen = nil
	a.lineBase = nil
	if sess.Snapshot != nil {
		cp := *sess.Snapshot
		a.serverSnap = &cp
	}
	if sess.Relay != nil {
		v := sess.Relay.View
		doc, err := document.Load(v.Article, v.Lines, v.Disputes)
		if err == nil {
			applySuspended(doc, v.Suspended)
			a.doc = doc
			a.relayBooted = true
			a.relaySeen = relaySeenMap(sess.Relay.Seen)
			a.claimIDs = claimIDsFromHex(sess.Relay.ClaimIDs)
			a.yourLine = sess.Relay.YourLine
			if a.serverSnap != nil {
				a.people = a.serverSnap.People
				a.cursors = a.serverSnap.Cursors
				a.rememberAfterSeenLocked(a.serverSnap.Lines)
			}
			// Relay View 已含乐观队列，勿 replayQueue。
		}
		if a.doc == nil {
			a.doc = document.New("")
		}
		return
	}
	if a.serverSnap != nil {
		cp := *a.serverSnap
		doc, err := document.Load(cp.Article, cp.Lines, cp.Disputes)
		if err == nil {
			applySuspended(doc, cp.Suspended)
			a.rememberAfterSeenLocked(cp.Lines)
			a.doc = doc
			a.people = cp.People
			a.cursors = cp.Cursors
			a.replayQueueLocked()
		}
	}
	if a.doc == nil {
		a.doc = document.New("")
	}
}

func opPlainText(op protocol.Op) string {
	if op.Submit != nil && len(op.Submit.Content) > 0 {
		return strings.Join(op.Submit.Content, "\n")
	}
	if op.SpanEdit != nil && len(op.SpanEdit.Replacement) > 0 {
		return strings.Join(op.SpanEdit.Replacement, "\n")
	}
	return ""
}

func (a *App) rememberServerSnapLocked(snap protocol.Snapshot) {
	cp := snap
	// 最近共享链缓存；本端正文以 Doc/Relay View 为准。
	a.serverSnap = &cp
}

func (a *App) emitUnsyncedLocked() {
	if a.ctx == nil {
		return
	}
	runtime.EventsEmit(a.ctx, "unsynced", a.unsyncedItemsLocked())
}

func rejectedSummary(_ rejectedOp) string {
	return "有一处修改未能同步到服务器，原文已保存在本地"
}

func (a *App) unsyncedItemsLocked() []UnsyncedItem {
	out := make([]UnsyncedItem, 0, len(a.rejected))
	for _, r := range a.rejected {
		id := r.ID
		if id == "" {
			id = r.Op.ID
		}
		text := opPlainText(r.Op)
		out = append(out, UnsyncedItem{
			ID:      id,
			Text:    text,
			Summary: rejectedSummary(r),
		})
	}
	return out
}

func (a *App) cachedArticleLocked() *CachedArticle {
	id := a.lastArt
	if id == "" {
		return nil
	}
	sess := a.sessions[id]
	if sess == nil {
		return nil
	}
	if len(sess.Queue) == 0 && len(sess.Rejected) == 0 && sess.Snapshot == nil && sess.Relay == nil {
		return nil
	}
	title := "文档"
	if sess.Snapshot != nil && sess.Snapshot.Article.Title != "" {
		title = sess.Snapshot.Article.Title
	} else if sess.Relay != nil && sess.Relay.View.Article.Title != "" {
		title = sess.Relay.View.Article.Title
	}
	return &CachedArticle{
		ID:            id,
		Title:         title,
		Name:          sess.Name,
		PendingCount:  len(sess.Queue),
		UnsyncedCount: len(sess.Rejected),
	}
}

func newRejected(op protocol.Op, msg string) rejectedOp {
	id := op.ID
	if id == "" {
		id = model.NewID().Hex()
	}
	return rejectedOp{
		ID:      id,
		Op:      op,
		Message: msg,
		At:      time.Now().UnixMilli(),
	}
}

// 本地领域拒绝时给前端的稳定文案；KNOWN 白名单同源。
const msgLocalSpanReject = "这次修改暂时无法应用，内容已保存在未同步修改中"

// localRejectKey 同目标草稿去重：跨度按 BaseIDs，普通提交按 LineID+Action。
func localRejectKey(op protocol.Op) string {
	if op.SpanEdit != nil {
		return "span\x00" + strings.Join(op.SpanEdit.BaseIDs, "\x00")
	}
	if op.Submit != nil {
		return "submit\x00" + op.Submit.LineID + "\x00" + op.Submit.Action
	}
	return ""
}

// spanRejectDraftOp 仅 rejected 草稿：起止 ID + Replacement，不入队不执行。
func spanRejectDraftOp(personID, startID, endID string, replacement []string) protocol.Op {
	return protocol.Op{
		Kind: protocol.TypeSpanEdit,
		SpanEdit: &protocol.SpanEdit{
			Type:        protocol.TypeSpanEdit,
			PersonID:    personID,
			BaseIDs:     []string{startID, endID},
			Replacement: append([]string(nil), replacement...),
			ClientTs:    time.Now().UnixMilli(),
		},
	}
}

// saveLocalSpanRejectLocked 写入 rejected；成功返回稳定自然文案。
func (a *App) saveLocalSpanRejectLocked(op protocol.Op) error {
	if err := a.storeLocalSpanRejectLocked(op); err != nil {
		return err
	}
	return fmt.Errorf("%s", msgLocalSpanReject)
}

// storeLocalSpanRejectLocked 正式 doc/队列不动；同目标覆盖旧草稿，避免连打堆叠。
func (a *App) storeLocalSpanRejectLocked(op protocol.Op) error {
	key := localRejectKey(op)
	updated := false
	if key != "" {
		for i := range a.rejected {
			if localRejectKey(a.rejected[i].Op) == key {
				id := a.rejected[i].ID
				r := newRejected(op, msgLocalSpanReject)
				if id != "" {
					r.ID = id
				}
				a.rejected[i] = r
				updated = true
				break
			}
		}
	}
	if !updated {
		a.rejected = append(a.rejected, newRejected(op, msgLocalSpanReject))
	}
	a.persistAfterMutationLocked()
	a.emitUnsyncedLocked()
	return nil
}

// upsertRejectedByOpIDLocked 按 OpID 覆盖未同步原文（本地重放失败 / 服务端拒绝）。
func (a *App) upsertRejectedByOpIDLocked(op protocol.Op, msg string) {
	if op.ID == "" {
		op.ID = model.NewID().Hex()
	}
	for i := range a.rejected {
		if a.rejected[i].Op.ID == op.ID || a.rejected[i].ID == op.ID {
			keep := a.rejected[i].ID
			r := newRejected(op, msg)
			if keep != "" {
				r.ID = keep
			}
			a.rejected[i] = r
			return
		}
	}
	a.rejected = append(a.rejected, newRejected(op, msg))
}

func (a *App) removeRejectedByOpIDLocked(opID string) bool {
	if opID == "" {
		return false
	}
	out := a.rejected[:0]
	removed := false
	for _, r := range a.rejected {
		if r.Op.ID == opID || r.ID == opID {
			removed = true
			continue
		}
		out = append(out, r)
	}
	a.rejected = out
	return removed
}
