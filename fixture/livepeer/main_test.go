//go:build fixture

package main

import (
	"fmt"
	"math/rand"
	"strings"
	"testing"
	"time"

	"github.com/KDreamelf/collaborative-editor/internal/document"
	"github.com/KDreamelf/collaborative-editor/internal/model"
	"github.com/KDreamelf/collaborative-editor/internal/protocol"
)

func TestCursorUTF16End(t *testing.T) {
	s := "你好𠮷世界"
	got := utf16Len(s)
	if got != 6 {
		t.Fatalf("期望 6 UTF-16 units，得 %d", got)
	}
	if got == len([]byte(s)) {
		t.Fatal("不得用 UTF-8 字节长当 offset")
	}
	line := mustID("000000000000000000000001")
	p := newTestPeer("p1", line.Hex(), s)
	text, _, dispute := p.cursorTarget()
	if text != s || dispute != "" || utf16Len(text) == 0 {
		t.Fatalf("行尾光标 text=%q dispute=%q off=%d", text, dispute, utf16Len(text))
	}
}

func TestOrdinaryEditTriggersCCWithoutLocalDispute(t *testing.T) {
	line := model.NewID()
	p := newTestPeer("script", line.Hex(), "脚本正文")
	op := protocol.Op{
		ID:   model.NewID().Hex(),
		Kind: protocol.TypeSubmit,
		Submit: &protocol.Submit{
			Type: protocol.TypeSubmit, PersonID: "user", LineID: line.Hex(),
			Action: model.ActionEdit, Content: []string{"用户异文"},
		},
	}
	if err := p.onOrdinaryEdit(op, nil); err != nil {
		t.Fatal(err)
	}
	if len(p.foreign) != 0 {
		t.Fatalf("发 CC 后本端不得进争议 foreign=%d", len(p.foreign))
	}
	if p.ownText != "脚本正文" {
		t.Fatalf("本端正文被改写: %q", p.ownText)
	}
	if len(p.outQ) != 1 || p.outQ[0].Kind != protocol.TypeDisputeCC {
		t.Fatalf("应入队 DisputeCC，outQ=%v", kinds(p.outQ))
	}
	cc := p.outQ[0].DisputeCC
	if cc.TargetPersonID != "user" || cc.Claim.Content[0] != "脚本正文" {
		t.Fatalf("cc=%+v", cc)
	}
	n := len(p.outQ)
	if err := p.onOrdinaryEdit(op, nil); err != nil {
		t.Fatal(err)
	}
	if len(p.outQ) != n {
		t.Fatal("重复普通 Edit（同 Op.ID seen）不得再入队")
	}
}

func TestSameClaimTwoContentCCDifferentOpID(t *testing.T) {
	line := model.NewID()
	p := newTestPeer("script", line.Hex(), "正文甲")
	p.claimID = model.NewID()
	op1 := p.buildOwnCC("user")
	p.ownText = "正文乙"
	op2 := p.buildOwnCC("user")
	if op1.DisputeCC.Claim.ID != op2.DisputeCC.Claim.ID || op1.DisputeCC.Claim.ID != p.claimID {
		t.Fatalf("Claim.ID 须稳定: %s vs %s", op1.DisputeCC.Claim.ID.Hex(), op2.DisputeCC.Claim.ID.Hex())
	}
	if op1.ID == op2.ID {
		t.Fatal("异文两次 CC 必须新 Op.ID，否则 Room 会当重发丢弃")
	}
	if op1.DisputeCC.Claim.Content[0] == op2.DisputeCC.Claim.Content[0] {
		t.Fatal("两次内容应不同")
	}
}

func TestForeignCCStartsDecisionOnce(t *testing.T) {
	line := model.NewID()
	p := newTestPeer("script", line.Hex(), "脚本正文")
	p.claimID = model.NewID()
	fid := model.NewID()
	var disputeC <-chan time.Time
	op := protocol.Op{
		ID:   model.NewID().Hex(),
		Kind: protocol.TypeDisputeCC,
		DisputeCC: &protocol.DisputeCC{
			TargetPersonID: "script",
			Claim: model.Dispute{
				ID: fid, RealLine: line, Action: model.ActionEdit,
				Person: "user", Content: []string{"用户文"},
			},
		},
	}
	if err := p.onDisputeCC(op, &disputeC); err != nil {
		t.Fatal(err)
	}
	if p.disputeTimer == nil || disputeC == nil {
		t.Fatal("foreign CC 应启动 10s 决策")
	}
	key := p.disputeKey
	op2 := op
	op2.ID = model.NewID().Hex()
	if err := p.onDisputeCC(op2, &disputeC); err != nil {
		t.Fatal(err)
	}
	if p.disputeKey != key {
		t.Fatalf("不得换决策 key")
	}
	p.stopDisputeTimer()
}

func TestFollowAnswerQueuedStableOpIDAckThenAnswered(t *testing.T) {
	line := model.NewID()
	p := newTestPeer("script", line.Hex(), "脚本正文")
	p.claimID = model.NewID()
	fop := protocol.Op{ID: model.NewID().Hex(), Kind: protocol.TypeFollow, Follow: &protocol.Follow{
		Type: protocol.TypeFollow, PersonID: "user",
		DisputeID: p.claimID.Hex(), ClientTs: 42,
	}}
	if err := p.onFollow(fop); err != nil {
		t.Fatal(err)
	}
	if len(p.outQ) != 1 || p.outQ[0].Kind != protocol.TypeFollowAnswer || !p.outQ[0].FollowAnswer.Accept {
		t.Fatalf("应立即入队 Accept Answer: %v", kinds(p.outQ))
	}
	ansID := p.outQ[0].ID
	key := followKey(fop.Follow)
	if p.answered[key] {
		t.Fatal("ACK 前不得标记 answered")
	}
	// 再收到同 Follow：复用稳定 Op.ID，不双份
	fop2 := fop
	fop2.ID = model.NewID().Hex()
	if err := p.onFollow(fop2); err != nil {
		t.Fatal(err)
	}
	if len(p.outQ) != 1 || p.outQ[0].ID != ansID {
		t.Fatalf("须稳定 Answer Op.ID 留队，q=%d id=%s", len(p.outQ), p.outQ[0].ID)
	}
	p.inflightSeq = 1
	if err := p.onAck(protocol.Ack{Type: protocol.TypeAck, Seq: 1, Applied: 1}); err != nil {
		t.Fatal(err)
	}
	if !p.answered[key] || len(p.outQ) != 0 {
		t.Fatalf("ACK 后才 answered；answered=%v q=%d", p.answered[key], len(p.outQ))
	}
}

func TestQueueSurvivesDisconnectResendSameOpID(t *testing.T) {
	line := model.NewID()
	p := newTestPeer("script", line.Hex(), "脚本正文")
	p.claimID = model.NewID()
	cc := p.buildOwnCC("user")
	p.enqueue(cc)
	ansID := model.NewID().Hex()
	p.answerOpID["user|"+p.claimID.Hex()+"|7"] = ansID
	p.enqueue(protocol.Op{
		ID:   ansID,
		Kind: protocol.TypeFollowAnswer,
		FollowAnswer: &protocol.FollowAnswer{
			Type: protocol.TypeFollowAnswer, PersonID: "script",
			FromID: "user", DisputeID: p.claimID.Hex(), Accept: true,
		},
	})
	if len(p.outQ) != 2 {
		t.Fatalf("q=%d", len(p.outQ))
	}
	id0, id1 := p.outQ[0].ID, p.outQ[1].ID

	// 模拟已写出后断线
	p.inflightSeq = 9
	p.conn = nil
	p.inflightSeq = 0

	if len(p.outQ) != 2 || p.outQ[0].ID != id0 || p.outQ[1].ID != id1 {
		t.Fatalf("断线须保留队列同 Op.ID: %+v", p.outQ)
	}
	// 无 conn 时 flush 为空操作；队头 Op.ID 不变，待重连重发。
	if err := p.flushOut(); err != nil {
		t.Fatal(err)
	}
	if p.inflightSeq != 0 || p.outQ[0].ID != id0 {
		t.Fatal("无连接不得推进在途或丢队头")
	}
	// 重连 Bootstrap：Disputes 已有同 Claim.ID+Content → CC 出队；Answer 无法确认则原 ID 重发
	boot := protocol.Bootstrap{
		Type:     protocol.TypeBootstrap,
		YourLine: line.Hex(),
		Base:     viewOne(line, "脚本正文"),
		Disputes: []model.Dispute{cc.DisputeCC.Claim},
	}
	if err := p.applyBootstrap(boot, nil); err != nil {
		t.Fatal(err)
	}
	if len(p.outQ) != 1 || p.outQ[0].ID != id1 {
		t.Fatalf("Disputes 已落地 CC 应出队，Answer 原 ID 重发: q=%v", p.outQ)
	}
	if p.confirmedCC["user"] != "脚本正文" {
		t.Fatalf("落地 CC 应记 confirmedCC，得 %q", p.confirmedCC["user"])
	}
}

func TestKeepOwnThenTickWriteAndNewCC(t *testing.T) {
	line := model.NewID()
	p := newTestPeer("script", line.Hex(), "脚本旧")
	p.claimID = model.NewID()
	p.rng = randSrc(1)
	fid := model.NewID()
	p.foreign[fid.Hex()] = model.Dispute{
		ID: fid, RealLine: line, Action: model.ActionEdit,
		Person: "user", Content: []string{"用户文"},
	}
	p.handled[disputeKeyOf(p.foreign[fid.Hex()])] = true // 已选保留本人

	if err := p.maybeWrite(); err != nil {
		t.Fatal(err)
	}
	if len(p.outQ) != 1 || p.outQ[0].Kind != protocol.TypeDisputeCC {
		t.Fatalf("争议后续写只更新本人主张，不再改正式行，q=%v", kinds(p.outQ))
	}
	var cc *protocol.Op
	for i := range p.outQ {
		if p.outQ[i].Kind == protocol.TypeDisputeCC {
			op := p.outQ[i]
			cc = &op
			break
		}
	}
	if cc == nil || cc.DisputeCC.Claim.ID != p.claimID {
		t.Fatal("须带稳定 Claim.ID 的新 CC")
	}
	if cc.DisputeCC.Claim.Content[0] != p.ownText {
		t.Fatalf("CC 正文应是更新后主张 %q", p.ownText)
	}
}

func TestPendingEditNotOverwrittenByFollow(t *testing.T) {
	line := model.NewID()
	p := newTestPeer("script", line.Hex(), "脚本")
	p.rng = randSrc(0) // Intn(2)==0 → Follow
	editID := model.NewID().Hex()
	p.enqueue(protocol.Op{
		ID:   editID,
		Kind: protocol.TypeSubmit,
		Submit: &protocol.Submit{
			Type: protocol.TypeSubmit, PersonID: "script", LineID: line.Hex(),
			Action: model.ActionEdit, Content: []string{"待发编辑"},
		},
	})
	fid := model.NewID()
	p.foreign[fid.Hex()] = model.Dispute{
		ID: fid, RealLine: line, Action: model.ActionEdit,
		Person: "user", Content: []string{"用户"},
	}
	p.disputeKey = disputeKeyOf(p.foreign[fid.Hex()])
	if err := p.onDisputeTimer(); err != nil {
		t.Fatal(err)
	}
	if len(p.outQ) < 2 {
		t.Fatalf("Follow 应追加而非覆盖，q=%v", kinds(p.outQ))
	}
	if p.outQ[0].ID != editID || p.outQ[0].Kind != protocol.TypeSubmit {
		t.Fatalf("队头须仍是待发编辑: %+v", p.outQ[0])
	}
	if p.outQ[1].Kind != protocol.TypeFollow {
		t.Fatalf("Follow 应在编辑之后: %v", kinds(p.outQ))
	}
	if !p.awaitFollow {
		t.Fatal("选 Follow 后 awaitFollow")
	}
	// awaitFollow 期间 maybeWrite 不得再堆编辑
	n := len(p.outQ)
	if err := p.maybeWrite(); err != nil {
		t.Fatal(err)
	}
	if len(p.outQ) != n {
		t.Fatal("等 Answer 期间不得再写")
	}
}

func TestAckRejectKeepsOwnTextDropsBadOp(t *testing.T) {
	line := model.NewID()
	p := newTestPeer("script", line.Hex(), "旧文")
	p.ownText = "已乐观写入"
	p.enqueue(protocol.Op{
		ID: "bad1", Kind: protocol.TypeSubmit,
		Submit: &protocol.Submit{Content: []string{"已乐观写入"}, LineID: line.Hex()},
	})
	p.inflightSeq = 3
	err := p.onAck(protocol.Ack{Type: protocol.TypeAck, Seq: 3, Applied: 0, Message: "拒绝"})
	if err == nil || p.ownText != "已乐观写入" {
		t.Fatalf("须报错且保留正文 err=%v own=%q", err, p.ownText)
	}
	if len(p.outQ) != 0 {
		t.Fatal("拒绝包须出队，避免无限重发")
	}
}

func TestFirstJoinBaseLatestNoCC(t *testing.T) {
	line := model.NewID()
	p := freshPeer("script")
	boot := protocol.Bootstrap{
		Type: protocol.TypeBootstrap, YourLine: line.Hex(),
		Base: viewOne(line, "最新文本"),
	}
	if err := p.applyBootstrap(boot, nil); err != nil {
		t.Fatal(err)
	}
	if p.ownText != "最新文本" {
		t.Fatalf("首次 Join 应取 Base 当前文，得 %q", p.ownText)
	}
	if p.attending {
		t.Fatal("Bootstrap 后不得立刻有注意力")
	}
	if !p.seeded {
		t.Fatal("已有 Base 正文应 seeded，不得再首写")
	}
	for _, op := range p.outQ {
		if op.Kind == protocol.TypeDisputeCC {
			t.Fatalf("Base 最新文不得误发 CC: %+v", op)
		}
	}
}

func TestBootstrapFocusExistingLine(t *testing.T) {
	first, second := model.NewID(), model.NewID()
	p := freshPeer("script")
	p.focusLine = first.Hex()
	boot := protocol.Bootstrap{Type: protocol.TypeBootstrap, YourLine: second.Hex(),
		Base: document.View{Article: model.Article{ID: model.NewID(), Title: "t"}, Lines: []model.Line{
			{ID: first, Next: second, Content: "正在写的行"}, {ID: second, Prev: first},
		}}}
	if err := p.applyBootstrap(boot, nil); err != nil {
		t.Fatal(err)
	}
	if p.yourLine != first.Hex() || p.ownText != "正在写的行" || len(p.outQ) != 0 {
		t.Fatalf("联测夹具应移动注意力到指定已有行: line=%s text=%q queue=%v", p.yourLine, p.ownText, kinds(p.outQ))
	}
}

func TestBootstrapOwnDisputeOnlyNoForeign(t *testing.T) {
	line := model.NewID()
	claimID := model.NewID()
	p := freshPeer("script")
	var disputeC <-chan time.Time
	boot := protocol.Bootstrap{
		Type: protocol.TypeBootstrap, YourLine: line.Hex(),
		Base: viewOne(line, "我的文"),
		Disputes: []model.Dispute{{
			ID: claimID, RealLine: line, Action: model.ActionEdit,
			Person: "script", Content: []string{"我的文"},
		}},
	}
	if err := p.applyBootstrap(boot, &disputeC); err != nil {
		t.Fatal(err)
	}
	if p.claimID != claimID {
		t.Fatalf("本人主张应稳 claimID，得 %s want %s", p.claimID.Hex(), claimID.Hex())
	}
	if len(p.foreign) != 0 {
		t.Fatalf("本人主张不得自争议 foreign=%d", len(p.foreign))
	}
	if p.disputeTimer != nil || disputeC != nil {
		t.Fatal("仅本人 Disputes 不得开 10s 决策")
	}
}

func TestBootstrapForeignDisputeStartsDecision(t *testing.T) {
	line := model.NewID()
	fid := model.NewID()
	p := freshPeer("script")
	var disputeC <-chan time.Time
	boot := protocol.Bootstrap{
		Type: protocol.TypeBootstrap, YourLine: line.Hex(),
		Base: viewOne(line, "脚本正文"),
		Disputes: []model.Dispute{{
			ID: fid, RealLine: line, Action: model.ActionEdit,
			Person: "user", Content: []string{"用户文"},
		}},
	}
	if err := p.applyBootstrap(boot, &disputeC); err != nil {
		t.Fatal(err)
	}
	if len(p.foreign) != 1 || p.foreign[fid.Hex()].Person != "user" {
		t.Fatalf("他人主张应按 Claim.ID 导入 foreign=%+v", p.foreign)
	}
	if p.disputeTimer == nil || disputeC == nil {
		t.Fatal("他人 Disputes 应启动 10s 决策")
	}
	p.stopDisputeTimer()
}

// 别行主张不得进 foreign / 挡写 / 开 Follow；本行继续普通 Edit。
func TestOtherLineDisputeIgnoredBootstrapAndLive(t *testing.T) {
	mine, other := model.NewID(), model.NewID()
	fid := model.NewID()
	p := freshPeer("script")
	p.rng = randSrc(1)
	var disputeC <-chan time.Time
	boot := protocol.Bootstrap{
		Type: protocol.TypeBootstrap, YourLine: mine.Hex(),
		Base: document.View{
			Article: model.Article{ID: model.NewID(), Title: "测试"},
			Lines: []model.Line{
				{ID: mine, Content: "本行正文"},
				{ID: other, Content: "别行正文"},
			},
		},
		Disputes: []model.Dispute{{
			ID: fid, RealLine: other, Action: model.ActionEdit,
			Person: "user", Content: []string{"别行争议文"},
		}},
	}
	if err := p.applyBootstrap(boot, &disputeC); err != nil {
		t.Fatal(err)
	}
	if len(p.foreign) != 0 || p.disputeTimer != nil || disputeC != nil {
		t.Fatalf("别行 Bootstrap 主张不得入场 foreign=%d timer=%v", len(p.foreign), p.disputeTimer != nil)
	}
	if n := len(p.outQ); n != 0 {
		t.Fatalf("别行主张不得触发 CC/Follow: %v", kinds(p.outQ))
	}
	if p.ownText != "本行正文" {
		t.Fatalf("本行正文被改: %q", p.ownText)
	}
	if err := p.maybeWrite(); err != nil {
		t.Fatal(err)
	}
	if len(p.outQ) != 1 || p.outQ[0].Kind != protocol.TypeSubmit || p.outQ[0].Submit.LineID != mine.Hex() {
		t.Fatalf("本行应能继续普通写: %v", kinds(p.outQ))
	}
	p.outQ = nil

	live := protocol.Op{
		ID: model.NewID().Hex(), Kind: protocol.TypeDisputeCC,
		DisputeCC: &protocol.DisputeCC{
			TargetPersonID: "script",
			Claim: model.Dispute{
				ID: model.NewID(), RealLine: other, Action: model.ActionEdit,
				Person: "user", Content: []string{"别行在线CC"},
			},
		},
	}
	if err := p.onDisputeCC(live, &disputeC); err != nil {
		t.Fatal(err)
	}
	if !p.seen[live.ID] {
		t.Fatal("别行 CC 应记 seen")
	}
	if len(p.foreign) != 0 || p.disputeTimer != nil || disputeC != nil || len(p.outQ) != 0 {
		t.Fatalf("在线别行 CC 不得 foreign/计时/出队 foreign=%d q=%v", len(p.foreign), kinds(p.outQ))
	}
	if p.ownText == "别行在线CC" || p.formal[mine.Hex()] == "别行在线CC" {
		t.Fatal("别行 CC 不得写入本行")
	}
	before := p.ownText
	if err := p.maybeWrite(); err != nil {
		t.Fatal(err)
	}
	if len(p.outQ) != 1 || p.outQ[0].Kind != protocol.TypeSubmit {
		t.Fatalf("别行 CC 后本行仍应可写: %v", kinds(p.outQ))
	}
	if p.ownText == before {
		t.Fatal("maybeWrite 应推进本行正文")
	}
}

func TestOtherLineFollowAnswerDoesNotOverwriteOwn(t *testing.T) {
	mine, other := model.NewID(), model.NewID()
	p := newTestPeer("script", mine.Hex(), "本行正文")
	p.formal[other.Hex()] = "别行正文"
	fid := model.NewID()
	p.foreign[fid.Hex()] = model.Dispute{
		ID: fid, RealLine: other, Action: model.ActionEdit,
		Person: "user", Content: []string{"别行候选"},
	}
	p.awaitFollow = true
	ans := protocol.Op{
		ID: model.NewID().Hex(), Kind: protocol.TypeFollowAnswer,
		FollowAnswer: &protocol.FollowAnswer{
			Type: protocol.TypeFollowAnswer, PersonID: "user",
			FromID: "script", DisputeID: fid.Hex(), Accept: true,
		},
	}
	if err := p.onFollowAnswer(ans, nil); err != nil {
		t.Fatal(err)
	}
	if p.ownText != "本行正文" || p.formal[mine.Hex()] != "本行正文" {
		t.Fatalf("别行 FollowAnswer 不得覆盖本行 own=%q formal=%q", p.ownText, p.formal[mine.Hex()])
	}
	if p.followTarget != "" || p.awaitFollow {
		t.Fatalf("不得进入别行追随 follow=%q await=%v", p.followTarget, p.awaitFollow)
	}
	if _, ok := p.foreign[fid.Hex()]; ok {
		t.Fatal("非本行残留 foreign 应清掉")
	}
}

func TestFollowTargetOtherLineCCDoesNotOverwriteOwn(t *testing.T) {
	mine, other := model.NewID(), model.NewID()
	p := newTestPeer("script", mine.Hex(), "本行正文")
	p.followTarget = "user"
	p.attending = false
	cc := protocol.Op{
		ID: model.NewID().Hex(), Kind: protocol.TypeDisputeCC,
		DisputeCC: &protocol.DisputeCC{
			TargetPersonID: "script",
			Claim: model.Dispute{
				ID: model.NewID(), RealLine: other, Action: model.ActionEdit,
				Person: "user", Content: []string{"追随人别行文"},
			},
		},
	}
	if err := p.onDisputeCC(cc, nil); err != nil {
		t.Fatal(err)
	}
	if p.ownText != "本行正文" || p.formal[mine.Hex()] != "本行正文" {
		t.Fatalf("追随中别行 CC 不得写本行 own=%q", p.ownText)
	}
	if len(p.foreign) != 0 || len(p.outQ) != 0 {
		t.Fatalf("别行 CC 不得 foreign/CC 出队 foreign=%d q=%v", len(p.foreign), kinds(p.outQ))
	}
	if !p.seen[cc.ID] {
		t.Fatal("仍应记 seen")
	}
}

// foreign 排在 own 前时，必须先定本人 claimID 再 enqueueOwnCC，避免 NewID 与服务器同槽冲突。
func TestBootstrapForeignBeforeOwnReusesOwnClaimID(t *testing.T) {
	line := model.NewID()
	ownID, fid := model.NewID(), model.NewID()
	p := freshPeer("script")
	var disputeC <-chan time.Time
	boot := protocol.Bootstrap{
		Type: protocol.TypeBootstrap, YourLine: line.Hex(),
		Base: viewOne(line, "我的文"),
		Disputes: []model.Dispute{
			{ID: fid, RealLine: line, Action: model.ActionEdit, Person: "user", Content: []string{"用户文"}},
			{ID: ownID, RealLine: line, Action: model.ActionEdit, Person: "script", Content: []string{"我的文"}},
		},
	}
	if err := p.applyBootstrap(boot, &disputeC); err != nil {
		t.Fatal(err)
	}
	if p.claimID != ownID {
		t.Fatalf("claimID=%s want own %s", p.claimID.Hex(), ownID.Hex())
	}
	if _, self := p.foreign[ownID.Hex()]; self || len(p.foreign) != 1 || p.foreign[fid.Hex()].Person != "user" {
		t.Fatalf("不自争议且仅导入他人: foreign=%+v", p.foreign)
	}
	var ccs []protocol.Op
	for _, op := range p.outQ {
		if op.Kind == protocol.TypeDisputeCC {
			ccs = append(ccs, op)
		}
	}
	if len(ccs) != 1 {
		t.Fatalf("同 target/content 只一份 CC，得 %d queue=%v", len(ccs), kinds(p.outQ))
	}
	if ccs[0].DisputeCC == nil || ccs[0].DisputeCC.Claim.ID != ownID || ccs[0].DisputeCC.TargetPersonID != "user" {
		t.Fatalf("CC 须用 own.ID 指向 foreign: %+v", ccs[0].DisputeCC)
	}
	if p.disputeTimer == nil || disputeC == nil {
		t.Fatal("他人主张仍须 10s 决策")
	}
	p.stopDisputeTimer()
}

func TestReconnectAdoptBaseWhenNoPending(t *testing.T) {
	line := model.NewID()
	p := newTestPeer("script", line.Hex(), "断线前正文")
	p.attending = false
	p.conn = nil
	p.inflightSeq = 0
	boot := protocol.Bootstrap{
		Type: protocol.TypeBootstrap, YourLine: line.Hex(),
		Base: viewOne(line, "服务器新文"),
	}
	if err := p.applyBootstrap(boot, nil); err != nil {
		t.Fatal(err)
	}
	if p.ownText != "服务器新文" {
		t.Fatalf("无未确认编辑应采纳 Base，得 %q", p.ownText)
	}
	if p.attending {
		t.Fatal("重连后注意力须失活")
	}
	if len(p.outQ) != 0 {
		t.Fatalf("不得无端造队: %v", kinds(p.outQ))
	}
}

func TestReconnectKeepOwnTextAndOpIDWhenOutQPending(t *testing.T) {
	line := model.NewID()
	p := newTestPeer("script", line.Hex(), "本机未确认文")
	editID := model.NewID().Hex()
	p.enqueue(protocol.Op{
		ID: editID, Kind: protocol.TypeSubmit,
		Submit: &protocol.Submit{
			Type: protocol.TypeSubmit, PersonID: "script", LineID: line.Hex(),
			Action: model.ActionEdit, Content: []string{"本机未确认文"},
		},
	})
	boot := protocol.Bootstrap{
		Type: protocol.TypeBootstrap, YourLine: line.Hex(),
		Base: viewOne(line, "服务器快照旧文"),
	}
	if err := p.applyBootstrap(boot, nil); err != nil {
		t.Fatal(err)
	}
	if p.ownText != "本机未确认文" {
		t.Fatalf("有 outQ Edit 须保留 ownText，得 %q", p.ownText)
	}
	if len(p.outQ) != 1 || p.outQ[0].ID != editID {
		t.Fatalf("须原 Op.ID 重发: q=%v", p.outQ)
	}
	if p.attending {
		t.Fatal("重连注意力须失活")
	}
}

func newTestPeer(person, line, own string) *peer {
	return &peer{
		personID:    person,
		yourLine:    line,
		ownText:     own,
		formal:      map[string]string{line: own},
		seen:        map[string]bool{},
		foreign:     map[string]model.Dispute{},
		handled:     map[string]bool{},
		answered:    map[string]bool{},
		answerOpID:  map[string]string{},
		confirmedCC: map[string]string{},
		seeded:      own != "",
		booted:      true,
		attending:   own != "", // 已在写的脚本才有注意力
	}
}

func freshPeer(person string) *peer {
	return &peer{
		personID:    person,
		formal:      map[string]string{},
		seen:        map[string]bool{},
		foreign:     map[string]model.Dispute{},
		handled:     map[string]bool{},
		answered:    map[string]bool{},
		answerOpID:  map[string]string{},
		confirmedCC: map[string]string{},
	}
}

func TestFollowAcceptThenPeerEditNoCCUntilOwnWrite(t *testing.T) {
	line := model.NewID()
	p := newTestPeer("script", line.Hex(), "脚本旧")
	p.claimID = model.NewID()
	p.rng = randSrc(1)
	fid := model.NewID()
	p.foreign[fid.Hex()] = model.Dispute{
		ID: fid, RealLine: line, Action: model.ActionEdit,
		Person: "user", Content: []string{"用户文"},
	}
	p.awaitFollow = true

	ans := protocol.Op{
		ID: model.NewID().Hex(), Kind: protocol.TypeFollowAnswer,
		FollowAnswer: &protocol.FollowAnswer{
			Type: protocol.TypeFollowAnswer, PersonID: "user",
			FromID: "script", DisputeID: fid.Hex(), Accept: true,
		},
	}
	if err := p.onFollowAnswer(ans, nil); err != nil {
		t.Fatal(err)
	}
	if p.followTarget != "user" || p.attending || p.awaitFollow {
		t.Fatalf("追随后应被动跟随 follow=%q attending=%v await=%v", p.followTarget, p.attending, p.awaitFollow)
	}
	if p.ownText != "用户文" {
		t.Fatalf("应采纳追随正文，得 %q", p.ownText)
	}

	peerEdit := protocol.Op{
		ID: model.NewID().Hex(), Kind: protocol.TypeSubmit,
		Submit: &protocol.Submit{
			Type: protocol.TypeSubmit, PersonID: "user", LineID: line.Hex(),
			Action: model.ActionEdit, Content: []string{"用户续写"},
		},
	}
	if err := p.onOrdinaryEdit(peerEdit, nil); err != nil {
		t.Fatal(err)
	}
	if len(p.outQ) != 0 {
		t.Fatalf("追随中对方普通 Edit 不得 CC，q=%v", kinds(p.outQ))
	}
	if p.ownText != "用户续写" {
		t.Fatalf("应更新 ownText，得 %q", p.ownText)
	}
	text, _, _ := p.cursorTarget()
	if text != "用户续写" || utf16Len(text) != utf16Len("用户续写") {
		t.Fatalf("光标应在新文行尾 text=%q off=%d", text, utf16Len(text))
	}
	if p.lastCursor == "" || !strings.Contains(p.lastCursor, fmt.Sprintf("%d", utf16Len("用户续写"))) {
		t.Fatalf("应强制刷新光标到行尾 last=%q", p.lastCursor)
	}

	// 对方 CC 更新也采纳，不新开决策
	var disputeC <-chan time.Time
	ccOp := protocol.Op{
		ID: model.NewID().Hex(), Kind: protocol.TypeDisputeCC,
		DisputeCC: &protocol.DisputeCC{
			TargetPersonID: "script",
			Claim: model.Dispute{
				ID: model.NewID(), RealLine: line, Action: model.ActionEdit,
				Person: "user", Content: []string{"用户CC更新"},
			},
		},
	}
	if err := p.onDisputeCC(ccOp, &disputeC); err != nil {
		t.Fatal(err)
	}
	if p.disputeTimer != nil || disputeC != nil {
		t.Fatal("追随中不得新开 10s 决策")
	}
	if p.ownText != "用户CC更新" {
		t.Fatalf("应采纳追随人 CC，得 %q", p.ownText)
	}

	// 自己再写：恢复注意力，可重新主张
	if err := p.sendEdit("脚本再主张"); err != nil {
		t.Fatal(err)
	}
	if p.followTarget != "" || !p.attending {
		t.Fatalf("再写后应恢复注意力 follow=%q attending=%v", p.followTarget, p.attending)
	}
	foreignAgain := protocol.Op{
		ID: model.NewID().Hex(), Kind: protocol.TypeSubmit,
		Submit: &protocol.Submit{
			Type: protocol.TypeSubmit, PersonID: "user", LineID: line.Hex(),
			Action: model.ActionEdit, Content: []string{"用户又改"},
		},
	}
	n := len(p.outQ)
	if err := p.onOrdinaryEdit(foreignAgain, nil); err != nil {
		t.Fatal(err)
	}
	if len(p.outQ) != n+1 || p.outQ[len(p.outQ)-1].Kind != protocol.TypeDisputeCC {
		t.Fatalf("恢复注意力后异文应再 CC，q=%v", kinds(p.outQ))
	}
}

func TestIncomingFollowCancelsRandomDecision(t *testing.T) {
	line := model.NewID()
	p := newTestPeer("script", line.Hex(), "脚本文")
	p.claimID = model.NewID()
	foreign := model.Dispute{ID: model.NewID(), RealLine: line, Action: model.ActionEdit,
		Person: "user", Content: []string{"用户文"}}
	p.foreign[foreign.ID.Hex()] = foreign
	var timer <-chan time.Time
	p.syncDisputeTimer(&timer)
	if p.disputeTimer == nil {
		t.Fatal("应等待随机决定")
	}
	follow := protocol.Op{ID: model.NewID().Hex(), Kind: protocol.TypeFollow,
		Follow: &protocol.Follow{PersonID: "user", DisputeID: p.claimID.Hex(), ClientTs: 1}}
	if err := p.onFollow(follow); err != nil {
		t.Fatal(err)
	}
	if p.disputeTimer != nil || len(p.outQ) != 1 || p.outQ[0].Kind != protocol.TypeFollowAnswer {
		t.Fatalf("收到追随后只应自动答复: timer=%v queue=%v", p.disputeTimer, kinds(p.outQ))
	}
	if err := p.onDisputeTimer(); err != nil {
		t.Fatal(err)
	}
	if len(p.outQ) != 1 {
		t.Fatalf("已取消的随机决定不得再发 Follow: %v", kinds(p.outQ))
	}
	p.onOpAcked(p.outQ[0])
	if len(p.foreign) != 0 {
		t.Fatalf("自动答复确认后应收起对方候选: %+v", p.foreign)
	}
	if err := p.sendEdit("脚本续写"); err != nil || len(p.outQ) != 2 || p.outQ[1].Kind != protocol.TypeSubmit {
		t.Fatalf("收口后恢复普通编辑: queue=%v err=%v", kinds(p.outQ), err)
	}
}

func mustID(hex string) model.ID {
	id, err := model.ParseID(hex)
	if err != nil {
		panic(err)
	}
	return id
}

func kinds(ops []protocol.Op) []string {
	out := make([]string, len(ops))
	for i, op := range ops {
		out[i] = op.Kind
	}
	return out
}

func viewOne(line model.ID, content string) document.View {
	return document.View{
		Article: model.Article{ID: model.NewID(), Title: "测试"},
		Lines:   []model.Line{{ID: line, Content: content}},
	}
}

func randSrc(seed int64) *rand.Rand {
	return rand.New(rand.NewSource(seed))
}
