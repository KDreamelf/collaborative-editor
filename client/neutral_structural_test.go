package main

import (
	"testing"

	"github.com/KDreamelf/collaborative-editor/internal/document"
	"github.com/KDreamelf/collaborative-editor/internal/model"
	"github.com/KDreamelf/collaborative-editor/internal/protocol"
)

func TestNeutralLocalStructuralEditsIgnoreOtherCursor(t *testing.T) {
	first, second := model.NewID(), model.NewID()
	base := model.Article{ID: model.NewID(), Title: "t"}
	lines := []model.Line{{ID: first, Next: second, Content: "甲"}, {ID: second, Prev: first}}
	newApp := func() *App {
		d, err := document.Load(base, lines, nil)
		if err != nil {
			t.Fatal(err)
		}
		a := NewAppWithStateDir(t.TempDir())
		a.doc, a.personID, a.relayBooted = d, "我", true
		a.cursors = []protocol.Cursor{{PersonID: "别人", LineID: second.Hex()}}
		return a
	}

	a := newApp()
	if err := a.DeleteLine(second.Hex()); err != nil {
		t.Fatal(err)
	}
	v, err := a.doc.View()
	if err != nil || len(v.Lines) != 1 || len(v.Disputes) != 0 || len(a.queue) != 1 || a.queue[0].Kind != protocol.TypeDelete {
		t.Fatalf("删空行应为普通操作: view=%+v queue=%+v err=%v", v, a.queue, err)
	}

	b := newApp()
	if err := b.MergeUp(second.Hex()); err != nil {
		t.Fatal(err)
	}
	v, err = b.doc.View()
	if err != nil || len(v.Lines) != 1 || v.Lines[0].Content != "甲" || len(v.Disputes) != 0 || len(b.queue) != 1 || b.queue[0].Kind != protocol.TypeMerge {
		t.Fatalf("合并应为普通操作: view=%+v queue=%+v err=%v", v, b.queue, err)
	}
}

func TestNeutralLocalDeleteAfterForeignClaimSendsClaim(t *testing.T) {
	first, second := model.NewID(), model.NewID()
	d, err := document.Load(model.Article{ID: model.NewID(), Title: "t"}, []model.Line{
		{ID: first, Next: second}, {ID: second, Prev: first},
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	foreign := model.Dispute{ID: model.NewID(), RealLine: second, Action: model.ActionEdit, Person: "别人", Content: []string{"保留"}}
	if err := d.ReceiveForeignEditClaim("我", foreign, model.NewID()); err != nil {
		t.Fatal(err)
	}
	a := NewAppWithStateDir(t.TempDir())
	a.doc, a.personID, a.relayBooted = d, "我", true
	if err := a.DeleteLine(second.Hex()); err != nil {
		t.Fatal(err)
	}
	v, err := a.doc.View()
	if err != nil || len(v.Lines) != 2 || len(a.queue) != 1 || a.queue[0].Kind != protocol.TypeDisputeCC || a.queue[0].DisputeCC.Claim.Action != model.ActionDelete {
		t.Fatalf("收到外来主张后删行应只抄送本人主张: view=%+v queue=%+v err=%v", v, a.queue, err)
	}
}

func TestIncomingDeleteOnAttentionSendsClaimWithoutLosingLine(t *testing.T) {
	first, second := model.NewID(), model.NewID()
	d, err := document.Load(model.Article{ID: model.NewID(), Title: "t"}, []model.Line{
		{ID: first, Next: second, Content: "上一行"}, {ID: second, Prev: first, Content: "我的文字"},
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	a := NewAppWithStateDir(t.TempDir())
	a.doc, a.personID, a.relayBooted = d, "我", true
	a.attentionLine, a.attentionActive = second.Hex(), true
	op := protocol.Op{ID: model.NewID().Hex(), Kind: protocol.TypeDelete,
		Delete: &protocol.Delete{PersonID: "别人", LineID: second.Hex()}}
	if err := a.applyRelayOpLocked(op); err != nil {
		t.Fatal(err)
	}
	v, err := a.doc.View()
	if err != nil || len(v.Lines) != 2 || v.Lines[1].Content != "我的文字" || len(v.Disputes) != 0 {
		t.Fatalf("自己仍应相信原行: view=%+v err=%v", v, err)
	}
	if len(a.queue) != 1 || a.queue[0].Kind != protocol.TypeDisputeCC {
		t.Fatalf("应向删行者提出本人主张: %+v", a.queue)
	}
	claim := a.queue[0].DisputeCC.Claim
	if claim.RealLine != first || claim.Action != model.ActionInsert || len(claim.Content) != 1 || claim.Content[0] != "我的文字" {
		t.Fatalf("被删行已不在服务器，主张应挂在存活的上一行: %+v", claim)
	}
	bDoc, err := document.Load(d.Article(), []model.Line{{ID: first, Content: "上一行"}}, nil)
	if err != nil {
		t.Fatal(err)
	}
	b := NewAppWithStateDir(t.TempDir())
	b.doc, b.personID, b.relayBooted = bDoc, "别人", true
	if err := b.applyRelayOpLocked(a.queue[0]); err != nil {
		t.Fatal(err)
	}
	if len(b.queue) != 0 {
		t.Fatalf("收到主张后不应再回一包: %+v", b.queue)
	}
	v = appView(t, b)
	if len(v.Disputes) != 1 || v.Disputes[0].Person != "我" {
		t.Fatalf("只登记对方主张: %+v", v.Disputes)
	}
}

func TestForeignClaimReturnsOwnClaimOnce(t *testing.T) {
	line := model.NewID()
	article := model.Article{ID: model.NewID(), Title: "t"}
	base := []model.Line{{ID: line, Content: "底稿"}}
	newApp := func(person, text string) *App {
		d, err := document.Load(article, base, nil)
		if err != nil {
			t.Fatal(err)
		}
		if err := d.SubmitWith(person, line, model.ActionEdit, []string{text}, document.SubmitOpts{}); err != nil {
			t.Fatal(err)
		}
		a := NewAppWithStateDir(t.TempDir())
		a.doc, a.personID, a.relayBooted = d, person, true
		a.attentionLine, a.attentionActive = line.Hex(), true
		return a
	}
	a, b := newApp("甲", "甲文"), newApp("乙", "乙文")
	incoming := protocol.Op{ID: model.NewID().Hex(), Kind: protocol.TypeSubmit,
		Submit: &protocol.Submit{PersonID: "乙", LineID: line.Hex(), Action: model.ActionEdit, Content: []string{"乙文"}}}
	if err := a.applyRelayOpLocked(incoming); err != nil {
		t.Fatal(err)
	}
	if len(a.queue) != 1 || len(appView(t, a).Disputes) != 0 {
		t.Fatalf("甲先发主张，本地仍不显示争议: %+v", a.queue)
	}
	ccA := a.queue[0]
	if err := b.applyRelayOpLocked(ccA); err != nil {
		t.Fatal(err)
	}
	if len(b.queue) != 0 || len(appView(t, b).Disputes) != 1 || appView(t, b).Disputes[0].Person != "甲" {
		t.Fatalf("乙只登记甲的主张，不回包: queue=%+v disputes=%+v", b.queue, appView(t, b).Disputes)
	}
	if err := b.SubmitEditClaim(line.Hex(), []string{"乙改过的主张"}); err != nil {
		t.Fatal(err)
	}
	last := b.queue[len(b.queue)-1]
	if last.Kind != protocol.TypeSubmit || last.Submit == nil || last.Submit.Content[0] != "乙改过的主张" {
		t.Fatalf("再编辑应是普通提交: %+v", b.queue)
	}
	if appView(t, b).Lines[0].Content != "乙改过的主张" {
		t.Fatalf("正式行应更新: %+v", appView(t, b).Lines)
	}
}

func TestOwnEditAfterSendingClaimRemainsLatestOnForeignArrival(t *testing.T) {
	line := model.NewID()
	d, err := document.Load(model.Article{ID: model.NewID(), Title: "t"}, []model.Line{{ID: line, Content: "底稿"}}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := d.SubmitWith("甲", line, model.ActionEdit, []string{"甲旧"}, document.SubmitOpts{}); err != nil {
		t.Fatal(err)
	}
	a := NewAppWithStateDir(t.TempDir())
	a.doc, a.personID, a.relayBooted = d, "甲", true
	a.attentionLine, a.attentionActive = line.Hex(), true
	if err := a.applyRelayOpLocked(protocol.Op{ID: model.NewID().Hex(), Kind: protocol.TypeSubmit,
		Submit: &protocol.Submit{PersonID: "乙", LineID: line.Hex(), Action: model.ActionEdit, Content: []string{"乙文"}}}); err != nil {
		t.Fatal(err)
	}
	if err := a.SubmitEdit(line.Hex(), "甲新"); err != nil {
		t.Fatal(err)
	}
	foreign := protocol.Op{ID: model.NewID().Hex(), Kind: protocol.TypeDisputeCC,
		DisputeCC: &protocol.DisputeCC{TargetPersonID: "甲", Claim: model.Dispute{
			ID: model.NewID(), RealLine: line, Action: model.ActionEdit, Person: "乙", Content: []string{"乙文"},
		}}}
	if err := a.applyRelayOpLocked(foreign); err != nil {
		t.Fatal(err)
	}
	v := appView(t, a)
	if v.Lines[0].Content != "甲新" {
		t.Fatalf("正式行应是甲新: %+v", v.Lines)
	}
	for _, claim := range v.Disputes {
		if claim.Person == "甲" && (len(claim.Content) != 1 || claim.Content[0] != "甲新") {
			t.Fatalf("留下的本人主张应是最新正文: %+v", claim)
		}
	}
	last := a.queue[len(a.queue)-1]
	if last.Kind != protocol.TypeSubmit || last.Submit == nil || last.Submit.Content[0] != "甲新" {
		t.Fatalf("最新文字走普通提交: %+v", a.queue)
	}
}

func TestSpanAcrossDisputedFirstLineOnlySendsOwnClaim(t *testing.T) {
	first, second := model.NewID(), model.NewID()
	d, err := document.Load(model.Article{ID: model.NewID(), Title: "t"}, []model.Line{
		{ID: first, Next: second, Content: "第一行"}, {ID: second, Prev: first, Content: "第二行"},
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	foreign := model.Dispute{ID: model.NewID(), RealLine: first, Action: model.ActionEdit,
		Person: "别人", Content: []string{"别人的第一行"}}
	if err := d.ReceiveForeignEditClaim("我", foreign, model.NewID()); err != nil {
		t.Fatal(err)
	}
	a := NewAppWithStateDir(t.TempDir())
	a.doc, a.personID, a.relayBooted = d, "我", true
	if err := a.SubmitSpanEdit(first.Hex(), second.Hex(), []string{"我改的整段"}); err != nil {
		t.Fatal(err)
	}
	if len(a.queue) != 1 || a.queue[0].Kind != protocol.TypeDisputeCC ||
		len(a.queue[0].DisputeCC.Claim.Content) != 1 || a.queue[0].DisputeCC.Claim.Content[0] != "我改的整段" {
		t.Fatalf("争议内整段替换应只更新本人候选: %+v", a.queue)
	}
}
