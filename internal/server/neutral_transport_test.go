package server

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/KDreamelf/collaborative-editor/internal/document"
	"github.com/KDreamelf/collaborative-editor/internal/model"
	"github.com/KDreamelf/collaborative-editor/internal/protocol"
)

func TestNeutralJoinBootstrapCurrentChainAndClaims(t *testing.T) {
	h := NewHub(nil)
	meta := h.CreateArticle("t")
	r := h.getRoom(meta.ID)
	a, b := &wsClient{}, &wsClient{}

	msgsA := h.neutralJoin(r, a, "A", "甲")
	bootA := mustOutboundBootstrap(t, msgsA, a)
	if len(bootA.Base.Disputes) != 0 {
		t.Fatalf("Base.Disputes 应空: %+v", bootA.Base.Disputes)
	}
	if len(bootA.Disputes) != 0 || len(bootA.Base.Lines) != 1 || bootA.YourLine == "" {
		t.Fatalf("首入场: %+v", bootA)
	}

	line := bootA.Base.Lines[0].ID
	claimID := model.NewID()
	if err := r.applyNeutralOp("A", protocol.Op{
		ID: "ccA", Kind: protocol.TypeDisputeCC,
		DisputeCC: &protocol.DisputeCC{
			TargetPersonID: "B",
			Claim: model.Dispute{
				ID: claimID, RealLine: line, Action: model.ActionEdit, Person: "A",
				Content: []string{"甲主张"}, Followers: []string{},
			},
		},
	}); err != nil {
		t.Fatal(err)
	}

	msgsB := h.neutralJoin(r, b, "B", "乙")
	bootB := mustOutboundBootstrap(t, msgsB, b)
	if len(bootB.Base.Disputes) != 0 {
		t.Fatalf("Base.Disputes 应空: %+v", bootB.Base.Disputes)
	}
	if len(bootB.Disputes) != 1 || bootB.Disputes[0].ID != claimID {
		t.Fatalf("Disputes 应含显式主张: %+v", bootB.Disputes)
	}
	if len(bootB.Base.Lines) != 2 || bootB.YourLine == bootA.YourLine {
		t.Fatalf("新用户应有第二条初始行: %+v", bootB.Base.Lines)
	}
	snap := mustOutboundTyped(t, msgsB, a, protocol.TypeSnapshot)
	if snap == nil {
		t.Fatal("新用户加入应更新在线者列表")
	}
}

func TestNeutralJoinRejectsEmptyPersonOrNilRoom(t *testing.T) {
	h := NewHub(nil)
	meta := h.CreateArticle("t")
	r := h.getRoom(meta.ID)
	c := &wsClient{}

	msgs := h.neutralJoin(r, c, "", "甲")
	if typ := outboundType(t, msgs[0].data); typ != protocol.TypeError {
		t.Fatalf("空 person 应 TypeError: %s", msgs[0].data)
	}
	msgs = h.neutralJoin(nil, c, "A", "甲")
	if typ := outboundType(t, msgs[0].data); typ != protocol.TypeError {
		t.Fatalf("nil room 应 TypeError: %s", msgs[0].data)
	}
}

func TestNeutralJoinReusesAssignedInitialLine(t *testing.T) {
	h := NewHub(nil)
	r := h.getRoom(h.CreateArticle("t").ID)
	a, b := &wsClient{}, &wsClient{}
	first := mustOutboundBootstrap(t, h.neutralJoin(r, a, "A", "甲"), a)
	second := mustOutboundBootstrap(t, h.neutralJoin(r, b, "B", "乙"), b)
	if first.YourLine == second.YourLine || len(second.Base.Lines) != 2 {
		t.Fatalf("首次加入应分行: A=%s B=%s lines=%d", first.YourLine, second.YourLine, len(second.Base.Lines))
	}
	h.leave(r, a)
	rejoined := &wsClient{}
	boot := mustOutboundBootstrap(t, h.neutralJoin(r, rejoined, "A", "甲"), rejoined)
	if boot.YourLine != first.YourLine || len(boot.Base.Lines) != 2 || len(boot.Disputes) != 0 {
		t.Fatalf("同一身份重连应回原行，零争议: %+v", boot)
	}
}

func TestNeutralJoinPendingFollowRawRelay(t *testing.T) {
	h := NewHub(nil)
	meta := h.CreateArticle("t")
	r := h.getRoom(meta.ID)
	a, b := &wsClient{}, &wsClient{}
	h.neutralJoin(r, a, "A", "甲")
	h.neutralJoin(r, b, "B", "乙")
	line := mustView(t, r).Lines[0].ID
	claimB := model.NewID()
	if err := r.applyNeutralOp("B", protocol.Op{
		ID: "ccB", Kind: protocol.TypeDisputeCC,
		DisputeCC: &protocol.DisputeCC{
			TargetPersonID: "A",
			Claim: model.Dispute{
				ID: claimB, RealLine: line, Action: model.ActionEdit, Person: "B",
				Content: []string{"乙"}, Followers: []string{},
			},
		},
	}); err != nil {
		t.Fatal(err)
	}
	if err := r.applyNeutralOp("A", protocol.Op{
		ID: "f1", Kind: protocol.TypeFollow,
		Follow: &protocol.Follow{PersonID: "A", DisputeID: claimB.Hex(), ClientTs: 7},
	}); err != nil {
		t.Fatal(err)
	}

	// B 离线重连：Bootstrap 后 raw Follow，无 FollowAsk，不入 seen。
	b2 := &wsClient{}
	msgs := h.neutralJoin(r, b2, "B", "乙")
	boot := mustOutboundBootstrap(t, msgs, b2)
	pendingOK := false
	for _, d := range boot.Disputes {
		if d.ID == claimB && len(d.Pending) == 1 && d.Pending[0].From == "A" {
			pendingOK = true
		}
	}
	if !pendingOK {
		t.Fatalf("Bootstrap.Disputes 应含 Pending: %+v", boot.Disputes)
	}
	relays := outboundsOfType(t, msgs, b2, protocol.TypeRelay)
	if len(relays) != 1 {
		t.Fatalf("应补一条 Follow Relay: %d", len(relays))
	}
	var ev protocol.RelayEvent
	if err := json.Unmarshal(relays[0], &ev); err != nil {
		t.Fatal(err)
	}
	wantID := pendingFollowOpID(claimB.Hex(), "A", "B", 7)
	if ev.Op.ID != wantID || ev.Op.Kind != protocol.TypeFollow ||
		ev.Op.Follow == nil || ev.Op.Follow.PersonID != "A" || ev.Op.Follow.ClientTs != 7 {
		t.Fatalf("raw Follow: %+v", ev)
	}
	r.mu.Lock()
	_, inSeen := r.seenPayload[wantID]
	r.mu.Unlock()
	if inSeen {
		t.Fatal("pending key 不得入 seenPayload")
	}
	for _, m := range msgs {
		if outboundType(t, m.data) == protocol.TypeFollowAsk {
			t.Fatal("不得发旧 FollowAsk")
		}
	}
}

func TestNeutralBatchOrdinaryEditRelayIdempotentAndReject(t *testing.T) {
	h := NewHub(nil)
	meta := h.CreateArticle("t")
	r := h.getRoom(meta.ID)
	a, b := &wsClient{}, &wsClient{}
	h.neutralJoin(r, a, "A", "甲")
	h.neutralJoin(r, b, "B", "乙")
	line := mustView(t, r).Lines[0].ID.Hex()

	op := protocol.Op{
		ID: "e1", Kind: protocol.TypeSubmit,
		Submit: &protocol.Submit{PersonID: "A", LineID: line, Action: model.ActionEdit, Content: []string{"甲"}},
	}
	msgs := h.neutralBatch(r, a, protocol.Batch{Seq: 1, Ops: []protocol.Op{op}})
	ack := mustOutboundAck(t, msgs, a)
	if ack.Applied != 1 || ack.Message != "" {
		t.Fatalf("ACK: %+v", ack)
	}
	relays := outboundsOfType(t, msgs, b, protocol.TypeRelay)
	if len(relays) != 1 {
		t.Fatalf("应只向他人 Relay 一次: %d", len(relays))
	}
	var ev protocol.RelayEvent
	if err := json.Unmarshal(relays[0], &ev); err != nil || ev.Op.ID != "e1" || ev.Type != protocol.TypeRelay {
		t.Fatalf("Relay: %s err=%v", relays[0], err)
	}
	if len(outboundsOfType(t, msgs, a, protocol.TypeRelay)) != 0 {
		t.Fatal("sender 不应收自己的 Relay")
	}
	if n := len(mustView(t, r).Disputes); n != 0 {
		t.Fatalf("普通 Edit 无争议: %d", n)
	}

	msgs = h.neutralBatch(r, a, protocol.Batch{Seq: 2, Ops: []protocol.Op{op}})
	ack = mustOutboundAck(t, msgs, a)
	if ack.Applied != 1 || ack.Message != "" {
		t.Fatalf("幂等 ACK: %+v", ack)
	}
	if len(outboundsOfType(t, msgs, b, protocol.TypeRelay)) != 0 {
		t.Fatal("重复包不得再转发")
	}

	diff := protocol.Op{
		ID: "e1", Kind: protocol.TypeSubmit,
		Submit: &protocol.Submit{PersonID: "A", LineID: line, Action: model.ActionEdit, Content: []string{"异"}},
	}
	msgs = h.neutralBatch(r, a, protocol.Batch{Seq: 3, Ops: []protocol.Op{diff}})
	ack = mustOutboundAck(t, msgs, a)
	if ack.Applied != 0 || ack.Message == "" {
		t.Fatalf("同 ID 异内容拒且 Message 非空: %+v", ack)
	}
	if ackHasProtocolLeak(ack.Message) {
		t.Fatalf("Message 不得暴露协议字段: %q", ack.Message)
	}
	if len(outboundsOfType(t, msgs, b, protocol.TypeRelay)) != 0 {
		t.Fatal("拒包不得 Relay")
	}
	if mustView(t, r).Lines[0].Content != "甲" {
		t.Fatalf("拒包零突变: %q", mustView(t, r).Lines[0].Content)
	}
}

func TestNeutralBatchDomainRejectHasMessage(t *testing.T) {
	h := NewHub(nil)
	meta := h.CreateArticle("t")
	r := h.getRoom(meta.ID)
	a, b := &wsClient{}, &wsClient{}
	h.neutralJoin(r, a, "A", "甲")
	h.neutralJoin(r, b, "B", "乙")
	line := mustView(t, r).Lines[0].ID.Hex()

	ok := protocol.Op{
		ID: "ok1", Kind: protocol.TypeSubmit,
		Submit: &protocol.Submit{PersonID: "A", LineID: line, Action: model.ActionEdit, Content: []string{"先成"}},
	}
	bad := protocol.Op{
		ID: "bad1", Kind: protocol.TypeSubmit,
		Submit: &protocol.Submit{PersonID: "A", LineID: "not-a-line-id", Action: model.ActionEdit, Content: []string{"拒"}},
	}
	msgs := h.neutralBatch(r, a, protocol.Batch{Seq: 1, Ops: []protocol.Op{ok, bad}})
	ack := mustOutboundAck(t, msgs, a)
	if ack.Applied != 1 || ack.Message == "" {
		t.Fatalf("域校验拒：Applied=1 且 Message 非空: %+v", ack)
	}
	if ackHasProtocolLeak(ack.Message) {
		t.Fatalf("Message 不得暴露协议字段: %q", ack.Message)
	}
	if len(outboundsOfType(t, msgs, b, protocol.TypeRelay)) != 1 {
		t.Fatal("仅成功 Op 转发")
	}
	if mustView(t, r).Lines[0].Content != "先成" {
		t.Fatalf("拒后正文: %q", mustView(t, r).Lines[0].Content)
	}
}

func TestNeutralBatchDisputeOnlyToTargetRecordToObserver(t *testing.T) {
	h := NewHub(nil)
	meta := h.CreateArticle("t")
	r := h.getRoom(meta.ID)
	a, b, c := &wsClient{}, &wsClient{}, &wsClient{}
	h.neutralJoin(r, a, "A", "甲")
	h.neutralJoin(r, b, "B", "乙")
	h.neutralJoin(r, c, "C", "丙")
	line := mustView(t, r).Lines[0].ID
	claimID := model.NewID()
	cc := protocol.Op{
		ID: "cc", Kind: protocol.TypeDisputeCC,
		DisputeCC: &protocol.DisputeCC{
			TargetPersonID: "B",
			Claim: model.Dispute{
				ID: claimID, RealLine: line, Action: model.ActionEdit, Person: "A",
				Content: []string{"甲主张"}, Followers: []string{},
			},
		},
	}
	msgs := h.neutralBatch(r, a, protocol.Batch{Seq: 1, Ops: []protocol.Op{cc}})
	if len(outboundsOfType(t, msgs, a, protocol.TypeRelay)) != 0 || len(outboundsOfType(t, msgs, a, protocol.TypeDisputeRecord)) != 0 {
		t.Fatal("发送者不得收回主张包或落盘记录")
	}
	if len(outboundsOfType(t, msgs, b, protocol.TypeRelay)) != 1 {
		t.Fatal("主张包只送给收件人")
	}
	if len(outboundsOfType(t, msgs, c, protocol.TypeRelay)) != 0 {
		t.Fatal("旁观者不得收到主张包")
	}
	recs := outboundsOfType(t, msgs, c, protocol.TypeDisputeRecord)
	if len(recs) != 1 {
		t.Fatalf("旁观者应收到落盘记录: %d", len(recs))
	}
	var rec protocol.DisputeRecord
	if err := json.Unmarshal(recs[0], &rec); err != nil || len(rec.Disputes) != 1 || rec.Disputes[0].ID != claimID || rec.Disputes[0].Person != "A" {
		t.Fatalf("记录: %s err=%v", recs[0], err)
	}
}

func TestNeutralJoinReplaysDisputeToTargetOnly(t *testing.T) {
	h := NewHub(nil)
	meta := h.CreateArticle("t")
	r := h.getRoom(meta.ID)
	a := &wsClient{}
	h.neutralJoin(r, a, "A", "甲")
	line := mustView(t, r).Lines[0].ID
	claimID := model.NewID()
	cc := protocol.Op{
		ID: "cc", Kind: protocol.TypeDisputeCC,
		DisputeCC: &protocol.DisputeCC{
			TargetPersonID: "B",
			Claim: model.Dispute{
				ID: claimID, RealLine: line, Action: model.ActionEdit, Person: "A",
				Content: []string{"甲主张"}, Followers: []string{},
			},
		},
	}
	if ack := mustOutboundAck(t, h.neutralBatch(r, a, protocol.Batch{Seq: 1, Ops: []protocol.Op{cc}}), a); ack.Applied != 1 {
		t.Fatalf("ACK: %+v", ack)
	}
	stored := mustView(t, r).Disputes
	if len(stored) != 1 || stored[0].Target != "B" {
		t.Fatalf("落盘应记下收件人: %+v", stored)
	}

	b := &wsClient{}
	join := h.neutralJoin(r, b, "B", "乙")
	found := false
	for _, raw := range outboundsOfType(t, join, b, protocol.TypeRelay) {
		var ev protocol.RelayEvent
		if json.Unmarshal(raw, &ev) != nil || ev.Op.Kind != protocol.TypeDisputeCC || ev.Op.DisputeCC == nil {
			continue
		}
		found = true
		if ev.Op.DisputeCC.TargetPersonID != "B" || ev.Op.DisputeCC.Claim.ID != claimID {
			t.Fatalf("重连主张包: %+v", ev.Op.DisputeCC)
		}
	}
	if !found {
		t.Fatal("收件人重连应再收到写给自己的主张包")
	}

	c := &wsClient{}
	cJoin := h.neutralJoin(r, c, "C", "丙")
	for _, raw := range outboundsOfType(t, cJoin, c, protocol.TypeRelay) {
		var ev protocol.RelayEvent
		if json.Unmarshal(raw, &ev) == nil && ev.Op.Kind == protocol.TypeDisputeCC {
			t.Fatal("旁观者入场不得收到主张包")
		}
	}
}

func TestNeutralBatchCCThenDisputeNoErrorText(t *testing.T) {
	h := NewHub(nil)
	meta := h.CreateArticle("t")
	r := h.getRoom(meta.ID)
	a, b := &wsClient{}, &wsClient{}
	h.neutralJoin(r, a, "A", "甲")
	h.neutralJoin(r, b, "B", "乙")
	line := mustView(t, r).Lines[0].ID

	edit := protocol.Op{
		ID: "e", Kind: protocol.TypeSubmit,
		Submit: &protocol.Submit{PersonID: "A", LineID: line.Hex(), Action: model.ActionEdit, Content: []string{"正文"}},
	}
	msgs := h.neutralBatch(r, a, protocol.Batch{Seq: 1, Ops: []protocol.Op{edit}})
	if mustOutboundAck(t, msgs, a).Message != "" {
		t.Fatalf("ACK 不得带错误正文: %s", msgs[0].data)
	}
	for _, raw := range outboundsOfType(t, msgs, b, protocol.TypeRelay) {
		if outboundHasErrorText(raw) {
			t.Fatalf("Relay 不得带技术错误正文: %s", raw)
		}
	}
	if n := len(mustView(t, r).Disputes); n != 0 {
		t.Fatalf("普通 Edit 后无争议: %d", n)
	}

	cc := protocol.Op{
		ID: "cc", Kind: protocol.TypeDisputeCC,
		DisputeCC: &protocol.DisputeCC{
			TargetPersonID: "B",
			Claim: model.Dispute{
				ID: model.NewID(), RealLine: line, Action: model.ActionEdit, Person: "A",
				Content: []string{"甲主张"}, Followers: []string{},
			},
		},
	}
	msgs = h.neutralBatch(r, a, protocol.Batch{Seq: 2, Ops: []protocol.Op{cc}})
	ack := mustOutboundAck(t, msgs, a)
	if ack.Applied != 1 || ack.Message != "" {
		t.Fatalf("CC ACK: %+v", ack)
	}
	relays := outboundsOfType(t, msgs, b, protocol.TypeRelay)
	if len(relays) != 1 {
		t.Fatalf("CC 应 Relay: %d", len(relays))
	}
	if outboundHasErrorText(relays[0]) {
		t.Fatalf("Relay 不得带技术错误正文: %s", relays[0])
	}
	if n := len(mustView(t, r).Disputes); n != 1 {
		t.Fatalf("CC 后才有 Dispute: %d", n)
	}
	if len(outboundsOfType(t, msgs, nil, protocol.TypeSnapshot)) != 0 {
		t.Fatal("不得广播全局争议快照")
	}
}

func TestNeutralBatchMongoSaveFailNoAckKeepsDoc(t *testing.T) {
	fs := &fakeStore{docs: map[string]*document.Doc{}, saveErr: errors.New("write concern failed")}
	h := NewHub(fs)
	meta := h.CreateArticle("t")
	r := h.getRoom(meta.ID)
	a, b := &wsClient{}, &wsClient{}
	h.neutralJoin(r, a, "A", "甲")
	h.neutralJoin(r, b, "B", "乙")
	line := mustView(t, r).Lines[0].ID.Hex()
	before := mustView(t, r).Lines[0].Content

	op := protocol.Op{
		ID: "e1", Kind: protocol.TypeSubmit,
		Submit: &protocol.Submit{PersonID: "A", LineID: line, Action: model.ActionEdit, Content: []string{"甲"}},
	}
	msgs := h.neutralBatch(r, a, protocol.Batch{Seq: 1, Ops: []protocol.Op{op}})
	if mustOutboundTyped(t, msgs, a, protocol.TypeAck) != nil {
		t.Fatal("mongo save 失败不得发 ACK")
	}
	if len(outboundsOfType(t, msgs, b, protocol.TypeRelay)) != 0 {
		t.Fatal("save 失败不得转发")
	}
	if mustView(t, r).Lines[0].Content != before {
		t.Fatalf("正文应不变: %q", mustView(t, r).Lines[0].Content)
	}
	r.mu.Lock()
	_, seen := r.seenPayload["e1"]
	dirty := r.dirty
	r.mu.Unlock()
	if seen {
		t.Fatal("save 失败不得记 seenPayload")
	}
	if !dirty {
		t.Fatal("Join dirty 应保留给 flush ticker")
	}

	fs.mu.Lock()
	fs.saveErr = nil
	fs.mu.Unlock()
	msgs = h.neutralBatch(r, a, protocol.Batch{Seq: 2, Ops: []protocol.Op{op}})
	ack := mustOutboundAck(t, msgs, a)
	if ack.Applied != 1 || ack.Message != "" {
		t.Fatalf("清除错误后应成功: %+v", ack)
	}
	if mustView(t, r).Lines[0].Content != "甲" {
		t.Fatalf("重试后正文: %q", mustView(t, r).Lines[0].Content)
	}
	if len(outboundsOfType(t, msgs, b, protocol.TypeRelay)) != 1 {
		t.Fatal("成功应转发一次")
	}

	msgs = h.neutralBatch(r, a, protocol.Batch{Seq: 3, Ops: []protocol.Op{op}})
	ack = mustOutboundAck(t, msgs, a)
	if ack.Applied != 1 || ack.Message != "" {
		t.Fatalf("幂等 ACK: %+v", ack)
	}
	if len(outboundsOfType(t, msgs, b, protocol.TypeRelay)) != 0 {
		t.Fatal("幂等不得再转发")
	}
}

func TestNeutralBatchMongoPartialSaveThenResend(t *testing.T) {
	fs := &fakeStore{docs: map[string]*document.Doc{}, failAtSave: 2}
	h := NewHub(fs)
	meta := h.CreateArticle("t")
	r := h.getRoom(meta.ID)
	a, b := &wsClient{}, &wsClient{}
	h.neutralJoin(r, a, "A", "甲")
	h.neutralJoin(r, b, "B", "乙")
	line := mustView(t, r).Lines[0].ID.Hex()

	op1 := protocol.Op{
		ID: "e1", Kind: protocol.TypeSubmit,
		Submit: &protocol.Submit{PersonID: "A", LineID: line, Action: model.ActionEdit, Content: []string{"一"}},
	}
	op2 := protocol.Op{
		ID: "e2", Kind: protocol.TypeSubmit,
		Submit: &protocol.Submit{PersonID: "A", LineID: line, Action: model.ActionEdit, Content: []string{"二"}},
	}
	msgs := h.neutralBatch(r, a, protocol.Batch{Seq: 1, Ops: []protocol.Op{op1, op2}})
	if mustOutboundTyped(t, msgs, a, protocol.TypeAck) != nil {
		t.Fatal("第二笔 save 失败不得发 ACK")
	}
	if len(outboundsOfType(t, msgs, b, protocol.TypeRelay)) != 1 {
		t.Fatal("第一笔已保存应已转发")
	}
	if mustView(t, r).Lines[0].Content != "一" {
		t.Fatalf("仅第一笔生效: %q", mustView(t, r).Lines[0].Content)
	}
	r.mu.Lock()
	_, seen1 := r.seenPayload["e1"]
	_, seen2 := r.seenPayload["e2"]
	r.mu.Unlock()
	if !seen1 || seen2 {
		t.Fatalf("seen: e1=%v e2=%v", seen1, seen2)
	}

	fs.mu.Lock()
	fs.failAtSave = 0
	fs.mu.Unlock()
	msgs = h.neutralBatch(r, a, protocol.Batch{Seq: 2, Ops: []protocol.Op{op1, op2}})
	ack := mustOutboundAck(t, msgs, a)
	if ack.Applied != 2 || ack.Message != "" {
		t.Fatalf("整批重发 ACK: %+v", ack)
	}
	relays := outboundsOfType(t, msgs, b, protocol.TypeRelay)
	if len(relays) != 1 {
		t.Fatalf("只转发第二笔，不得重复第一笔: %d", len(relays))
	}
	var ev protocol.RelayEvent
	if err := json.Unmarshal(relays[0], &ev); err != nil || ev.Op.ID != "e2" {
		t.Fatalf("应只 Relay e2: %s err=%v", relays[0], err)
	}
	if mustView(t, r).Lines[0].Content != "二" {
		t.Fatalf("重发后正文: %q", mustView(t, r).Lines[0].Content)
	}
}

func TestNeutralBatchMemoryFastPathUnchanged(t *testing.T) {
	h := NewHub(nil)
	meta := h.CreateArticle("t")
	r := h.getRoom(meta.ID)
	a, b := &wsClient{}, &wsClient{}
	h.neutralJoin(r, a, "A", "甲")
	h.neutralJoin(r, b, "B", "乙")
	line := mustView(t, r).Lines[0].ID.Hex()

	op := protocol.Op{
		ID: "e1", Kind: protocol.TypeSubmit,
		Submit: &protocol.Submit{PersonID: "A", LineID: line, Action: model.ActionEdit, Content: []string{"甲"}},
	}
	msgs := h.neutralBatch(r, a, protocol.Batch{Seq: 1, Ops: []protocol.Op{op}})
	ack := mustOutboundAck(t, msgs, a)
	if ack.Applied != 1 || ack.Message != "" {
		t.Fatalf("无 Mongo ACK: %+v", ack)
	}
	if len(outboundsOfType(t, msgs, b, protocol.TypeRelay)) != 1 {
		t.Fatal("无 Mongo 应转发")
	}
	r.mu.Lock()
	dirty := r.dirty
	r.mu.Unlock()
	if !dirty {
		t.Fatal("无 Mongo 仍标 dirty")
	}
}

func mustOutboundBootstrap(t *testing.T, msgs []outbound, to *wsClient) protocol.Bootstrap {
	t.Helper()
	for _, m := range msgs {
		if m.client != to {
			continue
		}
		if outboundType(t, m.data) != protocol.TypeBootstrap {
			continue
		}
		var boot protocol.Bootstrap
		if err := json.Unmarshal(m.data, &boot); err != nil {
			t.Fatal(err)
		}
		return boot
	}
	t.Fatalf("缺 Bootstrap for %v in %d msgs", to, len(msgs))
	return protocol.Bootstrap{}
}

func mustOutboundAck(t *testing.T, msgs []outbound, to *wsClient) protocol.Ack {
	t.Helper()
	for _, m := range msgs {
		if m.client != to {
			continue
		}
		if outboundType(t, m.data) != protocol.TypeAck {
			continue
		}
		var ack protocol.Ack
		if err := json.Unmarshal(m.data, &ack); err != nil {
			t.Fatal(err)
		}
		return ack
	}
	t.Fatalf("缺 ACK")
	return protocol.Ack{}
}

func mustOutboundTyped(t *testing.T, msgs []outbound, to *wsClient, typ string) []byte {
	t.Helper()
	for _, m := range msgs {
		if to != nil && m.client != to {
			continue
		}
		if outboundType(t, m.data) == typ {
			return m.data
		}
	}
	return nil
}

func outboundsOfType(t *testing.T, msgs []outbound, to *wsClient, typ string) [][]byte {
	t.Helper()
	var out [][]byte
	for _, m := range msgs {
		if to != nil && m.client != to {
			continue
		}
		if outboundType(t, m.data) == typ {
			out = append(out, m.data)
		}
	}
	return out
}

func outboundType(t *testing.T, data []byte) string {
	t.Helper()
	var head struct {
		Type string `json:"type"`
	}
	if err := json.Unmarshal(data, &head); err != nil {
		t.Fatal(err)
	}
	return head.Type
}

func outboundHasErrorText(data []byte) bool {
	var m map[string]any
	if err := json.Unmarshal(data, &m); err != nil {
		return true
	}
	if msg, ok := m["message"].(string); ok && msg != "" {
		return true
	}
	if msg, ok := m["error"].(string); ok && msg != "" {
		return true
	}
	return false
}

func ackHasProtocolLeak(msg string) bool {
	lower := strings.ToLower(msg)
	for _, leak := range []string{"op.id", "personid", "lineid", "stack"} {
		if strings.Contains(lower, leak) {
			return true
		}
	}
	return false
}
