package main

import (
	"crypto/rand"
	"errors"
	"fmt"
	"time"

	"github.com/KDreamelf/collaborative-editor/internal/document"
	"github.com/KDreamelf/collaborative-editor/internal/model"
	"github.com/KDreamelf/collaborative-editor/internal/protocol"
	"github.com/wailsapp/wails/v2/pkg/runtime"
)

// newClientLogID 时间戳类日志编号，例如 E20260324-143052-a3f，供 ReportError / UI 检索。
func newClientLogID() string {
	stamp := time.Now().Format("20060102-150405")
	var b [2]byte
	_, _ = rand.Read(b[:])
	v := int(b[0])<<8 | int(b[1])
	const digits = "0123456789abcdefghijklmnopqrstuvwxyz"
	var r [3]byte
	for i := 2; i >= 0; i-- {
		r[i] = digits[v%36]
		v /= 36
	}
	return fmt.Sprintf("E%s-%s", stamp, string(r[:]))
}

func (a *App) markRelaySeenLocked(opID string) {
	if opID == "" {
		return
	}
	if a.relaySeen == nil {
		a.relaySeen = map[string]bool{}
	}
	a.relaySeen[opID] = true
}

func (a *App) relaySeenLocked(opID string) bool {
	return opID != "" && a.relaySeen != nil && a.relaySeen[opID]
}

// claimIDSlotAction：Edit/Delete 同 body 槽（规范键 ActionEdit）；插入方向各自独立。
func claimIDSlotAction(action string) string {
	if action == model.ActionEdit || action == model.ActionDelete {
		return model.ActionEdit
	}
	if model.IsInsertAction(action) {
		return model.InsertAction(action)
	}
	return action
}

// stableClaimIDLocked 本人在该锚行+槽位的稳定主张 ID。
func (a *App) stableClaimIDLocked(lineHex, action string) model.ID {
	if a.claimIDs == nil {
		a.claimIDs = map[string]model.ID{}
	}
	slot := claimIDSlotAction(action)
	key := lineHex + "\x00" + slot
	if id, ok := a.claimIDs[key]; ok && !id.IsZero() {
		return id
	}
	// 最小兼容：旧 map 可能用 ActionDelete 键存 body 槽。
	if slot == model.ActionEdit {
		oldKey := lineHex + "\x00" + model.ActionDelete
		if id, ok := a.claimIDs[oldKey]; ok && !id.IsZero() {
			a.claimIDs[key] = id
			delete(a.claimIDs, oldKey)
			return id
		}
	}
	id := model.NewID()
	a.claimIDs[key] = id
	return id
}

func (a *App) rememberClaimIDLocked(lineHex, action string, id model.ID) {
	if lineHex == "" || action == "" || id.IsZero() {
		return
	}
	if a.claimIDs == nil {
		a.claimIDs = map[string]model.ID{}
	}
	slot := claimIDSlotAction(action)
	key := lineHex + "\x00" + slot
	a.claimIDs[key] = id
	if slot == model.ActionEdit {
		delete(a.claimIDs, lineHex+"\x00"+model.ActionDelete)
	}
}

func (a *App) onBootstrap(boot protocol.Bootstrap, gen uint64) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.connGen != gen {
		return
	}

	oldDoc := a.doc
	newDoc, err := rebuildFromChainAndClaims(a.personID, oldDoc, boot, a.claimIDs)
	if err != nil {
		// 失败：保留 doc/queue/preboot；不 ready、不 flush、不改可见状态、不落盘。
		id := newClientLogID()
		a.ReportError(id, err.Error())
		a.relayReady = false
		a.emitOfflineLocked(true)
		if a.ctx != nil {
			runtime.EventsEmit(a.ctx, "error", "本地内容已保留，正在重连，日志编号 "+id)
		}
		// 关 conn，让既有 readLoop defer → reconnectLater；勿自建第二套重连。
		if conn := a.conn; conn != nil {
			go func() { _ = conn.Close() }()
		}
		return
	}

	a.yourLine = boot.YourLine
	a.people = append([]protocol.Person(nil), boot.People...)
	a.cursors = append([]protocol.Cursor(nil), boot.Cursors...)
	// 重连必须失活；首次加入同置 false。
	a.attentionActive = false
	a.attending = nil

	// serverSnap 只缓存最近共享链，不再当本端唯一正文权威。
	a.rememberServerSnapLocked(protocol.Snapshot{
		Type:     protocol.TypeSnapshot,
		YourLine: boot.YourLine,
		View:     boot.Base,
		People:   a.people,
		Cursors:  a.cursors,
	})
	a.rememberAfterSeenLocked(boot.Base.Lines)

	a.doc = newDoc
	a.ownSent = map[string]model.Dispute{}
	for _, claim := range boot.Disputes {
		if claim.Person == a.personID {
			a.ownSent[claimSlotKey(claim.RealLine, claim.Action)] = claim
		}
	}
	for _, op := range a.queue {
		if op.Kind == protocol.TypeDisputeCC && op.DisputeCC != nil && op.DisputeCC.Claim.Person == a.personID {
			claim := op.DisputeCC.Claim
			a.ownSent[claimSlotKey(claim.RealLine, claim.Action)] = claim
		}
	}
	a.rememberOwnClaimIDsFromDocLocked()
	// 未 ACK 普通 Op：无 foreign 候选则 ApplyPlain* 保本端乐观结果；有 foreign 则纯函数已留本人内容。
	a.replayUnackedPlainLocked()

	a.relayBooted = true
	// 只处理入场前实际到达的在线转发。
	buffered := a.preboot
	a.preboot = nil
	for _, ev := range buffered {
		if err := a.applyRelayOpLocked(ev.Op); err != nil && a.ctx != nil {
			runtime.EventsEmit(a.ctx, "error", err.Error())
		}
	}
	for _, op := range a.queue {
		a.markRelaySeenLocked(op.ID)
	}

	a.relayReady = true
	a.persistAfterMutationLocked()
	a.emitOfflineLocked(false)
	a.emitSnapshotLocked()
	if a.ctx != nil {
		runtime.EventsEmit(a.ctx, "cursors", a.cursors)
	}
	if a.conn != nil && a.sentCount == 0 && len(a.queue) > 0 {
		a.forceFlushLocked()
	}
}

func (a *App) rememberOwnClaimIDsFromDocLocked() {
	if a.doc == nil || a.personID == "" {
		return
	}
	v, err := a.doc.View()
	if err != nil {
		return
	}
	for _, d := range v.Disputes {
		if d.Person != a.personID || d.ID.IsZero() {
			continue
		}
		a.rememberClaimIDLocked(d.RealLine.Hex(), d.Action, d.ID)
	}
}

func (a *App) onRelay(ev protocol.RelayEvent, gen uint64) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.connGen != gen {
		return
	}
	if !a.relayReady {
		a.preboot = append(a.preboot, ev)
		return
	}
	if err := a.applyRelayOpLocked(ev.Op); err != nil {
		if a.ctx != nil {
			runtime.EventsEmit(a.ctx, "error", err.Error())
		}
		return
	}
}

func (a *App) onCursor(cur protocol.Cursor, gen uint64) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.connGen != gen {
		return
	}
	found := false
	for i := range a.cursors {
		if a.cursors[i].PersonID == cur.PersonID {
			a.cursors[i] = cur
			found = true
			break
		}
	}
	if !found {
		a.cursors = append(a.cursors, cur)
	}
	if a.ctx != nil {
		runtime.EventsEmit(a.ctx, "cursors", a.cursors)
	}
}

// applyRelayOpLocked 收普通操作与显式主张；只有收到他人主张才让本端进争议。
func (a *App) applyRelayOpLocked(op protocol.Op) error {
	if a.doc == nil {
		return fmt.Errorf("尚未加入文档")
	}
	if a.relaySeenLocked(op.ID) {
		return nil
	}

	switch {
	case op.Kind == protocol.TypeSubmit && op.Submit != nil && op.Submit.Action == model.ActionEdit:
		return a.applyRelayEditLocked(op)
	case op.Kind == protocol.TypeSubmit && op.Submit != nil && model.IsInsertAction(op.Submit.Action):
		return a.applyRelayInsertLocked(op)
	case op.Kind == protocol.TypeDelete || op.Kind == protocol.TypeMerge || op.Kind == protocol.TypeSpanEdit:
		cc, err := a.structuralOwnCCLocked(op)
		if err != nil {
			return err
		}
		if cc != nil {
			a.markRelaySeenLocked(op.ID)
			if err := a.pushOpLocked(protocol.Op{Kind: protocol.TypeDisputeCC, DisputeCC: cc}); err != nil {
				return err
			}
			a.forceFlushLocked()
			return nil
		}
		if err := a.applyPlainOpLocked(op); err != nil {
			return err
		}
		a.markRelaySeenLocked(op.ID)
		a.persistAfterMutationLocked()
		a.emitSnapshotLocked()
		return nil
	case op.Kind == protocol.TypeSuspend && op.Suspend != nil:
		id, err := model.ParseID(op.Suspend.LineID)
		if err != nil {
			return err
		}
		if err := a.doc.ApplyPlainSuspend(op.Suspend.PersonID, id, op.Suspend.Action, op.Suspend.Suspended); err != nil {
			return err
		}
		a.markRelaySeenLocked(op.ID)
		a.persistAfterMutationLocked()
		a.emitSnapshotLocked()
		return nil
	case op.Kind == protocol.TypeDisputeCC && op.DisputeCC != nil:
		return a.applyRelayDisputeCCLocked(op)
	case op.Kind == protocol.TypeFollow && op.Follow != nil:
		return a.applyRelayFollowLocked(op)
	case op.Kind == protocol.TypeFollowAnswer && op.FollowAnswer != nil:
		return a.applyRelayFollowAnswerLocked(op)
	default:
		// 暂未接线的 Action：可诊断，不标记已应用。
		return fmt.Errorf("在线转发暂未处理: %s", op.Kind)
	}
}

func (a *App) isOwnEditClaimLocked(id model.ID) bool {
	if id.IsZero() || a.claimIDs == nil {
		return false
	}
	for _, v := range a.claimIDs {
		if v == id {
			return true
		}
	}
	return false
}

func (a *App) emitFollowResultLocked(status, disputeID string) {
	if a.ctx == nil || status == "" {
		return
	}
	runtime.EventsEmit(a.ctx, "followResult", protocol.FollowResult{
		Type:      protocol.TypeFollowResult,
		Status:    status,
		DisputeID: disputeID,
	})
}

func (a *App) enqueueFollowAnswerLocked(fromID, disputeID string, accept bool) error {
	return a.pushOpLocked(protocol.Op{
		Kind: protocol.TypeFollowAnswer,
		FollowAnswer: &protocol.FollowAnswer{
			Type:      protocol.TypeFollowAnswer,
			PersonID:  a.personID,
			FromID:    fromID,
			DisputeID: disputeID,
			Accept:    accept,
		},
	})
}

// applyRelayFollowLocked：他人追随本端主张 → 立即 Accept；互追已决胜则不再答复。
// 单向 CC：本端无争议但 target 是已发稳定 claimID → 仍 Accept（正文已是目标）。
func (a *App) applyRelayFollowLocked(op protocol.Op) error {
	f := op.Follow
	if f.PersonID == a.personID {
		a.markRelaySeenLocked(op.ID)
		a.persistAfterMutationLocked()
		return nil
	}
	id, err := model.ParseID(f.DisputeID)
	if err != nil {
		return err
	}
	lineHex := a.disputeRealLineHexLocked(id)
	out, err := a.doc.RequestFollow(f.PersonID, id, f.ClientTs)
	if err != nil {
		if !errors.Is(err, document.ErrNoDispute) {
			return err
		}
		// 未知目标拒绝；本人已发稳定主张则无争议也 Accept。
		accept := a.isOwnEditClaimLocked(id)
		a.markRelaySeenLocked(op.ID)
		if err := a.enqueueFollowAnswerLocked(f.PersonID, f.DisputeID, accept); err != nil {
			return err
		}
		a.forceFlushLocked()
		return nil
	}
	a.markRelaySeenLocked(op.ID)
	switch out.Status {
	case document.FollowPending:
		if _, err := a.doc.AnswerFollow(a.personID, f.PersonID, id, true); err != nil {
			return err
		}
		if err := a.enqueueFollowAnswerLocked(f.PersonID, f.DisputeID, true); err != nil {
			return err
		}
		a.forceFlushLocked()
		a.persistAfterMutationLocked()
		a.emitSnapshotLocked()
		return nil
	case document.FollowApplied, document.FollowLost:
		// PeerStatus 非空：互追已按 ClientTs 决胜，禁止再 Answer。
		if out.PeerStatus != "" {
			// 本端是真正跟随方（较早胜者）：放下该行注意力。
			if out.Status == document.FollowLost && out.PeerStatus == document.FollowApplied {
				a.deactivateAttentionIfOnLineLocked(lineHex)
			}
			a.persistAfterMutationLocked()
			a.emitSnapshotLocked()
			a.emitFollowResultLocked(out.PeerStatus, f.DisputeID)
			return nil
		}
		// FollowApplied 且无 PeerStatus：已追随者换新 Op 重发，须再答 Accept。
		if out.Status == document.FollowApplied {
			if err := a.enqueueFollowAnswerLocked(f.PersonID, f.DisputeID, true); err != nil {
				return err
			}
			a.forceFlushLocked()
		}
		a.persistAfterMutationLocked()
		a.emitSnapshotLocked()
		return nil
	default:
		a.persistAfterMutationLocked()
		return nil
	}
}

func (a *App) applyRelayFollowAnswerLocked(op protocol.Op) error {
	ans := op.FollowAnswer
	if ans.PersonID == a.personID {
		a.markRelaySeenLocked(op.ID)
		a.persistAfterMutationLocked()
		return nil
	}
	if ans.FromID != a.personID {
		a.markRelaySeenLocked(op.ID)
		a.persistAfterMutationLocked()
		return nil
	}
	id, err := model.ParseID(ans.DisputeID)
	if err != nil {
		return err
	}
	lineHex := a.disputeRealLineHexLocked(id)
	slotLine, slotAction, slotOK := a.disputeSlotLocked(id)
	wasIdle := !a.attentionActive
	out, err := a.doc.AnswerFollow(ans.PersonID, ans.FromID, id, ans.Accept)
	if err != nil {
		if errors.Is(err, document.ErrNoDispute) {
			a.markRelaySeenLocked(op.ID)
			return nil
		}
		return err
	}
	// 本端追随确认 applied：放下目标主张 RealLine 上的注意力，避免下一笔普通 Edit 再发 CC。
	// 停笔后的同文收束也在这里丢掉旧 ownSent，避免下次再把旧主张外发。
	if ans.Accept && out.Status == document.FollowApplied {
		if wasIdle && slotOK {
			delete(a.ownSent, claimSlotKey(slotLine, slotAction))
		}
		a.deactivateAttentionIfOnLineLocked(lineHex)
	}
	a.markRelaySeenLocked(op.ID)
	a.persistAfterMutationLocked()
	a.emitSnapshotLocked()
	a.emitFollowResultLocked(out.Status, ans.DisputeID)
	return nil
}

func (a *App) applyRelayEditLocked(op protocol.Op) error {
	sub := op.Submit
	if sub.PersonID == a.personID {
		// 本人普通 Edit 回声：正式链用 ApplyPlain（幂等），不走 SubmitWith 争议分支。
		if err := a.applyPlainOpLocked(op); err != nil {
			return err
		}
		a.markRelaySeenLocked(op.ID)
		a.persistAfterMutationLocked()
		a.emitSnapshotLocked()
		return nil
	}

	claimID := a.stableClaimIDLocked(sub.LineID, model.ActionEdit)
	before := a.lineContentLocked(sub.LineID)
	cc, err := receiveOrdinaryEdit(a.doc, a.personID, sub.LineID, a.lineAttendingLocked(sub.LineID), op, claimID)
	if err != nil {
		return err
	}
	a.markRelaySeenLocked(op.ID)
	if cc != nil {
		ccOp := protocol.Op{
			Kind:      protocol.TypeDisputeCC,
			DisputeCC: cc,
		}
		if err := a.pushOpLocked(ccOp); err != nil {
			return err
		}
		a.forceFlushLocked()
		// 还在写这一行：不收对方正文。
		return nil
	}
	if a.lineContentLocked(sub.LineID) != before {
		delete(a.localText, sub.LineID)
	}
	a.persistAfterMutationLocked()
	a.emitSnapshotLocked()
	return nil
}

func (a *App) lineContentLocked(lineHex string) string {
	if a.doc == nil {
		return ""
	}
	v, err := a.doc.View()
	if err != nil {
		return ""
	}
	for _, ln := range v.Lines {
		if ln.ID.Hex() == lineHex {
			return ln.Content
		}
	}
	return ""
}

func (a *App) applyRelayInsertLocked(op protocol.Op) error {
	sub := op.Submit
	if sub.PersonID == a.personID {
		// Bootstrap 重放本人普通 Insert：无本地 View 时按历史普通插入恢复，不造争议。
		if err := a.applyOpLocked(op); err != nil {
			return err
		}
		a.markRelaySeenLocked(op.ID)
		a.persistAfterMutationLocked()
		a.emitSnapshotLocked()
		return nil
	}

	claimID := a.stableClaimIDLocked(sub.LineID, sub.Action)
	cc, err := receiveOrdinaryInsert(a.doc, a.personID, a.attentionLine, a.attentionActive, op, claimID)
	if err != nil {
		return err
	}
	a.markRelaySeenLocked(op.ID)
	if cc != nil {
		ccOp := protocol.Op{
			Kind:      protocol.TypeDisputeCC,
			DisputeCC: cc,
		}
		if err := a.pushOpLocked(ccOp); err != nil {
			return err
		}
		a.forceFlushLocked()
		// 本地不显示争议、不改正式链；pushOp 已落盘 queue+seen+本端 View。
		return nil
	}
	a.persistAfterMutationLocked()
	a.emitSnapshotLocked()
	return nil
}

// disputeSlotLocked 在 Answer 删掉争议前记下槽位，供确认后清 ownSent。
func (a *App) disputeSlotLocked(id model.ID) (model.ID, string, bool) {
	if a.doc == nil || id.IsZero() {
		return model.ID{}, "", false
	}
	v, err := a.doc.View()
	if err != nil {
		return model.ID{}, "", false
	}
	for _, d := range v.Disputes {
		if d.ID == id {
			return d.RealLine, d.Action, true
		}
	}
	return model.ID{}, "", false
}

func (a *App) onDisputeRecord(rec protocol.DisputeRecord, gen uint64) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.connGen != gen || a.doc == nil {
		return
	}
	if err := a.doc.ReplaceForeignClaims(a.personID, rec.Disputes); err != nil {
		return
	}
	a.persistAfterMutationLocked()
	a.emitSnapshotLocked()
}

func (a *App) applyRelayDisputeCCLocked(op protocol.Op) error {
	cc := op.DisputeCC
	claim := cc.Claim
	if claim.Person == a.personID {
		a.rememberClaimIDLocked(claim.RealLine.Hex(), claim.Action, claim.ID)
		a.markRelaySeenLocked(op.ID)
		a.persistAfterMutationLocked()
		return nil
	}
	if cc.TargetPersonID != a.personID {
		// 没写给本端。旁观争议只走已落盘记录。
		a.markRelaySeenLocked(op.ID)
		a.persistAfterMutationLocked()
		return nil
	}
	if err := a.doc.StoreExplicitClaim(claim); err != nil {
		return err
	}
	// 本端已经抄送过的主张一并留下，追随才能在双方主张上都成立。不再回一包新的主张。
	slot := claimSlotKey(claim.RealLine, claim.Action)
	if sent, ok := a.ownSent[slot]; ok {
		if err := a.doc.StoreExplicitClaim(sent); err != nil {
			return err
		}
	}
	a.markRelaySeenLocked(op.ID)
	a.persistAfterMutationLocked()
	a.emitSnapshotLocked()
	return nil
}
