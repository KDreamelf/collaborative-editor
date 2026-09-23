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
	if err := a.ensurePersistAfterPushLocked(); err != nil {
		return err
	}
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
	ws, err := a.wsURL()
	articleID := a.articleID
	personID := a.personID
	name := a.name
	a.mu.Unlock()
	if err != nil {
		a.mu.Lock()
		a.dialing = false
		a.mu.Unlock()
		return fmt.Errorf("无法连接服务器")
	}

	conn, _, err := websocket.DefaultDialer.Dial(ws, nil)
	a.mu.Lock()
	a.dialing = false
	if err != nil {
		a.mu.Unlock()
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
	a.emitOfflineLocked(false)
	a.mu.Unlock()
	go a.readLoop(conn)
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

func (a *App) readLoop(conn *websocket.Conn) {
	defer func() {
		a.mu.Lock()
		if a.conn == conn {
			a.conn = nil
			a.sentCount = 0
			a.sentSeq = 0
		}
		articleID := a.articleID
		joined := a.joined
		a.mu.Unlock()
		_ = conn.Close()
		if articleID != "" && joined {
			go a.reconnectLater()
		}
	}()

	for {
		_, data, err := conn.ReadMessage()
		if err != nil {
			return
		}
		a.handleMessage(data)
	}
}

func (a *App) handleMessage(data []byte) {
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
		a.onSnapshot(snap)
	case protocol.TypeCursors:
		var msg struct {
			Cursors []protocol.Cursor `json:"cursors"`
		}
		if err := json.Unmarshal(data, &msg); err != nil {
			return
		}
		a.mu.Lock()
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
		if a.ctx != nil {
			runtime.EventsEmit(a.ctx, "followAsk", ask)
		}
	case protocol.TypeFollowResult:
		var res protocol.FollowResult
		if err := json.Unmarshal(data, &res); err != nil {
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
		a.onAck(ack)
	case protocol.TypeError:
		var em protocol.ErrMsg
		if err := json.Unmarshal(data, &em); err != nil {
			return
		}
		if a.ctx != nil {
			runtime.EventsEmit(a.ctx, "error", em.Message)
		}
	}
}

func (a *App) onAck(ack protocol.Ack) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if ack.Seq != a.sentSeq || a.sentCount == 0 {
		return
	}
	oldQueue := append([]protocol.Op(nil), a.queue...)
	oldRejected := append([]rejectedOp(nil), a.rejected...)

	drop := ack.Applied
	rejectedNow := false
	if ack.Message != "" && drop < len(a.queue) {
		failed := a.queue[drop]
		a.rejected = append(a.rejected, newRejected(failed, ack.Message))
		drop++
		rejectedNow = true
	}
	if drop > len(a.queue) {
		drop = len(a.queue)
	}
	a.queue = append([]protocol.Op(nil), a.queue[drop:]...)
	a.sentCount = 0
	a.sentSeq = 0
	if len(a.queue) > 0 {
		a.oldestAt = time.Now()
		a.lastEnqAt = a.oldestAt
	} else {
		a.oldestAt = time.Time{}
	}
	if err := a.savePersistLocked(); err != nil {
		// 回退队列/拒绝列表；清 sent（本次 ack 已消费）。不立刻重发，等重连或后续入队。
		a.queue = oldQueue
		a.rejected = oldRejected
		a.sentCount = 0
		a.sentSeq = 0
		a.oldestAt = time.Now()
		a.lastEnqAt = a.oldestAt
		if a.ctx != nil {
			runtime.EventsEmit(a.ctx, "error", "本地保存失败，请检查磁盘后重试")
		}
		return
	}
	if rejectedNow {
		a.emitUnsyncedLocked()
		if a.ctx != nil {
			runtime.EventsEmit(a.ctx, "error", "有一处修改未能同步到服务器，原文已保存在本地")
		}
	}
	if len(a.deferred) > 0 {
		a.drainDeferredLocked()
		return
	}
	if len(a.queue) > 0 {
		a.armFlushLocked()
	}
}

func (a *App) onSnapshot(snap protocol.Snapshot) {
	a.mu.Lock()
	defer a.mu.Unlock()
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
	for _, op := range a.queue {
		_ = a.applyOpLocked(op)
	}
	_ = a.savePersistLocked()
	a.emitSnapshotLocked()
	if a.ctx != nil {
		runtime.EventsEmit(a.ctx, "cursors", a.cursors)
	}
	// 断线重连后重发未确认队列
	if a.conn != nil && a.sentCount == 0 && len(a.queue) > 0 {
		a.forceFlushLocked()
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
		opts := document.SubmitOpts{}
		if s.AfterSeen != nil {
			seen, err := model.ParseID(*s.AfterSeen)
			if err != nil {
				return err
			}
			opts.AfterSeen = &seen
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
