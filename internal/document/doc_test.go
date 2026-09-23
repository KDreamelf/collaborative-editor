package document

import (
	"testing"

	"github.com/KDreamelf/collaborative-editor/internal/model"
	"go.mongodb.org/mongo-driver/bson"
)

func view(t *testing.T, d *Doc) View {
	t.Helper()
	v, err := d.View()
	if err != nil {
		t.Fatal(err)
	}
	return v
}

func byPerson(v View, person string) (model.Dispute, bool) {
	for _, item := range v.Disputes {
		if item.Person == person {
			return item, true
		}
	}
	return model.Dispute{}, false
}

func TestNewHasOneHead(t *testing.T) {
	v := view(t, New("标题"))
	if v.Article.Title != "标题" || len(v.Lines) != 1 {
		t.Fatalf("新建文档应当只有一个空的首行: %+v", v)
	}
	if !v.Lines[0].Prev.IsZero() || !v.Lines[0].Next.IsZero() {
		t.Fatal("首行不该有前后")
	}
}

func TestLoadRejectsTwoHeads(t *testing.T) {
	a := model.Article{ID: model.NewID(), Title: "t"}
	_, err := Load(a, []model.Line{{ID: model.NewID(), Content: "a"}, {ID: model.NewID(), Content: "b"}}, nil)
	if err != ErrHead {
		t.Fatalf("两条没有「前」的正式行应失败，得到 %v", err)
	}
}

func TestLoadWalksNextNotQueryOrder(t *testing.T) {
	a := model.Article{ID: model.NewID(), Title: "t"}
	first := model.Line{ID: model.NewID(), Content: "1"}
	second := model.Line{ID: model.NewID(), Content: "2", Prev: first.ID}
	first.Next = second.ID
	d, err := Load(a, []model.Line{second, first}, nil)
	if err != nil {
		t.Fatal(err)
	}
	v := view(t, d)
	if v.Lines[0].Content != "1" || v.Lines[1].Content != "2" {
		t.Fatalf("应按「后」走，不是按载入顺序: %+v", v.Lines)
	}
}

func TestSpliceWritesOnlyTwoOldPointers(t *testing.T) {
	d := New("t")
	if err := d.EnsureLines(2); err != nil {
		t.Fatal(err)
	}
	before := view(t, d)
	anchor, tail := before.Lines[0], before.Lines[1]
	if err := d.Submit("p1", anchor.ID, model.ActionInsert, []string{"x", "y"}); err != nil {
		t.Fatal(err)
	}
	after := view(t, d)
	if len(after.Lines) != 4 || len(after.Disputes) != 0 {
		t.Fatalf("没人争的插入应直接进正文: %+v", after)
	}
	if after.Lines[0].ID != anchor.ID || after.Lines[3].ID != tail.ID {
		t.Fatalf("旧行应当还在两端: %+v", after.Lines)
	}
	if after.Lines[0].Prev != anchor.Prev || after.Lines[3].Next != tail.Next {
		t.Fatal("旧行除了衔接处，前后不该动")
	}
	if after.Lines[0].Next != after.Lines[1].ID || after.Lines[3].Prev != after.Lines[2].ID {
		t.Fatal("衔接处应当改成新段的头和尾")
	}
	if after.Lines[2].Prev != after.Lines[1].ID {
		t.Fatal("段尾的前一行应当仍是段里面的上一行")
	}
}

func TestConcurrentInsertRestoresNext(t *testing.T) {
	d := New("t")
	if err := d.EnsureLines(2); err != nil {
		t.Fatal(err)
	}
	before := view(t, d)
	anchor, tail := before.Lines[0].ID, before.Lines[1].ID
	if err := d.Submit("p1", anchor, model.ActionInsert, []string{"x", "same"}); err != nil {
		t.Fatal(err)
	}
	if err := d.Submit("p2", anchor, model.ActionInsert, []string{"x", "other"}); err != nil {
		t.Fatal(err)
	}
	after := view(t, d)
	if len(after.Lines) != 2 || after.Lines[0].Next != tail || after.Lines[1].Prev != anchor {
		t.Fatalf("两段相撞时，原来的后文还应挂在正式行的「后」上: %+v", after.Lines)
	}
	if len(after.Disputes) != 2 {
		t.Fatalf("每人一份争议: %+v", after.Disputes)
	}
	p1, _ := byPerson(after, "p1")
	p2, _ := byPerson(after, "p2")
	if len(p1.Content) != 2 || p1.Content[1] != "same" || p2.Content[1] != "other" || p1.Action != model.ActionInsert {
		t.Fatalf("整段留下，不因为第一行相同就拆开: %+v %+v", p1.Content, p2.Content)
	}
}

func TestEditConflictRestoresFormalLine(t *testing.T) {
	d := New("t")
	line := view(t, d).Lines[0].ID
	if err := d.Submit("p1", line, model.ActionEdit, []string{"mine"}); err != nil {
		t.Fatal(err)
	}
	if err := d.Submit("p2", line, model.ActionEdit, []string{"yours"}); err != nil {
		t.Fatal(err)
	}
	after := view(t, d)
	if after.Lines[0].Content != "" || len(after.Disputes) != 2 {
		t.Fatalf("撞上之后正式行回到原来的字，两份主张都留: %+v", after)
	}
}

func TestSuspendedEditDoesNotOpenDispute(t *testing.T) {
	d := New("t")
	line := view(t, d).Lines[0].ID
	if err := d.Submit("p1", line, model.ActionEdit, []string{"faded"}); err != nil {
		t.Fatal(err)
	}
	if err := d.SetSuspended("p1", line, model.ActionEdit, true); err != nil {
		t.Fatal(err)
	}
	if err := d.Submit("p2", line, model.ActionEdit, []string{"fresh"}); err != nil {
		t.Fatal(err)
	}
	after := view(t, d)
	if after.Lines[0].Content != "fresh" || len(after.Disputes) != 1 || len(after.Suspended) != 1 {
		t.Fatalf("挂起时别人来写不拉争议，旧主张还在: %+v", after)
	}
	if err := d.SetSuspended("p1", line, model.ActionEdit, false); err != nil {
		t.Fatal(err)
	}
	back := view(t, d)
	if back.Lines[0].Content != "faded" || len(back.Disputes) != 2 {
		t.Fatalf("光标回到挂起的主张上，和后来的人形成争议: %+v", back)
	}
}

func TestFollowNeedsNodThenResolves(t *testing.T) {
	d := twoClaims(t)
	v := view(t, d)
	p1, _ := byPerson(v, "p1")
	p2, _ := byPerson(v, "p2")
	got, err := d.RequestFollow("p2", p1.ID, 1)
	if err != nil || got.Status != FollowPending || got.PeerID != "p1" {
		t.Fatalf("不是互追，要等对方点头: %+v %v", got, err)
	}
	if len(view(t, d).Disputes) != 2 {
		t.Fatal("点头前两边的主张都还在")
	}
	got, err = d.AnswerFollow("p1", "p2", p1.ID, true)
	if err != nil || got.Status != FollowApplied {
		t.Fatal(err, got)
	}
	after := view(t, d)
	if len(after.Disputes) != 0 || after.Lines[0].Content != "a" {
		t.Fatalf("都追随到 p1 后争议结束，正文写成他的: %+v", after)
	}
	_ = p2
}

func TestMutualFollowUsesEarlierTimestamp(t *testing.T) {
	d := twoClaims(t)
	v := view(t, d)
	p1, _ := byPerson(v, "p1")
	p2, _ := byPerson(v, "p2")
	if _, err := d.RequestFollow("p1", p2.ID, 20); err != nil {
		t.Fatal(err)
	}
	got, err := d.RequestFollow("p2", p1.ID, 10)
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != FollowApplied || got.PeerStatus != FollowLost || got.PeerID != "p1" {
		t.Fatalf("更早的追随生效，更晚的失败: %+v", got)
	}
	after := view(t, d)
	if after.Lines[0].Content != "a" || len(after.Disputes) != 0 {
		t.Fatalf("p2 更早追随 p1，正文应是 a: %+v", after)
	}
}

func TestFollowersCascadeAndReeditRejoins(t *testing.T) {
	d := New("t")
	line := view(t, d).Lines[0].ID
	for _, pair := range []struct{ p, text string }{{"p1", "a"}, {"p2", "b"}, {"p3", "c"}} {
		if err := d.Submit(pair.p, line, model.ActionEdit, []string{pair.text}); err != nil {
			t.Fatal(err)
		}
	}
	v := view(t, d)
	p1, _ := byPerson(v, "p1")
	p2, _ := byPerson(v, "p2")
	p3, _ := byPerson(v, "p3")
	if _, err := d.RequestFollow("p3", p1.ID, 1); err != nil {
		t.Fatal(err)
	}
	if _, err := d.AnswerFollow("p1", "p3", p1.ID, true); err != nil {
		t.Fatal(err)
	}
	mid := view(t, d)
	leader, _ := byPerson(mid, "p1")
	if len(leader.Followers) != 1 || leader.Followers[0] != "p3" {
		t.Fatalf("p3 应跟着 p1: %+v", leader.Followers)
	}
	if err := d.Submit("p3", line, model.ActionEdit, []string{"c2"}); err != nil {
		t.Fatal(err)
	}
	rejoinc := view(t, d)
	leader, _ = byPerson(rejoinc, "p1")
	if len(leader.Followers) != 0 {
		t.Fatalf("再改自己的这份，追随取消: %+v", leader.Followers)
	}
	mine, ok := byPerson(rejoinc, "p3")
	if !ok || mine.Content[0] != "c2" {
		t.Fatal("p3 应重新加入争议")
	}
	if _, err := d.RequestFollow("p3", p1.ID, 2); err != nil {
		t.Fatal(err)
	}
	if _, err := d.AnswerFollow("p1", "p3", p1.ID, true); err != nil {
		t.Fatal(err)
	}
	if _, err := d.RequestFollow("p1", p2.ID, 3); err != nil {
		t.Fatal(err)
	}
	if _, err := d.AnswerFollow("p2", "p1", p2.ID, true); err != nil {
		t.Fatal(err)
	}
	after := view(t, d)
	if len(after.Disputes) != 0 || after.Lines[0].Content != "b" {
		t.Fatalf("p1 带着 p3 追随 p2，争议结束: %+v", after)
	}
	_ = p3
}

func TestPasteConflictIsOneClaim(t *testing.T) {
	d := New("t")
	line := view(t, d).Lines[0].ID
	if err := d.Submit("p1", line, model.ActionEdit, []string{"hello", "world"}); err != nil {
		t.Fatal(err)
	}
	if err := d.Submit("p2", line, model.ActionEdit, []string{"no"}); err != nil {
		t.Fatal(err)
	}
	hit := view(t, d)
	if len(hit.Lines) != 1 || hit.Lines[0].Content != "" {
		t.Fatalf("粘贴相撞时不该把段留在正文里: %+v", hit.Lines)
	}
	p1, _ := byPerson(hit, "p1")
	if len(p1.Content) != 2 {
		t.Fatal("一次粘贴的多行在同一份主张里")
	}
	if _, err := d.RequestFollow("p2", p1.ID, 1); err != nil {
		t.Fatal(err)
	}
	if _, err := d.AnswerFollow("p1", "p2", p1.ID, true); err != nil {
		t.Fatal(err)
	}
	won := view(t, d)
	if len(won.Lines) != 2 || won.Lines[0].Content != "hello" || won.Lines[1].Content != "world" {
		t.Fatalf("胜出的粘贴才拆进正式行: %+v", won.Lines)
	}
}

func TestRepeatInsertDoesNotDuplicate(t *testing.T) {
	d := New("t")
	line := view(t, d).Lines[0].ID
	if err := d.Submit("p1", line, model.ActionInsert, []string{"x", "y"}); err != nil {
		t.Fatal(err)
	}
	if err := d.Submit("p1", line, model.ActionInsert, []string{"x", "y"}); err != nil {
		t.Fatal(err)
	}
	again := view(t, d)
	if len(again.Lines) != 3 || again.Lines[1].Content != "x" || again.Lines[2].Content != "y" {
		t.Fatalf("同一份插入重发不应再接一段: %+v", again.Lines)
	}
}

func TestDeleteAndMerge(t *testing.T) {
	d := New("t")
	if err := d.EnsureLines(2); err != nil {
		t.Fatal(err)
	}
	lines := view(t, d).Lines
	if err := d.Submit("p1", lines[0].ID, model.ActionEdit, []string{"ab"}); err != nil {
		t.Fatal(err)
	}
	if err := d.Submit("p1", lines[1].ID, model.ActionEdit, []string{"cd"}); err != nil {
		t.Fatal(err)
	}
	if err := d.DeleteIfIdle("p1", lines[1].ID, []Presence{{Person: "p2", Active: true}}); err != nil {
		t.Fatal(err)
	}
	held := view(t, d)
	if len(held.Lines) != 2 || len(held.Disputes) != 2 {
		t.Fatalf("有人还在这行，不销毁，拉争议: lines=%d disputes=%d", len(held.Lines), len(held.Disputes))
	}
	d = New("t")
	if err := d.EnsureLines(2); err != nil {
		t.Fatal(err)
	}
	lines = view(t, d).Lines
	if err := d.Submit("p1", lines[0].ID, model.ActionEdit, []string{"ab"}); err != nil {
		t.Fatal(err)
	}
	if err := d.Submit("p1", lines[1].ID, model.ActionEdit, []string{"cd"}); err != nil {
		t.Fatal(err)
	}
	if err := d.MergeUp("p1", lines[1].ID, nil); err != nil {
		t.Fatal(err)
	}
	merged := view(t, d)
	if len(merged.Lines) != 1 || merged.Lines[0].Content != "abcd" {
		t.Fatalf("行首退格应接上前一行并销毁这一行: %+v", merged.Lines)
	}
}

func TestBSONRoundTripKeepsHeadEmpty(t *testing.T) {
	d := New("面试")
	line := view(t, d).Lines[0]
	if err := d.Submit("p1", line.ID, model.ActionEdit, []string{"正文"}); err != nil {
		t.Fatal(err)
	}
	if err := d.Submit("p2", line.ID, model.ActionEdit, []string{"另一份"}); err != nil {
		t.Fatal(err)
	}
	v := view(t, d)
	var raws []bson.Raw
	for _, item := range []any{v.Article, v.Lines[0], v.Disputes[0], v.Disputes[1]} {
		b, err := bson.Marshal(item)
		if err != nil {
			t.Fatal(err)
		}
		raws = append(raws, b)
	}
	article, lines, disputes, err := model.SplitBSON(raws)
	if err != nil {
		t.Fatal(err)
	}
	loaded, err := Load(article, lines, disputes)
	if err != nil {
		t.Fatal(err)
	}
	again := view(t, loaded)
	if again.Article.Title != "面试" || again.Lines[0].Content != "" || len(again.Disputes) != 2 {
		t.Fatalf("入库再读回应保持争议，正式行不被后到的盖掉: %+v", again)
	}
	if !again.Lines[0].Prev.IsZero() {
		t.Fatal("首行的「前」应为空")
	}
}

func twoClaims(t *testing.T) *Doc {
	t.Helper()
	d := New("t")
	line := view(t, d).Lines[0].ID
	if err := d.Submit("p1", line, model.ActionEdit, []string{"a"}); err != nil {
		t.Fatal(err)
	}
	if err := d.Submit("p2", line, model.ActionEdit, []string{"b"}); err != nil {
		t.Fatal(err)
	}
	return d
}
