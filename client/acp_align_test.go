package main

import (
	"slices"
	"testing"

	"github.com/KDreamelf/collaborative-editor/internal/document"
	"github.com/KDreamelf/collaborative-editor/internal/model"
	"github.com/KDreamelf/collaborative-editor/internal/protocol"
)

func TestExeMatchesACPEditAcceptReject(t *testing.T) {
	line := model.NewID()
	art := model.Article{ID: model.NewID(), Title: "align"}
	doc, err := document.Load(art, []model.Line{{ID: line, Content: "甲句"}}, nil)
	if err != nil {
		t.Fatal(err)
	}
	foreignID := model.NewID()
	if err := doc.StoreExplicitClaim(model.Dispute{
		ID: foreignID, RealLine: line, Action: model.ActionEdit,
		Person: "乙", Content: []string{"乙句"}, Followers: []string{},
	}); err != nil {
		t.Fatal(err)
	}

	app := NewAppWithStateDir(t.TempDir())
	app.personID = "甲"
	app.articleID = art.ID.Hex()
	app.doc = doc
	app.relayReady = true

	if err := app.SubmitEdit(line.Hex(), "甲再改"); err != nil {
		t.Fatal(err)
	}
	v := appView(t, app)
	if v.Lines[0].Content != "甲再改" {
		t.Fatalf("普通编辑要改正式行: %q", v.Lines[0].Content)
	}
	if len(v.Disputes) != 1 || v.Disputes[0].Person != "乙" {
		t.Fatalf("编辑不得清掉他人主张: %+v", v.Disputes)
	}
	app.mu.Lock()
	for _, op := range app.queue {
		if op.Kind != protocol.TypeSubmit {
			app.mu.Unlock()
			t.Fatalf("已有主张时编辑仍应是普通提交: %s", op.Kind)
		}
	}
	if !app.lineAttendingLocked(line.Hex()) {
		app.mu.Unlock()
		t.Fatal("编辑后这一行应在写")
	}
	app.queue = nil
	app.mu.Unlock()

	other := protocol.Op{
		ID: model.NewID().Hex(), Kind: protocol.TypeSubmit,
		Submit: &protocol.Submit{
			Type: protocol.TypeSubmit, PersonID: "丙", LineID: line.Hex(),
			Action: model.ActionEdit, Content: []string{"丙句"},
		},
	}
	app.onRelay(protocol.RelayEvent{Type: protocol.TypeRelay, Op: other}, app.connGen)
	v = appView(t, app)
	if v.Lines[0].Content != "甲再改" {
		t.Fatalf("还在写时不得收对方正文: %q", v.Lines[0].Content)
	}
	app.mu.Lock()
	var cc *protocol.DisputeCC
	for i := range app.queue {
		if app.queue[i].Kind == protocol.TypeDisputeCC {
			cc = app.queue[i].DisputeCC
		}
	}
	app.mu.Unlock()
	if cc == nil || cc.TargetPersonID != "丙" || cc.Claim.Content[0] != "甲再改" {
		t.Fatalf("应把本端句子抄送给改这一行的人: %+v", cc)
	}

	if err := app.Suspend(line.Hex(), model.ActionEdit, true); err != nil {
		t.Fatal(err)
	}
	app.mu.Lock()
	app.queue = nil
	app.mu.Unlock()
	idle := protocol.Op{
		ID: model.NewID().Hex(), Kind: protocol.TypeSubmit,
		Submit: &protocol.Submit{
			Type: protocol.TypeSubmit, PersonID: "丙", LineID: line.Hex(),
			Action: model.ActionEdit, Content: []string{"丙后来"},
		},
	}
	app.onRelay(protocol.RelayEvent{Type: protocol.TypeRelay, Op: idle}, app.connGen)
	v = appView(t, app)
	if v.Lines[0].Content != "丙后来" || len(v.Disputes) != 1 {
		t.Fatalf("停笔后应收正文并留主张: line=%q disputes=%+v", v.Lines[0].Content, v.Disputes)
	}
	app.mu.Lock()
	n := len(app.queue)
	app.mu.Unlock()
	if n != 0 {
		t.Fatalf("停笔后不得再发主张: %d", n)
	}

	if err := app.RequestFollow(foreignID.Hex()); err != nil {
		t.Fatal(err)
	}
	v = appView(t, app)
	if v.Lines[0].Content != "丙后来" {
		t.Fatalf("接受不得改正式行: %q", v.Lines[0].Content)
	}
	app.mu.Lock()
	if app.localText[line.Hex()] != "乙句" || app.lineAttendingLocked(line.Hex()) {
		shown, on := app.localText[line.Hex()], app.lineAttendingLocked(line.Hex())
		app.mu.Unlock()
		t.Fatalf("接受后本端显示对方句子并停笔: shown=%q on=%v", shown, on)
	}
	followed := false
	for _, op := range app.queue {
		if op.Kind == protocol.TypeFollow {
			followed = true
		}
	}
	app.mu.Unlock()
	if !followed {
		t.Fatal("接受要发出追随")
	}

	if err := app.RejectClaim(foreignID.Hex()); err != nil {
		t.Fatal(err)
	}
	v = appView(t, app)
	if v.Lines[0].Content != "丙后来" {
		t.Fatalf("拒绝不得改正式行: %q", v.Lines[0].Content)
	}
	app.mu.Lock()
	defer app.mu.Unlock()
	if !app.lineAttendingLocked(line.Hex()) {
		t.Fatal("拒绝后应继续写这一行")
	}
	var rejectCC []string
	for _, op := range app.queue {
		if op.Kind == protocol.TypeDisputeCC && op.DisputeCC != nil && op.DisputeCC.TargetPersonID == "乙" {
			rejectCC = append([]string(nil), op.DisputeCC.Claim.Content...)
		}
	}
	if !slices.Equal(rejectCC, []string{"乙句"}) {
		t.Fatalf("拒绝应把当前本端句子抄送回去: %+v", rejectCC)
	}
}
