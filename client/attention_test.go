package main

import (
	"encoding/json"
	"slices"
	"testing"

	"github.com/KDreamelf/collaborative-editor/internal/document"
	"github.com/KDreamelf/collaborative-editor/internal/model"
	"github.com/KDreamelf/collaborative-editor/internal/protocol"
)

func loadDoc(t *testing.T, lines []model.Line, disputes []model.Dispute) *document.Doc {
	t.Helper()
	d, err := document.Load(model.Article{ID: model.NewID(), Title: "t"}, lines, disputes)
	if err != nil {
		t.Fatal(err)
	}
	return d
}

func docSnap(t *testing.T, d *document.Doc) []byte {
	t.Helper()
	v, err := d.View()
	if err != nil {
		t.Fatal(err)
	}
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func TestOwnEditCC(t *testing.T) {
	line := model.NewID()
	other := model.NewID()
	claimID := model.NewID()
	self := "甲"
	peer := "乙"

	baseLines := []model.Line{{ID: line, Content: "我的正文"}}

	edit := func(person, lineHex string, content ...string) protocol.Op {
		return protocol.Op{
			Kind: protocol.TypeSubmit,
			Submit: &protocol.Submit{
				Type:     protocol.TypeSubmit,
				PersonID: person,
				LineID:   lineHex,
				Action:   model.ActionEdit,
				Content:  content,
			},
		}
	}

	tests := []struct {
		name        string
		lines       []model.Line
		disputes    []model.Dispute
		attention   string
		active      bool
		incoming    protocol.Op
		wantNil     bool
		wantErr     bool
		wantTarget  string
		wantContent []string
	}{
		{
			name:      "自己Op",
			lines:     baseLines,
			attention: line.Hex(),
			active:    true,
			incoming:  edit(self, line.Hex(), "对方改"),
			wantNil:   true,
		},
		{
			name:      "非活跃",
			lines:     baseLines,
			attention: line.Hex(),
			active:    false,
			incoming:  edit(peer, line.Hex(), "对方改"),
			wantNil:   true,
		},
		{
			name:      "注意别行",
			lines:     baseLines,
			attention: other.Hex(),
			active:    true,
			incoming:  edit(peer, line.Hex(), "对方改"),
			wantNil:   true,
		},
		{
			name:      "相同文本",
			lines:     baseLines,
			attention: line.Hex(),
			active:    true,
			incoming:  edit(peer, line.Hex(), "我的正文"),
			wantNil:   true,
		},
		{
			name:        "不同文本",
			lines:       baseLines,
			attention:   line.Hex(),
			active:      true,
			incoming:    edit(peer, line.Hex(), "对方改"),
			wantTarget:  peer,
			wantContent: []string{"我的正文"},
		},
		{
			name:  "已有本人的候选",
			lines: []model.Line{{ID: line, Content: "正式旧"}},
			disputes: []model.Dispute{{
				ID:       model.NewID(),
				RealLine: line,
				Action:   model.ActionEdit,
				Person:   self,
				Content:  []string{"我的候选"},
			}},
			attention:   line.Hex(),
			active:      true,
			incoming:    edit(peer, line.Hex(), "对方改"),
			wantTarget:  peer,
			wantContent: []string{"我的候选"},
		},
		{
			name:  "多行粘贴相同",
			lines: []model.Line{{ID: line, Content: "正式旧"}},
			disputes: []model.Dispute{{
				ID:       model.NewID(),
				RealLine: line,
				Action:   model.ActionEdit,
				Person:   self,
				Content:  []string{"A", "B"},
			}},
			attention: line.Hex(),
			active:    true,
			incoming:  edit(peer, line.Hex(), "A", "B"),
			wantNil:   true,
		},
		{
			name:  "多行粘贴不同",
			lines: []model.Line{{ID: line, Content: "正式旧"}},
			disputes: []model.Dispute{{
				ID:       model.NewID(),
				RealLine: line,
				Action:   model.ActionEdit,
				Person:   self,
				Content:  []string{"A", "B"},
			}},
			attention:   line.Hex(),
			active:      true,
			incoming:    edit(peer, line.Hex(), "X", "Y"),
			wantTarget:  peer,
			wantContent: []string{"A", "B"},
		},
		{
			name:      "其他Kind",
			lines:     baseLines,
			attention: line.Hex(),
			active:    true,
			incoming:  protocol.Op{Kind: protocol.TypeDelete},
			wantNil:   true,
		},
		{
			name:      "无效行ID",
			lines:     baseLines,
			attention: "not-an-objectid",
			active:    true,
			incoming:  edit(peer, "not-an-objectid", "x"),
			wantErr:   true,
		},
		{
			name: "缺少本地行",
			lines: []model.Line{{ID: other, Content: "别行"}},
			attention: line.Hex(),
			active:    true,
			incoming:  edit(peer, line.Hex(), "对方改"),
			wantErr:   true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			d := loadDoc(t, tt.lines, tt.disputes)
			before := docSnap(t, d)
			got, err := ownEditCC(d, self, tt.attention, tt.active, tt.incoming, claimID)
			after := docSnap(t, d)
			if !slices.Equal(before, after) {
				t.Fatalf("Doc 被修改")
			}
			if tt.wantErr {
				if err == nil {
					t.Fatal("期望 error")
				}
				if got != nil {
					t.Fatalf("error 时不应返回包: %+v", got)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if tt.wantNil {
				if got != nil {
					t.Fatalf("期望 nil, got %+v", got)
				}
				return
			}
			if got == nil {
				t.Fatal("期望 DisputeCC")
			}
			if got.TargetPersonID != tt.wantTarget {
				t.Fatalf("target=%q want %q", got.TargetPersonID, tt.wantTarget)
			}
			if got.Claim.ID != claimID || got.Claim.Person != self || got.Claim.Action != model.ActionEdit {
				t.Fatalf("claim meta %+v", got.Claim)
			}
			if got.Claim.RealLine != line {
				t.Fatalf("realLine=%s want %s", got.Claim.RealLine.Hex(), line.Hex())
			}
			if !slices.Equal(got.Claim.Content, tt.wantContent) {
				t.Fatalf("content=%v want %v", got.Claim.Content, tt.wantContent)
			}
		})
	}
}

func TestOwnEditCCDocUnchanged(t *testing.T) {
	line := model.NewID()
	claimID := model.NewID()
	d := loadDoc(t, []model.Line{{ID: line, Content: "正文"}}, []model.Dispute{{
		ID:       model.NewID(),
		RealLine: line,
		Action:   model.ActionEdit,
		Person:   "甲",
		Content:  []string{"候选"},
	}})
	before := docSnap(t, d)
	_, err := ownEditCC(d, "甲", line.Hex(), true, protocol.Op{
		Kind: protocol.TypeSubmit,
		Submit: &protocol.Submit{
			Type:     protocol.TypeSubmit,
			PersonID: "乙",
			LineID:   line.Hex(),
			Action:   model.ActionEdit,
			Content:  []string{"对方"},
		},
	}, claimID)
	if err != nil {
		t.Fatal(err)
	}
	after := docSnap(t, d)
	if !slices.Equal(before, after) {
		t.Fatal("Doc 被修改")
	}
	v, err := d.View()
	if err != nil {
		t.Fatal(err)
	}
	if v.Disputes[0].Content[0] != "候选" || v.Lines[0].Content != "正文" {
		t.Fatal("Doc 字段被改写")
	}
}

func TestOwnEditCCLiveMultilinePaste(t *testing.T) {
	d := document.New("t")
	line := mustView(t, d).Lines[0].ID
	tail := model.NewID()
	if err := d.SubmitWith("甲", line, model.ActionEdit, []string{"hello", "world"}, document.SubmitOpts{
		LineIDs: []model.ID{tail},
	}); err != nil {
		t.Fatal(err)
	}
	v := mustView(t, d)
	if len(v.Disputes) != 0 {
		t.Fatalf("粘贴后尚无 Dispute: %+v", v.Disputes)
	}
	if len(v.Lines) < 2 || v.Lines[0].Content != "hello" || v.Lines[1].Content != "world" {
		t.Fatalf("正式链应已拆段: %+v", v.Lines)
	}
	before := docSnap(t, d)
	claimID := model.NewID()
	got, err := ownEditCC(d, "甲", line.Hex(), true, protocol.Op{
		Kind: protocol.TypeSubmit,
		Submit: &protocol.Submit{
			Type:     protocol.TypeSubmit,
			PersonID: "乙",
			LineID:   line.Hex(),
			Action:   model.ActionEdit,
			Content:  []string{"对方"},
		},
	}, claimID)
	if err != nil {
		t.Fatal(err)
	}
	after := docSnap(t, d)
	if !slices.Equal(before, after) {
		t.Fatal("Doc 被修改")
	}
	if got == nil {
		t.Fatal("期望 DisputeCC")
	}
	if !slices.Equal(got.Claim.Content, []string{"hello", "world"}) {
		t.Fatalf("CC.Content 应是 live 全段, got %v", got.Claim.Content)
	}
	if len(mustView(t, d).Disputes) != 0 {
		t.Fatal("ownEditCC 不得写入 Dispute")
	}
}

func mustView(t *testing.T, d *document.Doc) document.View {
	t.Helper()
	v, err := d.View()
	if err != nil {
		t.Fatal(err)
	}
	return v
}

func TestSuspendWithoutForeignOnlyTouchesAttention(t *testing.T) {
	doc := document.New("t")
	line := mustView(t, doc).Lines[0].ID.Hex()
	app := NewAppWithStateDir(t.TempDir())
	app.personID = "甲"
	app.articleID = "art"
	app.doc = doc

	if err := app.SubmitEdit(line, "改后正文"); err != nil {
		t.Fatal(err)
	}
	before := mustView(t, app.doc)
	if before.Lines[0].Content != "改后正文" {
		t.Fatalf("正文未更新: %q", before.Lines[0].Content)
	}
	if len(before.Disputes) != 0 {
		t.Fatalf("编辑后不应有 Dispute: %+v", before.Disputes)
	}

	if err := app.MoveCaretRange(line, "", 0, 0, "", "", 0, 0); err != nil {
		t.Fatal(err)
	}
	app.mu.Lock()
	if app.attentionLine != line || !app.attentionActive {
		app.mu.Unlock()
		t.Fatalf("正式行应激活注意力: line=%q active=%v", app.attentionLine, app.attentionActive)
	}
	qBefore := len(app.queue)
	app.mu.Unlock()

	if err := app.Suspend(line, model.ActionEdit, true); err != nil {
		t.Fatal(err)
	}
	app.mu.Lock()
	if app.attentionActive {
		app.mu.Unlock()
		t.Fatal("挂起后注意力应失活")
	}
	if len(app.queue) != qBefore {
		app.mu.Unlock()
		t.Fatalf("无外来时不得排 Suspend Op: %d→%d", qBefore, len(app.queue))
	}
	app.mu.Unlock()

	mid := mustView(t, app.doc)
	if len(mid.Disputes) != 0 {
		t.Fatalf("无外来 Suspend 不得产生 Dispute: %+v", mid.Disputes)
	}
	if mid.Lines[0].Content != "改后正文" || len(mid.Suspended) != 0 {
		t.Fatalf("正文/挂起被改: content=%q suspended=%v", mid.Lines[0].Content, mid.Suspended)
	}

	if err := app.Suspend(line, model.ActionEdit, false); err != nil {
		t.Fatal(err)
	}
	app.mu.Lock()
	if !app.attentionActive || app.attentionLine != line {
		app.mu.Unlock()
		t.Fatalf("恢复后应再激活: line=%q active=%v", app.attentionLine, app.attentionActive)
	}
	if len(app.queue) != qBefore {
		app.mu.Unlock()
		t.Fatalf("恢复亦不得排 Suspend Op")
	}
	app.mu.Unlock()

	after := mustView(t, app.doc)
	if len(after.Disputes) != 0 || after.Lines[0].Content != "改后正文" {
		t.Fatalf("恢复后 View 应仍无争议且正文不变: %+v", after)
	}
}

func TestSuspendWithForeignSuspendsOwnClaim(t *testing.T) {
	doc := document.New("t")
	lineID := mustView(t, doc).Lines[0].ID
	line := lineID.Hex()
	app := NewAppWithStateDir(t.TempDir())
	app.personID = "甲"
	app.articleID = "art"
	app.doc = doc

	if err := app.SubmitEdit(line, "本人主张"); err != nil {
		t.Fatal(err)
	}
	foreignID := model.NewID()
	ownID := model.NewID()
	if err := doc.ReceiveForeignEditClaim("甲", model.Dispute{
		ID:       foreignID,
		RealLine: lineID,
		Action:   model.ActionEdit,
		Person:   "乙",
		Content:  []string{"外来候选"},
	}, ownID); err != nil {
		t.Fatal(err)
	}
	v := mustView(t, doc)
	if len(v.Disputes) != 2 {
		t.Fatalf("应有双边候选: %d", len(v.Disputes))
	}

	if err := app.MoveCaretRange(line, ownID.Hex(), 0, 0, "", "", 0, 0); err != nil {
		t.Fatal(err)
	}
	app.mu.Lock()
	if !app.attentionActive || app.attentionLine != line {
		app.mu.Unlock()
		t.Fatalf("本人候选应激活: line=%q active=%v", app.attentionLine, app.attentionActive)
	}
	app.mu.Unlock()

	if err := app.Suspend(line, model.ActionEdit, true); err != nil {
		t.Fatal(err)
	}
	app.mu.Lock()
	if app.attentionActive {
		app.mu.Unlock()
		t.Fatal("挂起后注意力应失活")
	}
	foundSuspend := false
	for _, op := range app.queue {
		if op.Kind == protocol.TypeSuspend && op.Suspend != nil && op.Suspend.Suspended {
			foundSuspend = true
			break
		}
	}
	app.mu.Unlock()
	if !foundSuspend {
		t.Fatal("有外来时应排 Suspend Op")
	}
	after := mustView(t, app.doc)
	suspended := false
	for _, id := range after.Suspended {
		if id == ownID.Hex() {
			suspended = true
			break
		}
	}
	if !suspended {
		t.Fatalf("本人候选应挂起: suspended=%v disputes=%+v", after.Suspended, after.Disputes)
	}
}

func TestMoveCaretOnForeignClaimInactive(t *testing.T) {
	line := model.NewID()
	foreignID := model.NewID()
	ownID := model.NewID()
	doc := loadDoc(t, []model.Line{{ID: line, Content: "正式"}}, []model.Dispute{
		{ID: ownID, RealLine: line, Action: model.ActionEdit, Person: "甲", Content: []string{"我的"}},
		{ID: foreignID, RealLine: line, Action: model.ActionEdit, Person: "乙", Content: []string{"外来"}},
	})
	app := NewAppWithStateDir(t.TempDir())
	app.personID = "甲"
	app.doc = doc

	if err := app.MoveCaretRange(line.Hex(), "", 0, 0, "", "", 0, 0); err != nil {
		t.Fatal(err)
	}
	app.mu.Lock()
	if !app.attentionActive {
		app.mu.Unlock()
		t.Fatal("正式行应活跃")
	}
	app.mu.Unlock()

	if err := app.MoveCaretRange(line.Hex(), foreignID.Hex(), 0, 0, "", "", 0, 0); err != nil {
		t.Fatal(err)
	}
	app.mu.Lock()
	defer app.mu.Unlock()
	if app.attentionLine != line.Hex() {
		t.Fatalf("注意力行仍是起点: %q", app.attentionLine)
	}
	if app.attentionActive {
		t.Fatal("外来候选不得激活注意力")
	}
}
