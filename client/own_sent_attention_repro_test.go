package main

import (
	"slices"
	"strings"
	"testing"

	"github.com/KDreamelf/collaborative-editor/internal/document"
	"github.com/KDreamelf/collaborative-editor/internal/model"
	"github.com/KDreamelf/collaborative-editor/internal/protocol"
)

// TestOwnSentAttentionRepro：发送方只有乙，正文只有乙2。
// 甲还在这行时，第一笔普通乙2不整合，只入队甲已经在写的旧 CC「甲文」。
// 甲停笔等于退出该行。此后新的一笔普通乙2覆盖正文，显式 CC 的 Content 也是乙2。
// 停笔后不得再发本人 DisputeCC，正文不得被旧主张甲文盖回。同一包再收一次，状态不变。
func TestOwnSentAttentionRepro(t *testing.T) {
	line := model.NewID()
	art := model.Article{ID: model.NewID(), Title: "own-sent-attention"}
	doc, err := document.Load(art, []model.Line{{ID: line, Content: "底稿"}}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := doc.SubmitWith("甲", line, model.ActionEdit, []string{"甲文"}, document.SubmitOpts{}); err != nil {
		t.Fatal(err)
	}

	app := NewAppWithStateDir(t.TempDir())
	app.personID = "甲"
	app.doc = doc
	app.attentionLine = line.Hex()
	app.attentionActive = true
	app.relayReady = true

	// 停笔前。这一笔已被本端看见，所以停笔后不会重放。
	seenWhileActive := protocol.Op{
		ID:   model.NewID().Hex(),
		Kind: protocol.TypeSubmit,
		Submit: &protocol.Submit{
			Type:     protocol.TypeSubmit,
			PersonID: "乙",
			LineID:   line.Hex(),
			Action:   model.ActionEdit,
			Content:  []string{"乙2"},
		},
	}
	app.onRelay(protocol.RelayEvent{Type: protocol.TypeRelay, Op: seenWhileActive}, app.connGen)

	old := editCCs(app)
	if len(old) != 1 || old[0].Person != "甲" || !slices.Equal(old[0].Content, []string{"甲文"}) {
		t.Fatalf("停笔前应只有已发出的旧 CC 甲文: %+v", old)
	}
	if body := appView(t, app).Lines[0].Content; body != "甲文" {
		t.Fatalf("还在编辑时不得用乙2覆盖甲文: %q", body)
	}

	if err := app.Suspend(line.Hex(), model.ActionEdit, true); err != nil {
		t.Fatal(err)
	}
	app.mu.Lock()
	active := app.attentionActive
	app.mu.Unlock()
	if active {
		t.Fatal("停笔后注意力须消失")
	}

	ordinary := protocol.Op{
		ID:   model.NewID().Hex(),
		Kind: protocol.TypeSubmit,
		Submit: &protocol.Submit{
			Type:     protocol.TypeSubmit,
			PersonID: "乙",
			LineID:   line.Hex(),
			Action:   model.ActionEdit,
			Content:  []string{"乙2"},
		},
	}
	app.onRelay(protocol.RelayEvent{Type: protocol.TypeRelay, Op: ordinary}, app.connGen)
	if body, ccs := lineBody(t, app), editCCs(app); body != "乙2" || len(ccs) != 1 || slices.Equal(ccs[0].Content, []string{"乙2"}) {
		t.Fatalf("停笔后普通乙2应覆盖正文且不得新发或改写成乙2: body=%q ccs=%+v", body, ccs)
	}

	foreignID := model.NewID()
	foreignCC := protocol.Op{
		ID:   model.NewID().Hex(),
		Kind: protocol.TypeDisputeCC,
		DisputeCC: &protocol.DisputeCC{
			TargetPersonID: "甲",
			Claim: model.Dispute{
				ID:       foreignID,
				RealLine: line,
				Action:   model.ActionEdit,
				Person:   "乙",
				Content:  []string{"乙2"},
			},
		},
	}
	app.onRelay(protocol.RelayEvent{Type: protocol.TypeRelay, Op: foreignCC}, app.connGen)
	ccs := editCCs(app)
	body := lineBody(t, app)
	if len(ccs) != 1 {
		t.Fatalf("收到显式 CC 后不得新发本人 DisputeCC: %+v", ccs)
	}
	if ccs[0].Person == "甲" && slices.Equal(ccs[0].Content, []string{"乙2"}) {
		t.Fatalf("无注意力不得以自己名义外发乙2: %+v", ccs)
	}
	if body != "乙2" {
		t.Fatalf("旧主张不得把正文从乙2盖回: %q", body)
	}
	followN, _ := followOps(app)
	if followN != 0 {
		t.Fatalf("同文只登记主张，不自动追随: n=%d", followN)
	}
	foundForeign := false
	for _, d := range disputeSnap(t, app) {
		if strings.HasPrefix(d, "乙\x00") {
			foundForeign = true
		}
	}
	if !foundForeign {
		t.Fatal("应登记乙的主张")
	}
	app.mu.Lock()
	sent, sentOK := app.ownSent[claimSlotKey(line, model.ActionEdit)]
	app.mu.Unlock()
	if !sentOK || !slices.Equal(sent.Content, []string{"甲文"}) {
		t.Fatalf("确认前应保留旧 ownSent: ok=%v sent=%+v", sentOK, sent)
	}
	disputes := disputeSnap(t, app)

	app.onRelay(protocol.RelayEvent{Type: protocol.TypeRelay, Op: ordinary}, app.connGen)
	app.onRelay(protocol.RelayEvent{Type: protocol.TypeRelay, Op: foreignCC}, app.connGen)
	if again, againCC, againDisp, againFollow := lineBody(t, app), editCCs(app), disputeSnap(t, app), followCount(app); again != body || len(againCC) != len(ccs) || !slices.Equal(againDisp, disputes) || againFollow != 0 {
		t.Fatalf("重复收包须幂等: body=%q ccs=%+v disputes=%v follows=%d", again, againCC, againDisp, againFollow)
	}

	answer := protocol.Op{
		ID:   model.NewID().Hex(),
		Kind: protocol.TypeFollowAnswer,
		FollowAnswer: &protocol.FollowAnswer{
			Type:      protocol.TypeFollowAnswer,
			PersonID:  "乙",
			FromID:    "甲",
			DisputeID: foreignID.Hex(),
			Accept:    true,
		},
	}
	app.onRelay(protocol.RelayEvent{Type: protocol.TypeRelay, Op: answer}, app.connGen)
	if got := lineBody(t, app); got != "乙2" {
		t.Fatalf("没有追随时确认不得改正文: %q", got)
	}
	if after := editCCs(app); len(after) != 1 || slices.Equal(after[0].Content, []string{"乙2"}) {
		t.Fatalf("确认后不得新发本人 CC: %+v", after)
	}
	app.mu.Lock()
	_, still := app.ownSent[claimSlotKey(line, model.ActionEdit)]
	app.mu.Unlock()
	if !still {
		t.Fatal("没追随过，确认不应丢掉已抄送的旧主张")
	}
	app.onRelay(protocol.RelayEvent{Type: protocol.TypeRelay, Op: answer}, app.connGen)
	if lineBody(t, app) != "乙2" || len(editCCs(app)) != 1 {
		t.Fatal("重复确认须幂等")
	}
}

func lineBody(t *testing.T, app *App) string {
	t.Helper()
	v := appView(t, app)
	if len(v.Lines) != 1 {
		t.Fatalf("应保持一行: %+v", v.Lines)
	}
	return v.Lines[0].Content
}

func disputeSnap(t *testing.T, app *App) []string {
	t.Helper()
	v := appView(t, app)
	out := make([]string, 0, len(v.Disputes))
	for _, d := range v.Disputes {
		out = append(out, d.Person+"\x00"+d.Action+"\x00"+strings.Join(d.Content, "\x1e"))
	}
	slices.Sort(out)
	return out
}

func followCount(app *App) int {
	n, _ := followOps(app)
	return n
}

func followOps(app *App) (int, protocol.Follow) {
	app.mu.Lock()
	defer app.mu.Unlock()
	var one protocol.Follow
	n := 0
	for _, op := range app.queue {
		if op.Kind == protocol.TypeFollow && op.Follow != nil {
			n++
			one = *op.Follow
		}
	}
	return n, one
}

func foreignPending(t *testing.T, app *App, id model.ID) model.PendingConfirm {
	t.Helper()
	v := appView(t, app)
	for _, d := range v.Disputes {
		if d.ID != id {
			continue
		}
		if d.Person != "乙" || !slices.Equal(d.Content, []string{"乙2"}) || len(d.Followers) != 0 || len(d.Pending) != 1 {
			t.Fatalf("对方主张应待确认且无人已追随: %+v", d)
		}
		return d.Pending[0]
	}
	t.Fatalf("缺少对方主张 %s: %+v", id.Hex(), v.Disputes)
	return model.PendingConfirm{}
}

func editCCs(app *App) []model.Dispute {
	app.mu.Lock()
	defer app.mu.Unlock()
	var out []model.Dispute
	for _, op := range app.queue {
		if op.Kind == protocol.TypeDisputeCC && op.DisputeCC != nil && op.DisputeCC.Claim.Action == model.ActionEdit {
			out = append(out, op.DisputeCC.Claim)
		}
	}
	return out
}
