package document

import (
	"slices"
	"testing"

	"github.com/KDreamelf/collaborative-editor/internal/model"
)

func TestApplyPlainSubmitEditLastWinsNoDispute(t *testing.T) {
	d := New("t")
	line := view(t, d).Lines[0].ID
	if err := d.ApplyPlainSubmit("甲", line, model.ActionEdit, []string{"A"}, nil); err != nil {
		t.Fatal(err)
	}
	if err := d.ApplyPlainSubmit("乙", line, model.ActionEdit, []string{"B"}, nil); err != nil {
		t.Fatal(err)
	}
	v := view(t, d)
	if len(v.Lines) != 1 || v.Lines[0].Content != "B" {
		t.Fatalf("正式链应是后者: %+v", v.Lines)
	}
	if len(v.Disputes) != 0 {
		t.Fatalf("普通 Edit 不得开争议: %+v", v.Disputes)
	}
}

func TestApplyPlainSubmitKeepsExplicitDisputes(t *testing.T) {
	d := New("t")
	line := view(t, d).Lines[0].ID
	id1, id2 := model.NewID(), model.NewID()
	d.disputes[id1] = &model.Dispute{
		ID: id1, RealLine: line, Action: model.ActionEdit, Person: "甲",
		Content: []string{"甲候选"}, Followers: []string{},
	}
	d.disputes[id2] = &model.Dispute{
		ID: id2, RealLine: line, Action: model.ActionEdit, Person: "乙",
		Content: []string{"乙候选"}, Followers: []string{},
	}
	if err := d.ApplyPlainSubmit("丙", line, model.ActionEdit, []string{"正式新"}, nil); err != nil {
		t.Fatal(err)
	}
	v := view(t, d)
	if v.Lines[0].Content != "正式新" {
		t.Fatalf("正式行应更新: %+v", v.Lines[0])
	}
	if len(v.Disputes) != 2 {
		t.Fatalf("显式争议应原样保留: %+v", v.Disputes)
	}
	a, _ := byPerson(v, "甲")
	b, _ := byPerson(v, "乙")
	if a.Content[0] != "甲候选" || b.Content[0] != "乙候选" {
		t.Fatalf("争议正文被改了: %+v %+v", a, b)
	}
}

func TestApplyPlainSubmitInsertEndsAndIdempotent(t *testing.T) {
	d := New("t")
	head := view(t, d).Lines[0].ID
	if err := d.ApplyPlainSubmit("甲", head, model.ActionEdit, []string{"头"}, nil); err != nil {
		t.Fatal(err)
	}

	tailID := model.NewID()
	if err := d.ApplyPlainSubmit("甲", head, model.ActionInsert, []string{"尾"}, []model.ID{tailID}); err != nil {
		t.Fatal(err)
	}
	beforeID := model.NewID()
	if err := d.ApplyPlainSubmit("乙", head, model.ActionInsertBefore, []string{"首"}, []model.ID{beforeID}); err != nil {
		t.Fatal(err)
	}
	v := view(t, d)
	if len(v.Lines) != 3 {
		t.Fatalf("应三行: %+v", v.Lines)
	}
	if v.Lines[0].ID != beforeID || v.Lines[0].Content != "首" {
		t.Fatalf("行首插入错: %+v", v.Lines[0])
	}
	if v.Lines[1].ID != head || v.Lines[1].Content != "头" {
		t.Fatalf("锚点应居中: %+v", v.Lines[1])
	}
	if v.Lines[2].ID != tailID || v.Lines[2].Content != "尾" {
		t.Fatalf("行尾插入错: %+v", v.Lines[2])
	}

	if err := d.ApplyPlainSubmit("甲", head, model.ActionInsert, []string{"尾"}, []model.ID{tailID}); err != nil {
		t.Fatal(err)
	}
	if err := d.ApplyPlainSubmit("乙", head, model.ActionInsertBefore, []string{"首"}, []model.ID{beforeID}); err != nil {
		t.Fatal(err)
	}
	v2 := view(t, d)
	if len(v2.Lines) != 3 {
		t.Fatalf("重发不得再插: %+v", v2.Lines)
	}
	ids := []model.ID{v2.Lines[0].ID, v2.Lines[1].ID, v2.Lines[2].ID}
	if !slices.Equal(ids, []model.ID{beforeID, head, tailID}) {
		t.Fatalf("重发后 ID 序变了: %+v", ids)
	}
}

func TestApplyPlainSubmitMultilineEdit(t *testing.T) {
	d := New("t")
	head := view(t, d).Lines[0].ID
	if err := d.ApplyPlainSubmit("甲", head, model.ActionEdit, []string{"旧"}, nil); err != nil {
		t.Fatal(err)
	}
	id2, id3 := model.NewID(), model.NewID()
	if err := d.ApplyPlainSubmit("甲", head, model.ActionEdit, []string{"一", "二", "三"}, []model.ID{id2, id3}); err != nil {
		t.Fatal(err)
	}
	v := view(t, d)
	if len(v.Lines) != 3 {
		t.Fatalf("多行 Edit 应三行: %+v", v.Lines)
	}
	if v.Lines[0].ID != head || v.Lines[0].Content != "一" {
		t.Fatalf("首行覆盖失败: %+v", v.Lines[0])
	}
	if v.Lines[1].ID != id2 || v.Lines[1].Content != "二" || v.Lines[2].ID != id3 || v.Lines[2].Content != "三" {
		t.Fatalf("预生行错: %+v", v.Lines)
	}
	if err := d.ApplyPlainSubmit("乙", head, model.ActionEdit, []string{"一", "二", "三"}, []model.ID{id2, id3}); err != nil {
		t.Fatal(err)
	}
	if len(view(t, d).Lines) != 3 {
		t.Fatal("多行 Edit 重发再插了")
	}
}

func TestApplyPlainSubmitRejectsSameIDsDifferentText(t *testing.T) {
	d := New("t")
	head := view(t, d).Lines[0].ID
	if err := d.ApplyPlainSubmit("甲", head, model.ActionEdit, []string{"头"}, nil); err != nil {
		t.Fatal(err)
	}
	id := model.NewID()
	if err := d.ApplyPlainSubmit("甲", head, model.ActionInsert, []string{"旧"}, []model.ID{id}); err != nil {
		t.Fatal(err)
	}
	if err := d.ApplyPlainSubmit("乙", id, model.ActionEdit, []string{"新"}, nil); err != nil {
		t.Fatal(err)
	}
	if err := d.ApplyPlainSubmit("甲", head, model.ActionInsert, []string{"别的"}, []model.ID{id}); err != ErrLineIDs {
		t.Fatalf("同 ID 不同正文应拒: %v", err)
	}
	v := view(t, d)
	if len(v.Lines) != 2 || v.Lines[1].ID != id || v.Lines[1].Content != "新" {
		t.Fatalf("冲突后正文应不变: %+v", v.Lines)
	}
}

func TestApplyPlainSubmitRetryAfterDisplaced(t *testing.T) {
	d := New("t")
	head := view(t, d).Lines[0].ID
	if err := d.ApplyPlainSubmit("甲", head, model.ActionEdit, []string{"头"}, nil); err != nil {
		t.Fatal(err)
	}
	aID, bID := model.NewID(), model.NewID()
	if err := d.ApplyPlainSubmit("甲", head, model.ActionInsert, []string{"A"}, []model.ID{aID}); err != nil {
		t.Fatal(err)
	}
	if err := d.ApplyPlainSubmit("乙", head, model.ActionInsert, []string{"B"}, []model.ID{bID}); err != nil {
		t.Fatal(err)
	}
	v := view(t, d)
	if len(v.Lines) != 3 || v.Lines[1].ID != bID || v.Lines[2].ID != aID {
		t.Fatalf("B 应挤到 A 前: %+v", v.Lines)
	}
	if err := d.ApplyPlainSubmit("甲", head, model.ActionInsert, []string{"A"}, []model.ID{aID}); err != nil {
		t.Fatalf("挤离锚点后同包重发应幂等: %v", err)
	}
	v2 := view(t, d)
	if len(v2.Lines) != 3 || v2.Lines[2].Content != "A" {
		t.Fatalf("重发不得改链: %+v", v2.Lines)
	}
}

func TestApplyPlainSubmitInsertBeliefStackedNoDispute(t *testing.T) {
	for _, tc := range []struct {
		name   string
		action string
		want   []string
	}{
		{"后插", model.ActionInsert, []string{"C", "A"}},
		{"前插", model.ActionInsertBefore, []string{"A", "C"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			d := New("t")
			anchor := view(t, d).Lines[0].ID
			aID, cID := model.NewID(), model.NewID()
			if err := d.ApplyPlainSubmit("甲", anchor, tc.action, []string{"A"}, []model.ID{aID}); err != nil {
				t.Fatal(err)
			}
			if err := d.ApplyPlainSubmit("丙", anchor, tc.action, []string{"C"}, []model.ID{cID}); err != nil {
				t.Fatal(err)
			}

			assertCA := func(doc *Doc) {
				t.Helper()
				got, err := doc.InsertBelief("丙", anchor, tc.action)
				if err != nil || !slices.Equal(got, tc.want) {
					t.Fatalf("InsertBelief: %v %+v want %+v", err, got, tc.want)
				}
				v := view(t, doc)
				if len(v.Disputes) != 0 {
					t.Fatalf("不得开争议: %+v", v.Disputes)
				}
			}
			assertCA(d)
			assertCA(roundTripLoad(t, d))

			linesBefore := len(view(t, d).Lines)
			candsBefore := len(d.collectAnchorInserts(claimKey{anchor, tc.action}))
			if err := d.ApplyPlainSubmit("甲", anchor, tc.action, []string{"A"}, []model.ID{aID}); err != nil {
				t.Fatal(err)
			}
			if err := d.ApplyPlainSubmit("丙", anchor, tc.action, []string{"C"}, []model.ID{cID}); err != nil {
				t.Fatal(err)
			}
			v := view(t, d)
			if len(v.Lines) != linesBefore {
				t.Fatalf("重发增行: %d→%d %+v", linesBefore, len(v.Lines), v.Lines)
			}
			if n := len(d.collectAnchorInserts(claimKey{anchor, tc.action})); n != candsBefore {
				t.Fatalf("重发增候选: %d→%d", candsBefore, n)
			}
			assertCA(d)
		})
	}
}

func TestApplyPlainSubmitRejectsBadInputZeroMutation(t *testing.T) {
	d := New("t")
	head := view(t, d).Lines[0].ID
	if err := d.ApplyPlainSubmit("甲", head, model.ActionEdit, []string{"稳"}, nil); err != nil {
		t.Fatal(err)
	}
	snap := view(t, d)

	assertUnchanged := func(err error, want error) {
		t.Helper()
		if err != want {
			t.Fatalf("want %v got %v", want, err)
		}
		v := view(t, d)
		if len(v.Lines) != 1 || v.Lines[0].Content != "稳" || v.Lines[0].ID != head {
			t.Fatalf("失败应零突变: %+v", v.Lines)
		}
		if len(v.Disputes) != len(snap.Disputes) {
			t.Fatalf("争议被碰了: %+v", v.Disputes)
		}
	}

	assertUnchanged(d.ApplyPlainSubmit("", head, model.ActionEdit, []string{"x"}, nil), ErrPerson)
	assertUnchanged(d.ApplyPlainSubmit("甲", model.NewID(), model.ActionEdit, []string{"x"}, nil), ErrLine)
	assertUnchanged(d.ApplyPlainSubmit("甲", head, model.ActionDelete, []string{"x"}, nil), ErrAction)
	assertUnchanged(d.ApplyPlainSubmit("甲", head, "乱", []string{"x"}, nil), ErrAction)
	assertUnchanged(d.ApplyPlainSubmit("甲", head, model.ActionEdit, nil, nil), ErrContent)
	assertUnchanged(d.ApplyPlainSubmit("甲", head, model.ActionInsert, []string{"x"}, nil), ErrLineIDs)
	assertUnchanged(d.ApplyPlainSubmit("甲", head, model.ActionInsertBefore, []string{"x"}, nil), ErrLineIDs)
	assertUnchanged(d.ApplyPlainSubmit("甲", head, model.ActionEdit, []string{"一", "二"}, nil), ErrLineIDs)
	assertUnchanged(d.ApplyPlainSubmit("甲", head, model.ActionEdit, []string{"x"}, []model.ID{model.NewID()}), ErrLineIDs)
	assertUnchanged(d.ApplyPlainSubmit("甲", head, model.ActionInsert, []string{"x"}, []model.ID{{}}), ErrLineIDs)
	assertUnchanged(d.ApplyPlainSubmit("甲", head, model.ActionInsert, []string{"x", "y"}, []model.ID{model.NewID()}), ErrLineIDs)

	dup := model.NewID()
	assertUnchanged(d.ApplyPlainSubmit("甲", head, model.ActionInsert, []string{"a", "b"}, []model.ID{dup, dup}), ErrLineIDs)
	// 段未齐（非幂等）但含已有正式行 ID → 冲突拒绝。
	assertUnchanged(d.ApplyPlainSubmit("甲", head, model.ActionInsert, []string{"a", "b"}, []model.ID{head, model.NewID()}), ErrLineIDs)

	// 断链：无唯一头。apply 在副本上改完后 ordered 失败，原 Doc 零突变。
	broken := d.cloneDoc()
	broken.lines[head].Prev = model.NewID()
	if err := broken.ApplyPlainSubmit("甲", head, model.ActionEdit, []string{"坏"}, nil); err != ErrHead {
		t.Fatalf("断链应 ErrHead，得到 %v", err)
	}
	if broken.lines[head].Content != "稳" {
		t.Fatalf("断链失败后仍被改写: %q", broken.lines[head].Content)
	}
	if view(t, d).Lines[0].Content != "稳" {
		t.Fatal("原 Doc 被波及")
	}
}

func TestAppendPlainBlankIDsIsSystemRow(t *testing.T) {
	d := New("t")
	anchor := view(t, d).Lines[0].ID
	added := model.NewID()
	if err := d.AppendPlainBlankIDs([]model.ID{added}); err != nil {
		t.Fatal(err)
	}
	v := view(t, d)
	if len(v.Lines) != 2 || v.Lines[1].ID != added || v.Lines[1].InsertOrigin != nil || len(v.Disputes) != 0 {
		t.Fatalf("系统空行不得成为任何人的主张: %+v", v)
	}
	if belief, err := d.InsertBelief("甲", anchor, model.ActionInsert); err != nil || len(belief) != 0 {
		t.Fatalf("系统空行不属于插入段: %v %v", belief, err)
	}
	if err := d.AppendPlainBlankIDs([]model.ID{added}); err != nil || len(view(t, d).Lines) != 2 {
		t.Fatalf("同一尾空行重复送达应幂等: %v", err)
	}
	if _, err := Load(v.Article, v.Lines, v.Disputes); err != nil {
		t.Fatalf("系统行应可重新加载: %v", err)
	}
	third := model.NewID()
	if err := d.AppendPlainBlankIDs([]model.ID{third}); err != nil {
		t.Fatal(err)
	}
	if err := d.AppendPlainBlankIDs([]model.ID{added}); err != ErrLineIDs {
		t.Fatalf("中间已有行不可误认成尾部重发: %v", err)
	}
}
