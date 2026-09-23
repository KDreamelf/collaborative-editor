package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
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
	a.sessions[a.articleID] = &persistSession{
		Name:     a.name,
		NextSeq:  seq,
		Queue:    q,
		Snapshot: snap,
		Rejected: rej,
	}
}

func (a *App) restoreSessionLocked(articleID string) {
	sess := a.sessions[articleID]
	if sess == nil {
		a.queue = nil
		a.rejected = nil
		a.serverSnap = nil
		a.nextSeq = 1
		a.sentCount = 0
		a.sentSeq = 0
		a.afterSeen = nil
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
	a.lineBase = nil
	if sess.Snapshot != nil {
		cp := *sess.Snapshot
		a.serverSnap = &cp
		doc, err := document.Load(cp.Article, cp.Lines, cp.Disputes)
		if err == nil {
			for _, id := range cp.Suspended {
				did, err := model.ParseID(id)
				if err != nil || did.IsZero() {
					continue
				}
				if d := findDispute(doc, did); d != nil {
					_ = doc.SetSuspended(d.Person, d.RealLine, d.Action, true)
				}
			}
			a.rememberAfterSeenLocked(cp.Lines)
			a.doc = doc
			a.people = cp.People
			a.cursors = cp.Cursors
			for _, op := range a.queue {
				_ = a.applyOpLocked(op)
			}
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
	// 权威基线不含乐观重放；只存服务端原文。
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
	if len(sess.Queue) == 0 && len(sess.Rejected) == 0 && sess.Snapshot == nil {
		return nil
	}
	title := "文档"
	if sess.Snapshot != nil && sess.Snapshot.Article.Title != "" {
		title = sess.Snapshot.Article.Title
	}
	return &CachedArticle{
		ID:            id,
		Title:         title,
		Name:          sess.Name,
		PendingCount:  len(sess.Queue),
		UnsyncedCount: len(sess.Rejected),
	}
}

// ensurePersistOK 本地操作后写盘；失败则撤回刚入队的一步并报错。
func (a *App) ensurePersistAfterPushLocked() error {
	if err := a.savePersistLocked(); err != nil {
		if len(a.queue) > 0 {
			a.queue = a.queue[:len(a.queue)-1]
		}
		return fmt.Errorf("本地保存失败，请检查磁盘后重试")
	}
	return nil
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

// 本地领域拒绝跨度时给前端的稳定文案；KNOWN 白名单同源。
const msgLocalSpanReject = "这次修改暂时无法应用，内容已保存在未同步修改中"

func spanRejectBaseKey(op protocol.Op) string {
	if op.SpanEdit == nil {
		return ""
	}
	return strings.Join(op.SpanEdit.BaseIDs, "\x00")
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

// saveLocalSpanRejectLocked 写入 rejected；成功返回稳定自然文案，落盘失败回磁盘错误。
func (a *App) saveLocalSpanRejectLocked(op protocol.Op) error {
	if err := a.storeLocalSpanRejectLocked(op); err != nil {
		return err
	}
	return fmt.Errorf("%s", msgLocalSpanReject)
}

// storeLocalSpanRejectLocked 正式 doc/队列不动；同 BaseIDs 覆盖旧草稿，避免连打堆叠。
func (a *App) storeLocalSpanRejectLocked(op protocol.Op) error {
	oldRejected := append([]rejectedOp(nil), a.rejected...)
	key := spanRejectBaseKey(op)
	updated := false
	if key != "" {
		for i := range a.rejected {
			if spanRejectBaseKey(a.rejected[i].Op) == key {
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
	if err := a.savePersistLocked(); err != nil {
		a.rejected = oldRejected
		return fmt.Errorf("本地保存失败，请检查磁盘后重试")
	}
	a.emitUnsyncedLocked()
	return nil
}
