package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/KDreamelf/collaborative-editor/internal/document"
	"github.com/KDreamelf/collaborative-editor/internal/model"
	"github.com/KDreamelf/collaborative-editor/internal/protocol"
	"github.com/gorilla/websocket"
	"github.com/wailsapp/wails/v2/pkg/runtime"
)

var errDialBusy = errors.New("正在连接")

func (a *App) pushOpLocked(op protocol.Op) error {
	if op.ID == "" {
		op.ID = model.NewID().Hex()
	}
	now := time.Now()
	if len(a.queue) == a.sentCount {
		a.oldestAt = now
	}
	a.queue = append(a.queue, op)
	a.lastEnqAt = now
	a.persistAfterMutationLocked()
	a.armFlushLocked()
	return nil
}

func (a *App) armFlushLocked() {
	if a.flushTimer != nil {
		a.flushTimer.Stop()
	}
	delay := 100 * time.Millisecond
	if !a.oldestAt.IsZero() {
		until500 := time.Until(a.oldestAt.Add(500 * time.Millisecond))
		if until500 < delay {
			delay = until500
		}
	}
	if delay < 0 {
		delay = 0
	}
	a.flushTimer = time.AfterFunc(delay, func() {
		a.mu.Lock()
		defer a.mu.Unlock()
		a.flushLocked()
	})
}

func (a *App) flushLocked() {
	if a.sentCount > 0 {
		return
	}
	unsent := len(a.queue) - a.sentCount
	if unsent <= 0 {
		return
	}
	if a.conn == nil {
		return
	}
	now := time.Now()
	quiet := now.Sub(a.lastEnqAt) >= 100*time.Millisecond
	old := !a.oldestAt.IsZero() && now.Sub(a.oldestAt) >= 500*time.Millisecond
	// 控制消息强制 flush 时 quiet/old 都可能未到；调用方直接走这里时放行。
	if !quiet && !old {
		// 仍由定时器触发时才卡阈值；主动 flush（suspend 等）用 forceFlushLocked。
		return
	}
	a.sendBatchLocked()
}

func (a *App) forceFlushLocked() {
	if a.sentCount > 0 || a.conn == nil {
		return
	}
	if len(a.queue) == a.sentCount {
		return
	}
	a.sendBatchLocked()
}

func (a *App) sendAfterEditsLocked(msg any) error {
	if len(a.queue) > a.sentCount || a.sentCount > 0 {
		a.deferred = append(a.deferred, msg)
		if a.sentCount == 0 {
			a.sendBatchLocked()
		}
		return nil
	}
	if a.conn == nil {
		return nil
	}
	return a.writeLocked(msg)
}

func (a *App) drainDeferredLocked() {
	if a.sentCount > 0 {
		return
	}
	if len(a.queue) > 0 {
		a.sendBatchLocked()
		return
	}
	msgs := a.deferred
	a.deferred = nil
	for _, msg := range msgs {
		_ = a.writeLocked(msg)
	}
}

func (a *App) sendBatchLocked() {
	if a.conn == nil {
		return
	}
	ops := append([]protocol.Op(nil), a.queue[a.sentCount:]...)
	if len(ops) == 0 {
		return
	}
	seq := a.nextSeq
	a.nextSeq++
	a.sentSeq = seq
	a.sentCount = len(a.queue)
	msg := protocol.Batch{
		Type: protocol.TypeBatch,
		Seq:  seq,
		Ops:  ops,
	}
	if err := a.writeLocked(msg); err != nil {
		a.sentCount = 0
		a.sentSeq = 0
		a.nextSeq--
		return
	}
	if a.flushTimer != nil {
		a.flushTimer.Stop()
		a.flushTimer = nil
	}
}

func (a *App) writeLocked(v any) error {
	if a.conn == nil {
		return errors.New("未连接")
	}
	_ = a.conn.SetWriteDeadline(time.Now().Add(5 * time.Second))
	err := a.conn.WriteJSON(v)
	_ = a.conn.SetWriteDeadline(time.Time{})
	return err
}

func (a *App) connect() error {
	a.mu.Lock()
	if a.dialing {
		a.mu.Unlock()
		return errDialBusy
	}
	a.dialing = true
	if a.conn != nil {
		_ = a.conn.Close()
		a.conn = nil
	}
	a.connGen++
	myGen := a.connGen
	ws, err := a.wsURL()
	articleID := a.articleID
	personID := a.personID
	name := a.name
	handshakeTO := a.requestTimeout()
	a.mu.Unlock()
	if err != nil {
		a.mu.Lock()
		a.dialing = false
		a.mu.Unlock()
		return fmt.Errorf("无法连接服务器")
	}

	dialer := websocket.Dialer{HandshakeTimeout: handshakeTO}
	conn, _, err := dialer.Dial(ws, nil)
	a.mu.Lock()
	a.dialing = false
	if err != nil {
		a.mu.Unlock()
		return fmt.Errorf("无法连接服务器")
	}
	if a.connGen != myGen {
		a.mu.Unlock()
		_ = conn.Close()
		return fmt.Errorf("无法连接服务器")
	}
	a.conn = conn
	a.sentCount = 0
	a.sentSeq = 0
	join := protocol.Join{
		Type:      protocol.TypeJoin,
		ArticleID: articleID,
		PersonID:  personID,
		Name:      name,
	}
	err = a.writeLocked(join)
	if err != nil {
		a.conn = nil
		a.mu.Unlock()
		_ = conn.Close()
		return err
	}
	a.joined = true
	// 仅在收到有效 snapshot 后才 emit offline=false，避免假在线。
	a.mu.Unlock()
	go a.readLoop(conn, myGen)
	return nil
}

func (a *App) reconnectLater() {
	wait := a.reconnectWait
	if wait <= 0 {
		wait = time.Second
	}
	for {
		time.Sleep(wait)
		a.mu.Lock()
		if a.articleID == "" || !a.joined {
			a.mu.Unlock()
			return
		}
		if a.conn != nil {
			a.mu.Unlock()
			return
		}
		if a.dialing {
			a.mu.Unlock()
			continue
		}
		a.mu.Unlock()
		if err := a.connect(); err == nil {
			return
		}
	}
}

func (a *App) readLoop(conn *websocket.Conn, gen uint64) {
	defer func() {
		a.mu.Lock()
		mine := a.conn == conn && a.connGen == gen
		if mine {
			a.conn = nil
			a.sentCount = 0
			a.sentSeq = 0
			a.emitOfflineLocked(true)
		}
		articleID := a.articleID
		joined := a.joined
		a.mu.Unlock()
		_ = conn.Close()
		if mine && articleID != "" && joined {
			go a.reconnectLater()
		}
	}()

	for {
		_, data, err := conn.ReadMessage()
		if err != nil {
			return
		}
		a.handleMessage(data, gen)
	}
}

func (a *App) handleMessage(data []byte, gen uint64) {
	var head struct {
		Type string `json:"type"`
	}
	if err := json.Unmarshal(data, &head); err != nil {
		return
	}
	switch head.Type {
	case protocol.TypeSnapshot:
		var snap protocol.Snapshot
		if err := json.Unmarshal(data, &snap); err != nil {
			return
		}
		a.onSnapshot(snap, gen)
	case protocol.TypeCursors:
		var msg struct {
			Cursors []protocol.Cursor `json:"cursors"`
		}
		if err := json.Unmarshal(data, &msg); err != nil {
			return
		}
		a.mu.Lock()
		if a.connGen != gen {
			a.mu.Unlock()
			return
		}
		a.cursors = msg.Cursors
		a.mu.Unlock()
		if a.ctx != nil {
			runtime.EventsEmit(a.ctx, "cursors", msg.Cursors)
		}
	case protocol.TypeFollowAsk:
		var ask protocol.FollowAsk
		if err := json.Unmarshal(data, &ask); err != nil {
			return
		}
		a.mu.Lock()
		ok := a.connGen == gen
		a.mu.Unlock()
		if !ok {
			return
		}
		if a.ctx != nil {
			runtime.EventsEmit(a.ctx, "followAsk", ask)
		}
	case protocol.TypeFollowResult:
		var res protocol.FollowResult
		if err := json.Unmarshal(data, &res); err != nil {
			return
		}
		a.mu.Lock()
		ok := a.connGen == gen
		a.mu.Unlock()
		if !ok {
			return
		}
		if a.ctx != nil {
			runtime.EventsEmit(a.ctx, "followResult", res)
		}
	case protocol.TypeAck:
		var ack protocol.Ack
		if err := json.Unmarshal(data, &ack); err != nil {
			return
		}
		a.onAck(ack, gen)
	case protocol.TypeError:
		var em protocol.ErrMsg
		if err := json.Unmarshal(data, &em); err != nil {
			return
		}
		a.mu.Lock()
		ok := a.connGen == gen
		a.mu.Unlock()
		if !ok {
			return
		}
		if a.ctx != nil {
			runtime.EventsEmit(a.ctx, "error", em.Message)
		}
	}
}

func (a *App) onAck(ack protocol.Ack, gen uint64) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.connGen != gen {
		return
	}
	if ack.Seq != a.sentSeq || a.sentCount == 0 {
		return
	}
	// Applied 只覆盖本批 in-flight；负数 panic，>sentCount 会误丢后续未发送条目。
	if ack.Applied < 0 || ack.Applied > a.sentCount {
		return
	}
	oldQueue := append([]protocol.Op(nil), a.queue...)
	oldRejected := append([]rejectedOp(nil), a.rejected...)

	appliedN := ack.Applied
	applied := append([]protocol.Op(nil), a.queue[:appliedN]...)
	drop := appliedN
	rejectedNow := false
	var failed *protocol.Op
	// Message 只对应已发送但未 Applied 的那一笔；Applied==sentCount 时无失败槽，忽略孤儿 Message，勿误拒后续未发送项。
	if ack.Message != "" && drop < a.sentCount {
		f := a.queue[drop]
		failed = &f
		drop++
		rejectedNow = true
	}
	a.queue = append([]protocol.Op(nil), a.queue[drop:]...)
	// 成功送达的 Op：清掉仅因本地重放失败留下的误判草稿。
	for _, op := range applied {
		_ = a.removeRejectedByOpIDLocked(op.ID)
	}
	if failed != nil {
		a.upsertRejectedByOpIDLocked(*failed, ack.Message)
	}
	a.sentCount = 0
	a.sentSeq = 0
	if len(a.queue) > 0 {
		a.oldestAt = time.Now()
		a.lastEnqAt = a.oldestAt
	} else {
		a.oldestAt = time.Time{}
	}
	if err := a.savePersistLocked(); err != nil {
		// 回退队列/拒绝列表；清 sent。保留内存原文，saveWarning 重试，不诱发前端回滚。
		a.queue = oldQueue
		a.rejected = oldRejected
		a.sentCount = 0
		a.sentSeq = 0
		a.oldestAt = time.Now()
		a.lastEnqAt = a.oldestAt
		a.notePersistFailLocked()
		return
	}
	a.clearSaveWarningLocked()
	if rejectedNow {
		a.emitUnsyncedLocked()
		if a.ctx != nil {
			runtime.EventsEmit(a.ctx, "error", "有一处修改未能同步到服务器，原文已保存在本地")
		}
	} else if len(applied) > 0 {
		a.emitUnsyncedLocked()
	}
	if len(a.deferred) > 0 {
		a.drainDeferredLocked()
		return
	}
	if len(a.queue) > 0 {
		a.armFlushLocked()
	}
}

func (a *App) onSnapshot(snap protocol.Snapshot, gen uint64) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.connGen != gen {
		return
	}
	doc, err := document.Load(snap.Article, snap.Lines, snap.Disputes)
	if err != nil {
		if a.ctx != nil {
			runtime.EventsEmit(a.ctx, "error", err.Error())
		}
		return
	}
	for _, id := range snap.Suspended {
		did, err := model.ParseID(id)
		if err != nil || did.IsZero() {
			continue
		}
		if d := findDispute(doc, did); d != nil {
			_ = doc.SetSuspended(d.Person, d.RealLine, d.Action, true)
		}
	}
	// 基准后继只来自服务端快照；先记再重放乐观队列，避免污染。
	a.rememberServerSnapLocked(snap)
	a.rememberAfterSeenLocked(snap.Lines)
	a.doc = doc
	a.people = snap.People
	a.cursors = snap.Cursors
	a.replayQueueLocked()
	a.persistAfterMutationLocked()
	a.emitOfflineLocked(false)
	a.emitSnapshotLocked()
	if a.ctx != nil {
		runtime.EventsEmit(a.ctx, "cursors", a.cursors)
	}
	// 断线重连后重发未确认队列
	if a.conn != nil && a.sentCount == 0 && len(a.queue) > 0 {
		a.forceFlushLocked()
	}
}

// replayQueueLocked 重放待送 Op。本地失败只留可复制草稿（按 OpID 去重），不剥队列；
// 送达以服务端 ack 为准，避免 sentCount 与队列错位。
func (a *App) replayQueueLocked() {
	if len(a.queue) == 0 {
		return
	}
	changed := false
	for _, op := range a.queue {
		if err := a.applyOpLocked(op); err != nil {
			a.upsertRejectedByOpIDLocked(op, "本地无法重放该修改，原文已保存在未同步修改中")
			changed = true
			continue
		}
		if a.removeRejectedByOpIDLocked(op.ID) {
			changed = true
		}
	}
	if changed {
		a.emitUnsyncedLocked()
	}
}

func findDispute(doc *document.Doc, id model.ID) *model.Dispute {
	v, err := doc.View()
	if err != nil {
		return nil
	}
	for i := range v.Disputes {
		if v.Disputes[i].ID == id {
			cp := v.Disputes[i]
			return &cp
		}
	}
	return nil
}

func (a *App) applyOpLocked(op protocol.Op) error {
	switch op.Kind {
	case protocol.TypeSubmit:
		s := op.Submit
		if s == nil {
			return nil
		}
		id, err := model.ParseID(s.LineID)
		if err != nil {
			return err
		}
		opts := document.SubmitOpts{WholeClaim: s.WholeClaim}
		if s.AfterSeen != nil {
			seen, err := model.ParseID(*s.AfterSeen)
			if err != nil {
				return err
			}
			opts.AfterSeen = &seen
		}
		if s.BeforeSeen != nil {
			seen, err := model.ParseID(*s.BeforeSeen)
			if err != nil {
				return err
			}
			opts.BeforeSeen = &seen
		}
		if s.BaseContent != nil {
			cp := *s.BaseContent
			opts.BaseContent = &cp
		}
		if len(s.LineIDs) > 0 {
			opts.LineIDs = make([]model.ID, len(s.LineIDs))
			for i, raw := range s.LineIDs {
				lid, err := model.ParseID(raw)
				if err != nil {
					return err
				}
				opts.LineIDs[i] = lid
			}
		}
		return a.doc.SubmitWith(s.PersonID, id, s.Action, append([]string(nil), s.Content...), opts)
	case protocol.TypeSpanEdit:
		opts, err := protocol.ParseSpanEditOpts(op.SpanEdit)
		if err != nil {
			return err
		}
		return a.doc.SubmitSpanEdit(op.SpanEdit.PersonID, opts)
	case protocol.TypeDelete:
		d := op.Delete
		if d == nil {
			return nil
		}
		id, err := model.ParseID(d.LineID)
		if err != nil {
			return err
		}
		return a.doc.DeleteIfIdle(d.PersonID, id, a.holdersLocked(d.LineID))
	case protocol.TypeMerge:
		m := op.Merge
		if m == nil {
			return nil
		}
		id, err := model.ParseID(m.LineID)
		if err != nil {
			return err
		}
		return a.doc.MergeUp(m.PersonID, id, a.holdersLocked(m.LineID))
	case protocol.TypeFollow:
		f := op.Follow
		if f == nil {
			return nil
		}
		id, err := model.ParseID(f.DisputeID)
		if err != nil {
			return err
		}
		_, err = a.doc.RequestFollow(f.PersonID, id, f.ClientTs)
		return err
	case protocol.TypeFollowAnswer:
		ans := op.FollowAnswer
		if ans == nil {
			return nil
		}
		id, err := model.ParseID(ans.DisputeID)
		if err != nil {
			return err
		}
		_, err = a.doc.AnswerFollow(ans.PersonID, ans.FromID, id, ans.Accept)
		return err
	case protocol.TypeSuspend:
		s := op.Suspend
		if s == nil {
			return nil
		}
		id, err := model.ParseID(s.LineID)
		if err != nil {
			return err
		}
		return a.doc.SetSuspended(s.PersonID, id, s.Action, s.Suspended)
	default:
		return nil
	}
}

func (a *App) emitSnapshotLocked() {
	if a.doc == nil || a.ctx == nil {
		return
	}
	view, err := a.doc.View()
	if err != nil {
		runtime.EventsEmit(a.ctx, "error", err.Error())
		return
	}
	snap := protocol.Snapshot{
		Type:    protocol.TypeSnapshot,
		View:    view,
		People:  a.people,
		Cursors: a.cursors,
	}
	runtime.EventsEmit(a.ctx, "snapshot", snap)
}
