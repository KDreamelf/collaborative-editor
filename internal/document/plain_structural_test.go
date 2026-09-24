package document

import (
	"slices"
	"testing"

	"github.com/KDreamelf/collaborative-editor/internal/model"
)

func TestApplyPlainDeleteMergeSpanLikeEditor(t *testing.T) {
	d := New("t")
	if err := d.EnsureLines(3); err != nil {
		t.Fatal(err)
	}
	v := view(t, d)
	a, b, c := v.Lines[0].ID, v.Lines[1].ID, v.Lines[2].ID

	if err := d.ApplyPlainDelete(b); err != nil {
		t.Fatal(err)
	}
	v = view(t, d)
	if len(v.Lines) != 2 || v.Lines[0].ID != a || v.Lines[1].ID != c {
		t.Fatalf("删空行后应剩 a,c: %+v", v.Lines)
	}
	if len(v.Disputes) != 0 {
		t.Fatalf("普通删不得开争议: %+v", v.Disputes)
	}

	// 再补一行，测 merge：把末行并入前行。
	if err := d.EnsureLines(2); err != nil {
		t.Fatal(err)
	}
	v = view(t, d)
	head, tail := v.Lines[0].ID, v.Lines[1].ID
	if err := d.ApplyPlainSubmit("甲", head, model.ActionEdit, []string{"前"}, nil); err != nil {
		t.Fatal(err)
	}
	if err := d.ApplyPlainSubmit("甲", tail, model.ActionEdit, []string{"后"}, nil); err != nil {
		t.Fatal(err)
	}
	if err := d.ApplyPlainMerge(tail); err != nil {
		t.Fatal(err)
	}
	v = view(t, d)
	if len(v.Lines) != 1 || v.Lines[0].ID != head || v.Lines[0].Content != "前后" {
		t.Fatalf("merge 应并到前行: %+v", v.Lines)
	}
	if len(v.Disputes) != 0 {
		t.Fatalf("普通 merge 不得开争议: %+v", v.Disputes)
	}

	// 两行选区整段替换。
	if err := d.EnsureLines(2); err != nil {
		t.Fatal(err)
	}
	v = view(t, d)
	id0, id1 := v.Lines[0].ID, v.Lines[1].ID
	if err := d.ApplyPlainSubmit("甲", id0, model.ActionEdit, []string{"旧一"}, nil); err != nil {
		t.Fatal(err)
	}
	if err := d.ApplyPlainSubmit("甲", id1, model.ActionEdit, []string{"旧二"}, nil); err != nil {
		t.Fatal(err)
	}
	newTail := model.NewID()
	if err := d.ApplyPlainSpan(SpanEditOpts{
		BaseIDs:     []model.ID{id0, id1},
		BaseTexts:   []string{"旧一", "旧二"},
		AfterSeen:   model.ID{},
		Replacement: []string{"新一", "新二"},
		LineIDs:     []model.ID{newTail},
	}); err != nil {
		t.Fatal(err)
	}
	v = view(t, d)
	if len(v.Lines) != 2 {
		t.Fatalf("跨度替换应两行: %+v", v.Lines)
	}
	if v.Lines[0].ID != id0 || v.Lines[0].Content != "新一" {
		t.Fatalf("首行 ID 应保持: %+v", v.Lines[0])
	}
	if v.Lines[1].ID != newTail || v.Lines[1].Content != "新二" {
		t.Fatalf("后续应用预生 ID: %+v", v.Lines[1])
	}
	if len(v.Disputes) != 0 {
		t.Fatalf("普通跨度不得开争议: %+v", v.Disputes)
	}
}

func TestApplyPlainStructuralAfterOwnInsert(t *testing.T) {
	for _, action := range []string{"delete", "merge", "span"} {
		t.Run(action, func(t *testing.T) {
			d := New("t")
			anchor := view(t, d).Lines[0].ID
			inserted := model.NewID()
			text := ""
			if action != "delete" {
				text = "后"
			}
			if err := d.ApplyPlainSubmit("甲", anchor, model.ActionInsert, []string{text}, []model.ID{inserted}); err != nil {
				t.Fatal(err)
			}
			var err error
			switch action {
			case "delete":
				err = d.ApplyPlainDelete(inserted)
			case "merge":
				err = d.ApplyPlainMerge(inserted)
			case "span":
				err = d.ApplyPlainSpan(SpanEditOpts{BaseIDs: []model.ID{anchor, inserted}, BaseTexts: []string{"", text},
					Replacement: []string{"替换"}})
			}
			if err != nil {
				t.Fatal(err)
			}
			v := view(t, d)
			if len(v.Lines) != 1 || len(v.Disputes) != 0 {
				t.Fatalf("普通结构操作后应只剩一条正文，无争议: %+v", v)
			}
			if _, err := Load(v.Article, v.Lines, v.Disputes); err != nil {
				t.Fatalf("重新打开应能读到链: %v", err)
			}
		})
	}
}

func TestApplyPlainStructuralRejectsExplicitClaims(t *testing.T) {
	d := New("t")
	if err := d.EnsureLines(3); err != nil {
		t.Fatal(err)
	}
	v := view(t, d)
	a, b, c := v.Lines[0].ID, v.Lines[1].ID, v.Lines[2].ID
	if err := d.ApplyPlainSubmit("甲", a, model.ActionEdit, []string{"A"}, nil); err != nil {
		t.Fatal(err)
	}
	if err := d.ApplyPlainSubmit("甲", b, model.ActionEdit, []string{""}, nil); err != nil {
		t.Fatal(err)
	}
	if err := d.ApplyPlainSubmit("甲", c, model.ActionEdit, []string{"C"}, nil); err != nil {
		t.Fatal(err)
	}

	claimID := model.NewID()
	d.disputes[claimID] = &model.Dispute{
		ID: claimID, RealLine: b, Action: model.ActionEdit, Person: "乙",
		Content: []string{"乙候选"}, Followers: []string{},
	}
	snap := view(t, d)

	if err := d.ApplyPlainDelete(b); err != ErrHasClaims {
		t.Fatalf("有显式主张应拒删，got %v", err)
	}
	if err := d.ApplyPlainMerge(b); err != ErrHasClaims {
		t.Fatalf("有显式主张应拒 merge，got %v", err)
	}
	newID := model.NewID()
	if err := d.ApplyPlainSpan(SpanEditOpts{
		BaseIDs:     []model.ID{a, b},
		BaseTexts:   []string{"A", ""},
		AfterSeen:   c,
		Replacement: []string{"X", "Y"},
		LineIDs:     []model.ID{newID},
	}); err != ErrSpanBlocked {
		t.Fatalf("跨度含显式主张应拒，got %v", err)
	}

	v = view(t, d)
	if !slices.Equal(lineContents(v), lineContents(snap)) {
		t.Fatalf("拒绝后正文变了: got %+v want %+v", lineContents(v), lineContents(snap))
	}
	if len(v.Disputes) != 1 || v.Disputes[0].Content[0] != "乙候选" {
		t.Fatalf("主张应保留: %+v", v.Disputes)
	}
	ids := []model.ID{v.Lines[0].ID, v.Lines[1].ID, v.Lines[2].ID}
	if !slices.Equal(ids, []model.ID{a, b, c}) {
		t.Fatalf("行 ID 应原样: %+v", ids)
	}
}

func TestApplyPlainSpanRejectsBadInputZeroMutation(t *testing.T) {
	d := New("t")
	if err := d.EnsureLines(3); err != nil {
		t.Fatal(err)
	}
	v := view(t, d)
	a, b, c := v.Lines[0].ID, v.Lines[1].ID, v.Lines[2].ID
	_ = d.ApplyPlainSubmit("甲", a, model.ActionEdit, []string{"1"}, nil)
	_ = d.ApplyPlainSubmit("甲", b, model.ActionEdit, []string{"2"}, nil)
	_ = d.ApplyPlainSubmit("甲", c, model.ActionEdit, []string{"3"}, nil)
	snap := view(t, d)

	assertUnchanged := func(err, want error) {
		t.Helper()
		if err != want {
			t.Fatalf("want %v got %v", want, err)
		}
		got := view(t, d)
		if !slices.Equal(lineContents(got), lineContents(snap)) {
			t.Fatalf("失败应零突变: %+v", got.Lines)
		}
		if len(got.Disputes) != 0 {
			t.Fatalf("争议被碰: %+v", got.Disputes)
		}
	}

	// 不相邻：跳过中间行。
	assertUnchanged(d.ApplyPlainSpan(SpanEditOpts{
		BaseIDs:     []model.ID{a, c},
		BaseTexts:   []string{"1", "3"},
		AfterSeen:   model.ID{},
		Replacement: []string{"X", "Y"},
		LineIDs:     []model.ID{model.NewID()},
	}), ErrSpanBase)

	// 预生 ID 撞已有正式行。
	assertUnchanged(d.ApplyPlainSpan(SpanEditOpts{
		BaseIDs:     []model.ID{a, b},
		BaseTexts:   []string{"1", "2"},
		AfterSeen:   c,
		Replacement: []string{"X", "Y"},
		LineIDs:     []model.ID{c},
	}), ErrLineIDs)

	// 缺失行。
	assertUnchanged(d.ApplyPlainSpan(SpanEditOpts{
		BaseIDs:     []model.ID{a, model.NewID()},
		BaseTexts:   []string{"1", "?"},
		AfterSeen:   b,
		Replacement: []string{"X", "Y"},
		LineIDs:     []model.ID{model.NewID()},
	}), ErrSpanBase)
}

func TestApplyPlainSuspendNoClaimNoSolo(t *testing.T) {
	d := New("t")
	line := view(t, d).Lines[0].ID
	if err := d.ApplyPlainSubmit("甲", line, model.ActionEdit, []string{"正文"}, nil); err != nil {
		t.Fatal(err)
	}
	if err := d.ApplyPlainSuspend("甲", line, model.ActionEdit, true); err != nil {
		t.Fatal(err)
	}
	v := view(t, d)
	if len(v.Disputes) != 0 {
		t.Fatalf("无显式主张时 Suspend 不得造候选: %+v", v.Disputes)
	}
	if len(v.Suspended) != 0 {
		t.Fatalf("无主张不得挂 suspended: %+v", v.Suspended)
	}
	if v.Lines[0].Content != "正文" {
		t.Fatalf("正文被改: %+v", v.Lines[0])
	}

	// 有显式主张时只改 suspended。
	id := model.NewID()
	d.disputes[id] = &model.Dispute{
		ID: id, RealLine: line, Action: model.ActionEdit, Person: "甲",
		Content: []string{"候选"}, Followers: []string{},
	}
	if err := d.ApplyPlainSuspend("甲", line, model.ActionEdit, true); err != nil {
		t.Fatal(err)
	}
	v = view(t, d)
	if len(v.Disputes) != 1 || len(v.Suspended) != 1 || v.Suspended[0] != id.Hex() {
		t.Fatalf("应只挂起已有主张: disputes=%+v suspended=%+v", v.Disputes, v.Suspended)
	}
	if err := d.ApplyPlainSuspend("甲", line, model.ActionEdit, false); err != nil {
		t.Fatal(err)
	}
	v = view(t, d)
	if len(v.Suspended) != 0 || len(v.Disputes) != 1 {
		t.Fatalf("恢复后主张仍在、suspended 清空: %+v %+v", v.Disputes, v.Suspended)
	}
}

func TestApplyPlainDeleteLastLineClearsOnly(t *testing.T) {
	d := New("t")
	line := view(t, d).Lines[0].ID
	if err := d.ApplyPlainDelete(line); err != nil {
		t.Fatal(err)
	}
	v := view(t, d)
	if len(v.Lines) != 1 || v.Lines[0].ID != line || v.Lines[0].Content != "" {
		t.Fatalf("只剩一行应清空不删: %+v", v.Lines)
	}
}

func TestApplyPlainMergeFirstNoop(t *testing.T) {
	d := New("t")
	if err := d.EnsureLines(2); err != nil {
		t.Fatal(err)
	}
	v := view(t, d)
	head := v.Lines[0].ID
	_ = d.ApplyPlainSubmit("甲", head, model.ActionEdit, []string{"头"}, nil)
	if err := d.ApplyPlainMerge(head); err != nil {
		t.Fatal(err)
	}
	v = view(t, d)
	if len(v.Lines) != 2 || v.Lines[0].Content != "头" {
		t.Fatalf("首行 merge 应 no-op: %+v", v.Lines)
	}
}

func lineContents(v View) []string {
	out := make([]string, len(v.Lines))
	for i := range v.Lines {
		out[i] = v.Lines[i].Content
	}
	return out
}
