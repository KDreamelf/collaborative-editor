package main

import (
	"testing"

	"github.com/KDreamelf/collaborative-editor/internal/document"
	"github.com/KDreamelf/collaborative-editor/internal/model"
	"github.com/KDreamelf/collaborative-editor/internal/protocol"
)

// A 普通 Edit 包到 B；B 无注意力故应用。B 再移光标续写：直接改正文、零争议，
// 发出的 BaseContent 须等于已应用的甲文（本地正式链），不得用陈旧 serverSnap。
func TestRelayLocalBaseEditAfterAppliedOrdinary(t *testing.T) {
	line := model.NewID()
	art := model.Article{ID: model.NewID(), Title: "local-base"}
	baseLines := []model.Line{{ID: line, Content: "底稿"}}

	docA, err := document.Load(art, baseLines, nil)
	if err != nil {
		t.Fatal(err)
	}
	docB, err := document.Load(art, baseLines, nil)
	if err != nil {
		t.Fatal(err)
	}

	appA := NewAppWithStateDir(t.TempDir())
	appA.personID = "甲"
	appA.doc = docA
	appA.relayReady = true
	appA.serverSnap = &protocol.Snapshot{Type: protocol.TypeSnapshot, View: document.View{Article: art, Lines: baseLines}}
	appA.rememberAfterSeenLocked(baseLines)

	appB := NewAppWithStateDir(t.TempDir())
	appB.personID = "乙"
	appB.doc = docB
	appB.relayReady = true
	appB.attentionLine = ""
	appB.attentionActive = false
	appB.serverSnap = &protocol.Snapshot{Type: protocol.TypeSnapshot, View: document.View{Article: art, Lines: baseLines}}
	appB.rememberAfterSeenLocked(baseLines)
	// 故意保留陈旧映射：若仍读 lineBase 会用「底稿」制造争议。
	if appB.lineBase[line.Hex()] != "底稿" {
		t.Fatalf("预置陈旧 lineBase 失败: %+v", appB.lineBase)
	}

	if err := appA.SubmitEdit(line.Hex(), "甲已改"); err != nil {
		t.Fatal(err)
	}
	appA.mu.Lock()
	if len(appA.queue) != 1 || appA.queue[0].Submit == nil {
		appA.mu.Unlock()
		t.Fatalf("甲应入队 Edit: %+v", appA.queue)
	}
	editOp := appA.queue[0]
	appA.mu.Unlock()
	if editOp.Submit.BaseContent == nil || *editOp.Submit.BaseContent != "底稿" {
		t.Fatalf("甲首改 BaseContent 应为本地底稿: %+v", editOp.Submit.BaseContent)
	}

	appB.onRelay(protocol.RelayEvent{Type: protocol.TypeRelay, Op: editOp}, appB.connGen)
	vB := appView(t, appB)
	if len(vB.Lines) != 1 || vB.Lines[0].Content != "甲已改" {
		t.Fatalf("B 无注意力应应用甲文: %+v", vB.Lines)
	}
	if countEditDisputes(vB, line) != 0 {
		t.Fatalf("应用普通包后应零争议: %+v", vB.Disputes)
	}

	if err := appB.MoveCaret(line.Hex(), "", 0, 0); err != nil {
		t.Fatal(err)
	}
	if err := appB.SubmitEdit(line.Hex(), "乙续写"); err != nil {
		t.Fatal(err)
	}
	vB = appView(t, appB)
	if len(vB.Lines) != 1 || vB.Lines[0].Content != "乙续写" {
		t.Fatalf("续写应直接改正文: %+v", vB.Lines)
	}
	if countEditDisputes(vB, line) != 0 {
		t.Fatalf("续写后 View.Disputes 须为 0: %+v", vB.Disputes)
	}

	appB.mu.Lock()
	defer appB.mu.Unlock()
	var got *protocol.Submit
	for i := range appB.queue {
		if appB.queue[i].Kind == protocol.TypeSubmit && appB.queue[i].Submit != nil &&
			appB.queue[i].Submit.Action == model.ActionEdit &&
			len(appB.queue[i].Submit.Content) == 1 && appB.queue[i].Submit.Content[0] == "乙续写" {
			got = appB.queue[i].Submit
			break
		}
	}
	if got == nil {
		t.Fatalf("未找到乙续写包: %+v", appB.queue)
	}
	if got.BaseContent == nil || *got.BaseContent != "甲已改" {
		t.Fatalf("BaseContent 须等于已应用甲文，got=%+v", got.BaseContent)
	}
}

// 中立转发：插在后面的 AfterSeen 取调用前本地 View 后继；文中行与文末行都要正确。
// 陈旧 afterSeen 映射不得污染。
func TestRelayLocalAfterSeenOnInsertHeadAndTail(t *testing.T) {
	head, mid, tail := model.NewID(), model.NewID(), model.NewID()
	art := model.Article{ID: model.NewID(), Title: "after-seen"}
	lines := []model.Line{
		{ID: head, Next: mid, Content: "H"},
		{ID: mid, Prev: head, Next: tail, Content: "M"},
		{ID: tail, Prev: mid, Content: "T"},
	}
	doc, err := document.Load(art, lines, nil)
	if err != nil {
		t.Fatal(err)
	}

	app := NewAppWithStateDir(t.TempDir())
	app.personID = "乙"
	app.doc = doc
	app.relayReady = true
	app.serverSnap = &protocol.Snapshot{Type: protocol.TypeSnapshot, View: document.View{Article: art, Lines: lines}}
	app.rememberAfterSeenLocked(lines)
	// 污染旧映射：若仍读 afterSeen 会错。
	app.afterSeen[head.Hex()] = "dead-head"
	app.afterSeen[tail.Hex()] = "dead-tail"

	if err := app.SubmitInsert(head.Hex(), []string{"X"}); err != nil {
		t.Fatal(err)
	}
	app.mu.Lock()
	s0 := app.queue[0].Submit
	app.mu.Unlock()
	if s0 == nil || s0.AfterSeen == nil || *s0.AfterSeen != mid.Hex() {
		t.Fatalf("文中行 AfterSeen 须为本地当前后继 mid: %+v", s0)
	}

	// 再在刚同步后的文末行尾 Enter：本地后继为空串。
	if err := app.SubmitInsert(tail.Hex(), []string{"Y"}); err != nil {
		t.Fatal(err)
	}
	app.mu.Lock()
	defer app.mu.Unlock()
	var sTail *protocol.Submit
	for i := range app.queue {
		s := app.queue[i].Submit
		if s != nil && s.LineID == tail.Hex() && s.Action == model.ActionInsert {
			sTail = s
		}
	}
	if sTail == nil || sTail.AfterSeen == nil || *sTail.AfterSeen != "" {
		t.Fatalf("文末 AfterSeen 须非 nil 空串: %+v", sTail)
	}
}

// 中立转发跨度基准取本地 View；陈旧 serverSnap / lineBase 不得介入。
func TestRelayLocalSpanBasesFromView(t *testing.T) {
	doc, l1, l2, l3 := loadThreeLineDoc(t)
	v, err := doc.View()
	if err != nil {
		t.Fatal(err)
	}
	// 先把正式链改成与初始 serverSnap 不同，模拟已整合远端。
	if err := doc.SubmitWith("甲", l1, model.ActionEdit, []string{"A1"}, document.SubmitOpts{
		BaseContent: strPtr("L1"),
	}); err != nil {
		t.Fatal(err)
	}
	if err := doc.SubmitWith("甲", l2, model.ActionEdit, []string{"A2"}, document.SubmitOpts{
		BaseContent: strPtr("L2"),
	}); err != nil {
		t.Fatal(err)
	}

	app := NewAppWithStateDir(t.TempDir())
	app.personID = "乙"
	app.doc = doc
	app.relayReady = true
	app.rememberServerSnapLocked(protocol.Snapshot{
		Type: protocol.TypeSnapshot,
		View: document.View{Article: v.Article, Lines: v.Lines}, // 仍是 L1/L2/L3
	})
	app.rememberAfterSeenLocked(v.Lines)
	app.lineBase[l1.Hex()] = "脏"

	if err := app.SubmitSpanEdit(l1.Hex(), l2.Hex(), []string{"X", "Y"}); err != nil {
		t.Fatal(err)
	}
	app.mu.Lock()
	defer app.mu.Unlock()
	se := app.queue[0].SpanEdit
	if se == nil || se.BaseTexts[0] != "A1" || se.BaseTexts[1] != "A2" {
		t.Fatalf("跨度 BaseTexts 须来自本地 View: %+v", se)
	}
	if se.AfterSeen != l3.Hex() {
		t.Fatalf("AfterSeen 须为本地 end.Next: %q", se.AfterSeen)
	}
}
