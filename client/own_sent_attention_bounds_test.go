package main

import (
	"slices"
	"testing"

	"github.com/KDreamelf/collaborative-editor/internal/document"
	"github.com/KDreamelf/collaborative-editor/internal/model"
	"github.com/KDreamelf/collaborative-editor/internal/protocol"
)

// seedOwnSentActive：甲文在正式行、注意力活跃；收一笔普通乙文只入队本人 CC，写入 ownSent。
func seedOwnSentActive(t *testing.T, title, ownBody, peerBody string) (*App, model.ID) {
	t.Helper()
	line := model.NewID()
	art := model.Article{ID: model.NewID(), Title: title}
	doc, err := document.Load(art, []model.Line{{ID: line, Content: "底稿"}}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := doc.SubmitWith("甲", line, model.ActionEdit, []string{ownBody}, document.SubmitOpts{}); err != nil {
		t.Fatal(err)
	}
	app := NewAppWithStateDir(t.TempDir())
	app.personID = "甲"
	app.doc = doc
	app.attentionLine = line.Hex()
	app.attentionActive = true
	app.relayReady = true
	app.onRelay(protocol.RelayEvent{Type: protocol.TypeRelay, Op: protocol.Op{
		ID:   model.NewID().Hex(),
		Kind: protocol.TypeSubmit,
		Submit: &protocol.Submit{
			Type:     protocol.TypeSubmit,
			PersonID: "乙",
			LineID:   line.Hex(),
			Action:   model.ActionEdit,
			Content:  []string{peerBody},
		},
	}}, app.connGen)
	if old := editCCs(app); len(old) != 1 || old[0].Person != "甲" || !slices.Equal(old[0].Content, []string{ownBody}) {
		t.Fatalf("种子后应只有本人 CC %q: %+v", ownBody, old)
	}
	if body := lineBody(t, app); body != ownBody {
		t.Fatalf("注意力活跃时不得被普通异文盖写: %q", body)
	}
	return app, line
}

func foreignEditCC(line model.ID, content string) protocol.Op {
	return protocol.Op{
		ID:   model.NewID().Hex(),
		Kind: protocol.TypeDisputeCC,
		DisputeCC: &protocol.DisputeCC{
			TargetPersonID: "甲",
			Claim: model.Dispute{
				ID:       model.NewID(),
				RealLine: line,
				Action:   model.ActionEdit,
				Person:   "乙",
				Content:  []string{content},
			},
		},
	}
}

func ownEditContents(app *App) [][]string {
	var out [][]string
	for _, cc := range editCCs(app) {
		if cc.Person == "甲" {
			out = append(out, append([]string(nil), cc.Content...))
		}
	}
	return out
}

// TestOwnSentAttentionActiveKeepsTwoCandidates：注意力还在、正文不同 → 两候选，不追随。
func TestOwnSentAttentionActiveKeepsTwoCandidates(t *testing.T) {
	app, line := seedOwnSentActive(t, "bounds-active-two", "甲文", "乙2")
	app.onRelay(protocol.RelayEvent{Type: protocol.TypeRelay, Op: foreignEditCC(line, "乙2")}, app.connGen)

	body, owns, follows, v := lineBody(t, app), ownEditContents(app), followCount(app), appView(t, app)
	t.Logf("body=%q ownCC=%+v follows=%d disputes=%+v", body, owns, follows, v.Disputes)
	if follows != 0 {
		t.Fatalf("注意力还在不得停笔同文追随: follows=%d", follows)
	}
	if countEditDisputes(v, line) != 2 {
		t.Fatalf("应两份候选: %+v", v.Disputes)
	}
	if body != "甲文" {
		t.Fatalf("正式行应仍是甲文: %q", body)
	}
	if len(owns) == 0 || !slices.Equal(owns[len(owns)-1], []string{"甲文"}) {
		t.Fatalf("本人 CC 仍是甲文: %+v", owns)
	}
}

// TestOwnSentAttentionIdleDifferentBodyNoFollow：注意力已消失但正式行≠对方主张 → 不追随，走原路径。
func TestOwnSentAttentionIdleDifferentBodyNoFollow(t *testing.T) {
	app, line := seedOwnSentActive(t, "bounds-idle-diff", "甲文", "乙2")
	if err := app.Suspend(line.Hex(), model.ActionEdit, true); err != nil {
		t.Fatal(err)
	}
	app.mu.Lock()
	active := app.attentionActive
	app.mu.Unlock()
	if active {
		t.Fatal("停笔后注意力须消失")
	}
	if body := lineBody(t, app); body != "甲文" {
		t.Fatalf("停笔后未收普通乙2时正式行应仍是甲文: %q", body)
	}

	app.onRelay(protocol.RelayEvent{Type: protocol.TypeRelay, Op: foreignEditCC(line, "乙2")}, app.connGen)
	body, ccs, follows, v := lineBody(t, app), editCCs(app), followCount(app), appView(t, app)
	t.Logf("body=%q ccs=%+v follows=%d disputes=%+v", body, ccs, follows, v.Disputes)
	if follows != 0 {
		t.Fatalf("正式行不同于对方主张时不得自动追随: follows=%d queueCC=%+v", follows, ccs)
	}
	if countEditDisputes(v, line) != 2 {
		t.Fatalf("应保留原路径两候选: body=%q disputes=%+v", body, v.Disputes)
	}
}

// TestOwnSentAttentionActiveUpdatesOwnCCFromBelief：发出主张后又改这行（注意力仍在）→ 外来主张用新正文更新本人 CC。
func TestOwnSentAttentionActiveUpdatesOwnCCFromBelief(t *testing.T) {
	app, line := seedOwnSentActive(t, "bounds-active-update", "甲文", "乙2")
	if err := app.SubmitEdit(line.Hex(), "甲新"); err != nil {
		t.Fatal(err)
	}
	if body := lineBody(t, app); body != "甲新" {
		t.Fatalf("续写后正式行应是甲新: %q", body)
	}
	app.mu.Lock()
	sent, ok := app.ownSent[claimSlotKey(line, model.ActionEdit)]
	app.mu.Unlock()
	if !ok || !slices.Equal(sent.Content, []string{"甲新"}) {
		t.Fatalf("续写后 ownSent 应跟着正式行: ok=%v sent=%+v", ok, sent)
	}

	app.onRelay(protocol.RelayEvent{Type: protocol.TypeRelay, Op: foreignEditCC(line, "乙2")}, app.connGen)
	owns, follows, v := ownEditContents(app), followCount(app), appView(t, app)
	t.Logf("body=%q ownCC=%+v follows=%d disputes=%+v", lineBody(t, app), owns, follows, v.Disputes)
	if follows != 0 {
		t.Fatalf("注意力还在不得追随: follows=%d", follows)
	}
	if len(owns) != 1 || !slices.Equal(owns[0], []string{"甲文"}) {
		t.Fatalf("续写不再补发主张包: %+v", owns)
	}
	if countEditDisputes(v, line) != 2 {
		t.Fatalf("应两候选: %+v", v.Disputes)
	}
	var ownDisp *model.Dispute
	for i := range v.Disputes {
		d := &v.Disputes[i]
		if d.Person == "甲" && d.RealLine == line && d.Action == model.ActionEdit {
			ownDisp = d
			break
		}
	}
	if ownDisp == nil || !slices.Equal(ownDisp.Content, []string{"甲新"}) {
		t.Fatalf("争议里本人候选须是甲新: %+v", v.Disputes)
	}
}
