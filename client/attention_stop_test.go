package main

import (
	"testing"

	"github.com/KDreamelf/collaborative-editor/internal/document"
	"github.com/KDreamelf/collaborative-editor/internal/model"
	"github.com/KDreamelf/collaborative-editor/internal/protocol"
)

// 停笔/离开=注意力消失：其间外来普通 Edit 当普通整合，不外发主张包、不显示争议；
// 再移回光标恢复注意力后，下一笔异文才只发本人 CC。
func TestAttentionStopOrdinaryEditIntegratesNoCC(t *testing.T) {
	line := model.NewID()
	art := model.Article{ID: model.NewID(), Title: "attention-stop"}
	baseLines := []model.Line{{ID: line, Content: "底稿"}}
	doc, err := document.Load(art, baseLines, nil)
	if err != nil {
		t.Fatal(err)
	}

	app := NewAppWithStateDir(t.TempDir())
	app.personID = "甲"
	app.articleID = art.ID.Hex()
	app.doc = doc
	app.relayReady = true
	app.serverSnap = &protocol.Snapshot{Type: protocol.TypeSnapshot, View: document.View{Article: art, Lines: baseLines}}

	lineHex := line.Hex()
	if err := app.MoveCaret(lineHex, "", 0, 0); err != nil {
		t.Fatal(err)
	}
	app.mu.Lock()
	if app.attentionLine != lineHex || !app.attentionActive {
		app.mu.Unlock()
		t.Fatalf("移入后应活跃: line=%q active=%v", app.attentionLine, app.attentionActive)
	}
	app.mu.Unlock()

	if err := app.SubmitEdit(lineHex, "本人已改"); err != nil {
		t.Fatal(err)
	}
	// 模拟本笔已 ACK：清空 queue，避免后续断言被本人 Submit 干扰。
	app.mu.Lock()
	app.queue = nil
	app.sentCount = 0
	app.sentSeq = 0
	app.mu.Unlock()

	if err := app.Suspend(lineHex, model.ActionEdit, true); err != nil {
		t.Fatal(err)
	}
	app.mu.Lock()
	if app.attentionActive {
		app.mu.Unlock()
		t.Fatal("停笔后 attentionActive 须为 false")
	}
	app.mu.Unlock()

	remoteOp := protocol.Op{
		ID:   model.NewID().Hex(),
		Kind: protocol.TypeSubmit,
		Submit: &protocol.Submit{
			Type:     protocol.TypeSubmit,
			PersonID: "乙",
			LineID:   lineHex,
			Action:   model.ActionEdit,
			Content:  []string{"远端新文"},
		},
	}
	app.onRelay(protocol.RelayEvent{Type: protocol.TypeRelay, Op: remoteOp}, app.connGen)

	app.mu.Lock()
	if app.attentionActive {
		app.mu.Unlock()
		t.Fatal("收普通 Edit 后注意力仍须失活")
	}
	for _, op := range app.queue {
		if op.Kind == protocol.TypeDisputeCC {
			app.mu.Unlock()
			t.Fatalf("失活时不得外发 TypeDisputeCC: %+v", op)
		}
	}
	if !app.relaySeenLocked(remoteOp.ID) {
		app.mu.Unlock()
		t.Fatal("须标记已处理 Op.ID")
	}
	app.mu.Unlock()

	v := appView(t, app)
	if len(v.Disputes) != 0 {
		t.Fatalf("View.Disputes 须为 0: %+v", v.Disputes)
	}
	if len(v.Lines) != 1 || v.Lines[0].Content != "远端新文" {
		t.Fatalf("正文须直接成远端新文: %+v", v.Lines)
	}

	// 同 Op.ID 再投一次：幂等，不改状态、不排队 CC。
	app.onRelay(protocol.RelayEvent{Type: protocol.TypeRelay, Op: remoteOp}, app.connGen)
	app.mu.Lock()
	ccCount := 0
	for _, op := range app.queue {
		if op.Kind == protocol.TypeDisputeCC {
			ccCount++
		}
	}
	app.mu.Unlock()
	if ccCount != 0 {
		t.Fatalf("幂等重放后仍不得有 DisputeCC: %d", ccCount)
	}
	v = appView(t, app)
	if len(v.Disputes) != 0 || v.Lines[0].Content != "远端新文" {
		t.Fatalf("幂等后 View 不变: disputes=%+v lines=%+v", v.Disputes, v.Lines)
	}

	// 主动移回该行恢复注意力。
	if err := app.MoveCaret(lineHex, "", 0, 0); err != nil {
		t.Fatal(err)
	}
	app.mu.Lock()
	if !app.attentionActive || app.attentionLine != lineHex {
		app.mu.Unlock()
		t.Fatalf("移回后应再活跃: line=%q active=%v", app.attentionLine, app.attentionActive)
	}
	app.mu.Unlock()

	nextOp := protocol.Op{
		ID:   model.NewID().Hex(),
		Kind: protocol.TypeSubmit,
		Submit: &protocol.Submit{
			Type:     protocol.TypeSubmit,
			PersonID: "乙",
			LineID:   lineHex,
			Action:   model.ActionEdit,
			Content:  []string{"又一异文"},
		},
	}
	app.onRelay(protocol.RelayEvent{Type: protocol.TypeRelay, Op: nextOp}, app.connGen)

	v = appView(t, app)
	if len(v.Disputes) != 0 {
		t.Fatalf("活跃收异文不得显示争议: %+v", v.Disputes)
	}
	if v.Lines[0].Content != "远端新文" {
		t.Fatalf("活跃收异文不得改正文: %q", v.Lines[0].Content)
	}
	app.mu.Lock()
	defer app.mu.Unlock()
	var cc *protocol.DisputeCC
	for i := range app.queue {
		if app.queue[i].Kind == protocol.TypeDisputeCC {
			if cc != nil {
				t.Fatalf("只应一笔本人 CC: %+v", app.queue)
			}
			cc = app.queue[i].DisputeCC
		}
	}
	if cc == nil {
		t.Fatal("恢复注意力后应入队本人 DisputeCC")
	}
	if cc.Claim.Person != "甲" || len(cc.Claim.Content) != 1 || cc.Claim.Content[0] != "远端新文" {
		t.Fatalf("CC 须是本人当前主张正文: %+v", cc)
	}
}
