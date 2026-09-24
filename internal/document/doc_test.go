package document

import (
	"bytes"
	"encoding/json"
	"slices"
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

func strPtr(s string) *string { return &s }

func TestEditBaseSyncedContinuation(t *testing.T) {
	d := New("t")
	line := view(t, d).Lines[0].ID
	if err := d.Submit("甲", line, model.ActionEdit, []string{"y"}); err != nil {
		t.Fatal(err)
	}
	if err := d.SubmitWith("乙", line, model.ActionEdit, []string{"z"}, SubmitOpts{BaseContent: strPtr("y")}); err != nil {
		t.Fatal(err)
	}
	after := view(t, d)
	if after.Lines[0].Content != "z" || len(after.Disputes) != 0 {
		t.Fatalf("已同步续写应无争议: %+v", after)
	}
	if after.Lines[0].RecentEdit == nil || after.Lines[0].RecentEdit.Person != "乙" || after.Lines[0].RecentEdit.Before != "y" {
		t.Fatalf("最近编辑应换乙: %+v", after.Lines[0].RecentEdit)
	}
}

func TestEditBaseStaleOpensDispute(t *testing.T) {
	d := New("t")
	line := view(t, d).Lines[0].ID
	if err := d.Submit("甲", line, model.ActionEdit, []string{"y"}); err != nil {
		t.Fatal(err)
	}
	if err := d.SubmitWith("乙", line, model.ActionEdit, []string{"z"}, SubmitOpts{BaseContent: strPtr("")}); err != nil {
		t.Fatal(err)
	}
	after := view(t, d)
	if after.Lines[0].Content != "" || len(after.Disputes) != 2 {
		t.Fatalf("落后应两份争议、正文回共同基准: %+v", after)
	}
	jia, _ := byPerson(after, "甲")
	yi, _ := byPerson(after, "乙")
	if len(jia.Content) != 1 || jia.Content[0] != "y" || len(yi.Content) != 1 || yi.Content[0] != "z" {
		t.Fatalf("甲 y / 乙 z: %+v %+v", jia, yi)
	}
}

func TestEditBaseSameContentNoDispute(t *testing.T) {
	d := New("t")
	line := view(t, d).Lines[0].ID
	if err := d.Submit("甲", line, model.ActionEdit, []string{"z"}); err != nil {
		t.Fatal(err)
	}
	if err := d.SubmitWith("乙", line, model.ActionEdit, []string{"z"}, SubmitOpts{BaseContent: strPtr("")}); err != nil {
		t.Fatal(err)
	}
	after := view(t, d)
	if after.Lines[0].Content != "z" || len(after.Disputes) != 0 {
		t.Fatalf("相同正文不新开争议: %+v", after)
	}
}

func TestEditBaseRecentEditAfterLoad(t *testing.T) {
	d := New("t")
	line := view(t, d).Lines[0].ID
	if err := d.Submit("甲", line, model.ActionEdit, []string{"y"}); err != nil {
		t.Fatal(err)
	}
	loaded := roundTripLoad(t, d)
	if err := loaded.SubmitWith("乙", line, model.ActionEdit, []string{"z"}, SubmitOpts{BaseContent: strPtr("")}); err != nil {
		t.Fatal(err)
	}
	after := view(t, loaded)
	if after.Lines[0].Content != "" || len(after.Disputes) != 2 {
		t.Fatalf("Load 后甲来源仍能开场: %+v", after)
	}
	jia, ok := byPerson(after, "甲")
	if !ok || jia.Content[0] != "y" {
		t.Fatalf("应有甲 y: %+v", after.Disputes)
	}
}

// TestEditBaseSplicedMultiGenRejects：多行 splice live 遇多代基准仍安全拒绝、零变化。
func TestEditBaseSplicedMultiGenRejects(t *testing.T) {
	d := New("t")
	line := view(t, d).Lines[0].ID
	if err := d.Submit("甲", line, model.ActionEdit, []string{"y"}); err != nil {
		t.Fatal(err)
	}
	tail := model.NewID()
	if err := d.SubmitWith("丙", line, model.ActionEdit, []string{"z", "extra"}, SubmitOpts{BaseContent: strPtr("y"), LineIDs: []model.ID{tail}}); err != nil {
		t.Fatal(err)
	}
	live := d.live[claimKey{line, model.ActionEdit}]
	if live == nil || !live.spliced || live.before != "y" {
		t.Fatalf("前置：丙多行 splice live: %+v", live)
	}
	before := view(t, d)
	err := d.SubmitWith("乙", line, model.ActionEdit, []string{"w"}, SubmitOpts{BaseContent: strPtr("")})
	if err != ErrStaleEdit {
		t.Fatalf("多行 splice 多代落后应拒绝: %v", err)
	}
	after := view(t, d)
	if after.Lines[0].Content != "z" || len(after.Lines) != 2 || len(after.Disputes) != 0 {
		t.Fatalf("拒绝不得改正文: %+v", after)
	}
	if after.Lines[1].ID != before.Lines[1].ID || after.Lines[1].Content != "extra" {
		t.Fatalf("拒绝不得拆附属行: %+v", after.Lines)
	}
}

// TestEditBaseLinearAncestorOpensDispute：0→甲A→乙B 后丙仍基于 0→C；甲已是线性祖先，仅乙B与丙C两候选，正文回 0。
func TestEditBaseLinearAncestorOpensDispute(t *testing.T) {
	d := New("t")
	line := view(t, d).Lines[0].ID
	if err := d.Submit("甲", line, model.ActionEdit, []string{"0"}); err != nil {
		t.Fatal(err)
	}
	if err := d.SubmitWith("甲", line, model.ActionEdit, []string{"A"}, SubmitOpts{BaseContent: strPtr("0")}); err != nil {
		t.Fatal(err)
	}
	if err := d.SubmitWith("乙", line, model.ActionEdit, []string{"B"}, SubmitOpts{BaseContent: strPtr("A")}); err != nil {
		t.Fatal(err)
	}
	live := d.live[claimKey{line, model.ActionEdit}]
	if live == nil || live.person != "乙" || live.before != "A" || live.content[0] != "B" {
		t.Fatalf("前置：唯一 live 乙 before=A content=B: %+v", live)
	}
	if err := d.SubmitWith("丙", line, model.ActionEdit, []string{"C"}, SubmitOpts{BaseContent: strPtr("0")}); err != nil {
		t.Fatal(err)
	}
	after := view(t, d)
	if after.Lines[0].Content != "0" || after.Lines[0].ID != line || len(after.Disputes) != 2 {
		t.Fatalf("正文回 0、恰两候选: %+v", after)
	}
	if _, ok := byPerson(after, "甲"); ok {
		t.Fatalf("甲是线性祖先，不应第三候选: %+v", after.Disputes)
	}
	yi, _ := byPerson(after, "乙")
	bing, _ := byPerson(after, "丙")
	if len(yi.Content) != 1 || yi.Content[0] != "B" || len(bing.Content) != 1 || bing.Content[0] != "C" {
		t.Fatalf("乙 B / 丙 C: %+v %+v", yi, bing)
	}
	if d.live[claimKey{line, model.ActionEdit}] != nil {
		t.Fatal("争议后不应残留 edit live")
	}
}

// TestEditBaseLinearAncestorAfterLoad：View→BSON→Load 后同一线性时序仍两候选、正文回 0、无甲。
func TestEditBaseLinearAncestorAfterLoad(t *testing.T) {
	d := New("t")
	line := view(t, d).Lines[0].ID
	if err := d.Submit("甲", line, model.ActionEdit, []string{"0"}); err != nil {
		t.Fatal(err)
	}
	if err := d.SubmitWith("甲", line, model.ActionEdit, []string{"A"}, SubmitOpts{BaseContent: strPtr("0")}); err != nil {
		t.Fatal(err)
	}
	if err := d.SubmitWith("乙", line, model.ActionEdit, []string{"B"}, SubmitOpts{BaseContent: strPtr("A")}); err != nil {
		t.Fatal(err)
	}
	loaded := roundTripLoad(t, d)
	if err := loaded.SubmitWith("丙", line, model.ActionEdit, []string{"C"}, SubmitOpts{BaseContent: strPtr("0")}); err != nil {
		t.Fatal(err)
	}
	after := view(t, loaded)
	if after.Lines[0].Content != "0" || after.Lines[0].ID != line || len(after.Disputes) != 2 {
		t.Fatalf("Load 后正文回 0、恰两候选: %+v", after)
	}
	if _, ok := byPerson(after, "甲"); ok {
		t.Fatalf("不应有甲候选: %+v", after.Disputes)
	}
	yi, _ := byPerson(after, "乙")
	bing, _ := byPerson(after, "丙")
	if yi.Content[0] != "B" || bing.Content[0] != "C" {
		t.Fatalf("乙 B / 丙 C: %+v %+v", yi, bing)
	}
}

// TestEditBaseStaleEditKeepsFollow：多行 splice 触发 ErrStaleEdit 时不得拆追随、改正文、动争议或 live。
func TestEditBaseStaleEditKeepsFollow(t *testing.T) {
	d := New("t")
	line := view(t, d).Lines[0].ID
	if err := d.Submit("甲", line, model.ActionEdit, []string{"y"}); err != nil {
		t.Fatal(err)
	}
	if err := d.Submit("乙", line, model.ActionEdit, []string{"x"}); err != nil {
		t.Fatal(err)
	}
	if err := d.SetSuspended("甲", line, model.ActionEdit, true); err != nil {
		t.Fatal(err)
	}
	if err := d.SetSuspended("乙", line, model.ActionEdit, true); err != nil {
		t.Fatal(err)
	}
	v := view(t, d)
	jia := byPersonMust(t, v, "甲")
	if _, err := d.RequestFollow("丁", jia.ID, 1); err != nil {
		t.Fatal(err)
	}
	if _, err := d.AnswerFollow("甲", "丁", jia.ID, true); err != nil {
		t.Fatal(err)
	}
	// 挂起例外下丙多行直写；splice live.before=""，丁再带 base=y 触发 ErrStaleEdit。
	tail := model.NewID()
	if err := d.SubmitWith("丙", line, model.ActionEdit, []string{"z", "extra"}, SubmitOpts{BaseContent: strPtr(""), LineIDs: []model.ID{tail}}); err != nil {
		t.Fatal(err)
	}
	live := d.live[claimKey{line, model.ActionEdit}]
	if live == nil || live.person != "丙" || !live.spliced || live.before != "" {
		t.Fatalf("前置：丙 splice live before=\"\": %+v", live)
	}

	err := d.SubmitWith("丁", line, model.ActionEdit, []string{"w"}, SubmitOpts{BaseContent: strPtr("y")})
	if err != ErrStaleEdit {
		t.Fatalf("应对齐失败返回 ErrStaleEdit: %v", err)
	}

	after := view(t, d)
	if after.Lines[0].Content != "z" || len(after.Lines) != 2 || after.Lines[1].Content != "extra" {
		t.Fatalf("不得改正文/拆行: %+v", after.Lines)
	}
	leader, _ := byPerson(after, "甲")
	if len(leader.Followers) != 1 || leader.Followers[0] != "丁" {
		t.Fatalf("失败不得取消追随: %+v", leader.Followers)
	}
	if len(after.Disputes) != 2 {
		t.Fatalf("不得改争议份数: %+v", after.Disputes)
	}
	live = d.live[claimKey{line, model.ActionEdit}]
	if live == nil || live.person != "丙" || !live.spliced || live.content[0] != "z" || live.before != "" {
		t.Fatalf("不得改 live: %+v", live)
	}
}

func TestEditBaseLegacyCurrentBody(t *testing.T) {
	a := model.Article{ID: model.NewID(), Title: "旧"}
	id := model.NewID()
	d, err := Load(a, []model.Line{{ID: id, Content: "y"}}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := d.SubmitWith("乙", id, model.ActionEdit, []string{"z"}, SubmitOpts{BaseContent: strPtr("x")}); err != nil {
		t.Fatal(err)
	}
	after := view(t, d)
	if after.Lines[0].Content != "x" || len(after.Disputes) != 2 {
		t.Fatalf("无作者时应用当前正文候选: %+v", after)
	}
	body, _ := byPerson(after, CurrentBodyPerson)
	yi, _ := byPerson(after, "乙")
	if body.Content[0] != "y" || yi.Content[0] != "z" {
		t.Fatalf("当前正文 y / 乙 z: %+v %+v", body, yi)
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

func TestPendingFollowSurvivesViewLoadAndReplay(t *testing.T) {
	d := twoClaims(t)
	v := view(t, d)
	p1, _ := byPerson(v, "p1")
	if _, err := d.RequestFollow("p2", p1.ID, 42); err != nil {
		t.Fatal(err)
	}
	mid := view(t, d)
	target, ok := byPerson(mid, "p1")
	if !ok || len(target.Pending) != 1 {
		t.Fatalf("待确认应挂在被追随方争议上: %+v", target)
	}
	p := target.Pending[0]
	if p.From != "p2" || p.To != "p1" || p.ClientTs != 42 {
		t.Fatalf("待确认字段不对: %+v", p)
	}
	other, _ := byPerson(mid, "p2")
	if len(other.Pending) != 0 {
		t.Fatalf("发起方争议不应挂待确认: %+v", other)
	}

	// 同请求重放不增加待确认
	if _, err := d.RequestFollow("p2", p1.ID, 42); err != nil {
		t.Fatal(err)
	}
	if n := len(byPersonMust(t, view(t, d), "p1").Pending); n != 1 {
		t.Fatalf("重放后仍应只有一条待确认，得到 %d", n)
	}

	var raws []bson.Raw
	for _, item := range []any{mid.Article, mid.Lines[0], mid.Disputes[0], mid.Disputes[1]} {
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
	restored, _ := byPerson(again, "p1")
	if len(restored.Pending) != 1 || restored.Pending[0].From != "p2" || restored.Pending[0].ClientTs != 42 {
		t.Fatalf("Load 应恢复待确认: %+v", restored)
	}
	got, err := loaded.AnswerFollow("p1", "p2", restored.ID, true)
	if err != nil || got.Status != FollowApplied {
		t.Fatalf("Load 后应能点头: %+v %v", got, err)
	}
	done := view(t, loaded)
	if len(done.Disputes) != 0 || done.Lines[0].Content != "a" {
		t.Fatalf("点头后争议应收束: %+v", done)
	}
}

func byPersonMust(t *testing.T, v View, person string) model.Dispute {
	t.Helper()
	item, ok := byPerson(v, person)
	if !ok {
		t.Fatalf("找不到 %s 的争议", person)
	}
	return item
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
	if err := d.DeleteIfIdle("p1", lines[1].ID, []Presence{{Person: "p2", Active: true}}); err != ErrNotEmpty {
		t.Fatalf("非空行应拒绝普通删除，得到 %v", err)
	}

	d = New("t")
	if err := d.EnsureLines(2); err != nil {
		t.Fatal(err)
	}
	lines = view(t, d).Lines
	if err := d.DeleteIfIdle("p1", lines[1].ID, []Presence{{Person: "p2", Active: true}}); err != nil {
		t.Fatal(err)
	}
	held := view(t, d)
	if len(held.Lines) != 2 || len(held.Disputes) != 2 {
		t.Fatalf("有人还在空行，应成删除主张: lines=%d disputes=%d", len(held.Lines), len(held.Disputes))
	}
	p1, _ := byPerson(held, "p1")
	p2, _ := byPerson(held, "p2")
	if p1.Action != model.ActionDelete || p2.Action != model.ActionEdit {
		t.Fatalf("删除主张对保留行: p1=%+v p2=%+v", p1, p2)
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

func TestDeleteClaimKeepsExistingEdits(t *testing.T) {
	d := twoClaims(t)
	line := view(t, d).Lines[0].ID
	if err := d.DeleteIfIdle("p3", line, nil); err != nil {
		t.Fatal(err)
	}
	v := view(t, d)
	if len(v.Disputes) != 3 {
		t.Fatalf("已有两份文字主张时第三人删空行，三份都应保留: %+v", v.Disputes)
	}
	p3, ok := byPerson(v, "p3")
	if !ok || p3.Action != model.ActionDelete {
		t.Fatalf("第三人应是删这行: %+v", p3)
	}
}

func TestDeleteDefersWhileInsertPending(t *testing.T) {
	d := New("t")
	if err := d.EnsureLines(2); err != nil {
		t.Fatal(err)
	}
	anchor := view(t, d).Lines[0].ID
	if err := d.Submit("p1", anchor, model.ActionInsert, []string{"x"}); err != nil {
		t.Fatal(err)
	}
	if err := d.Submit("p2", anchor, model.ActionInsert, []string{"y"}); err != nil {
		t.Fatal(err)
	}
	if err := d.DeleteIfIdle("p3", anchor, nil); err != nil {
		t.Fatal(err)
	}
	mid := view(t, d)
	var del, inserts int
	for _, item := range mid.Disputes {
		switch item.Action {
		case model.ActionDelete:
			del++
		case model.ActionInsert:
			inserts++
		}
	}
	if del != 1 || inserts != 2 {
		t.Fatalf("插入未决时删除只挂主张: del=%d inserts=%d", del, inserts)
	}
	if len(mid.Lines) < 2 {
		t.Fatalf("真实行应仍在: %+v", mid.Lines)
	}

	var yID model.ID
	for _, item := range mid.Disputes {
		if item.Person == "p2" && item.Action == model.ActionInsert {
			yID = item.ID
		}
	}
	if _, err := d.RequestFollow("p1", yID, 1); err != nil {
		t.Fatal(err)
	}
	if _, err := d.AnswerFollow("p2", "p1", yID, true); err != nil {
		t.Fatal(err)
	}
	after := view(t, d)
	for _, item := range after.Disputes {
		if item.Action == model.ActionInsert {
			t.Fatalf("插入应已决议: %+v", after.Disputes)
		}
	}
	// 插入收口后仅剩删除主张，应自动删掉空锚点；插入正文保留且链有效。
	if len(after.Disputes) != 0 {
		t.Fatalf("删除应随插入收口后自动决议: %+v", after.Disputes)
	}
	if len(after.Lines) < 1 {
		t.Fatal("链不应空")
	}
	found := false
	for _, ln := range after.Lines {
		if ln.Content == "y" {
			found = true
		}
	}
	if !found {
		t.Fatalf("胜出插入应留在正文: %+v", after.Lines)
	}
}

func TestFollowDeleteRemovesLine(t *testing.T) {
	d := New("t")
	if err := d.EnsureLines(2); err != nil {
		t.Fatal(err)
	}
	line := view(t, d).Lines[1].ID
	if err := d.DeleteIfIdle("p1", line, []Presence{{Person: "p2", Active: true}}); err != nil {
		t.Fatal(err)
	}
	v := view(t, d)
	p1, _ := byPerson(v, "p1")
	if p1.Action != model.ActionDelete {
		t.Fatalf("p1 应是删这行: %+v", p1)
	}
	if _, err := d.RequestFollow("p2", p1.ID, 1); err != nil {
		t.Fatal(err)
	}
	if _, err := d.AnswerFollow("p1", "p2", p1.ID, true); err != nil {
		t.Fatal(err)
	}
	after := view(t, d)
	if len(after.Lines) != 1 || len(after.Disputes) != 0 {
		t.Fatalf("跟随删除后正式行应删且链有效: %+v", after)
	}
	if !after.Lines[0].Prev.IsZero() {
		t.Fatal("首行前应为空")
	}
}

func TestMergeRefusesUnresolvedClaims(t *testing.T) {
	d := New("t")
	if err := d.EnsureLines(2); err != nil {
		t.Fatal(err)
	}
	second := view(t, d).Lines[1].ID
	if err := d.Submit("p1", second, model.ActionEdit, []string{"x"}); err != nil {
		t.Fatal(err)
	}
	if err := d.Submit("p2", second, model.ActionEdit, []string{"y"}); err != nil {
		t.Fatal(err)
	}
	before := view(t, d)
	if err := d.MergeUp("p3", second, nil); err != ErrHasClaims {
		t.Fatalf("有未决主张时合并应拒绝，得到 %v", err)
	}
	after := view(t, d)
	if len(after.Lines) != len(before.Lines) || len(after.Disputes) != len(before.Disputes) {
		t.Fatalf("拒绝合并应保全: before=%+v after=%+v", before, after)
	}
}

func TestIdleEmptyDeleteRemovesLine(t *testing.T) {
	d := New("t")
	if err := d.EnsureLines(2); err != nil {
		t.Fatal(err)
	}
	lines := view(t, d).Lines
	if err := d.DeleteIfIdle("p1", lines[1].ID, nil); err != nil {
		t.Fatal(err)
	}
	v := view(t, d)
	if len(v.Lines) != 1 || len(v.Disputes) != 0 {
		t.Fatalf("无争议空行应直接删: %+v", v)
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

func TestResolveEditDoesNotRevertInsertLive(t *testing.T) {
	d := twoClaims(t)
	line := view(t, d).Lines[0].ID
	if err := d.Submit("p3", line, model.ActionInsert, []string{"ins"}); err != nil {
		t.Fatal(err)
	}
	if _, ok := d.live[claimKey{line, model.ActionInsert}]; !ok {
		t.Fatal("应有插入 live")
	}
	v := view(t, d)
	p1, _ := byPerson(v, "p1")
	if _, err := d.RequestFollow("p2", p1.ID, 1); err != nil {
		t.Fatal(err)
	}
	if _, err := d.AnswerFollow("p1", "p2", p1.ID, true); err != nil {
		t.Fatal(err)
	}
	after := view(t, d)
	found := false
	for _, ln := range after.Lines {
		if ln.Content == "ins" {
			found = true
		}
	}
	if !found {
		t.Fatalf("编辑组决议不应回滚同锚点插入 live: %+v", after.Lines)
	}
}

func TestResolveInsertDoesNotRevertEditLive(t *testing.T) {
	d := New("t")
	line := view(t, d).Lines[0].ID
	if err := d.Submit("p1", line, model.ActionEdit, []string{"keep"}); err != nil {
		t.Fatal(err)
	}
	if err := d.Submit("p2", line, model.ActionInsert, []string{"x"}); err != nil {
		t.Fatal(err)
	}
	if err := d.Submit("p3", line, model.ActionInsert, []string{"y"}); err != nil {
		t.Fatal(err)
	}
	v := view(t, d)
	var yID model.ID
	for _, item := range v.Disputes {
		if item.Person == "p3" {
			yID = item.ID
		}
	}
	if _, err := d.RequestFollow("p2", yID, 1); err != nil {
		t.Fatal(err)
	}
	if _, err := d.AnswerFollow("p3", "p2", yID, true); err != nil {
		t.Fatal(err)
	}
	after := view(t, d)
	if after.Lines[0].Content != "keep" {
		t.Fatalf("插入组决议不应回滚编辑 live: %+v", after.Lines)
	}
}

func TestSubmitDeleteRespectsNonEmpty(t *testing.T) {
	d := New("t")
	if err := d.EnsureLines(2); err != nil {
		t.Fatal(err)
	}
	line := view(t, d).Lines[1].ID
	if err := d.Submit("p1", line, model.ActionEdit, []string{"cd"}); err != nil {
		t.Fatal(err)
	}
	if err := d.Submit("p2", line, model.ActionDelete, nil); err != ErrNotEmpty {
		t.Fatalf("Submit 删除非空行应拒绝，得到 %v", err)
	}
	// 有编辑争议时也不能靠 Submit 绕过非空校验
	d2 := twoClaims(t)
	l2 := view(t, d2).Lines[0].ID
	d2.lines[l2].Content = "still"
	if err := d2.Submit("p3", l2, model.ActionDelete, nil); err != ErrNotEmpty {
		t.Fatalf("有争议时 Submit 删除非空仍应拒绝，得到 %v", err)
	}
}

func TestClaimDeletePropagatesRevertError(t *testing.T) {
	d := New("t")
	if err := d.EnsureLines(2); err != nil {
		t.Fatal(err)
	}
	line := view(t, d).Lines[1].ID
	if err := d.Submit("p1", line, model.ActionEdit, []string{"hi", "there"}); err != nil {
		t.Fatal(err)
	}
	seg := d.lines[line].Next
	delete(d.lines, seg)
	if err := d.MergeUp("p2", line, []Presence{{Person: "p1", Active: true}}); err == nil {
		t.Fatal("revert 失败应返回错误")
	}
	for _, item := range d.disputes {
		if item.Action == model.ActionDelete {
			t.Fatalf("半提交：不应写入删除主张: %+v", item)
		}
	}
}

func TestMutualFollowEditAndDeleteSameSlot(t *testing.T) {
	d := New("t")
	if err := d.EnsureLines(2); err != nil {
		t.Fatal(err)
	}
	line := view(t, d).Lines[1].ID
	if err := d.DeleteIfIdle("p1", line, []Presence{{Person: "p2", Active: true}}); err != nil {
		t.Fatal(err)
	}
	v := view(t, d)
	var editID, delID model.ID
	for _, item := range v.Disputes {
		if item.Action == model.ActionEdit {
			editID = item.ID
		} else if item.Action == model.ActionDelete {
			delID = item.ID
		}
	}
	if editID.IsZero() || delID.IsZero() {
		t.Fatalf("需要改/删各一份: %+v", v.Disputes)
	}
	if _, err := d.RequestFollow("p1", editID, 20); err != nil {
		t.Fatal(err)
	}
	got, err := d.RequestFollow("p2", delID, 10)
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != FollowApplied || got.PeerStatus != FollowLost {
		t.Fatalf("编辑与删除互追应识别同一场: %+v", got)
	}
}

func TestDeleteClaimViewContentEmptyArray(t *testing.T) {
	d := New("t")
	if err := d.EnsureLines(2); err != nil {
		t.Fatal(err)
	}
	line := view(t, d).Lines[1].ID
	if err := d.DeleteIfIdle("p1", line, []Presence{{Person: "p2", Active: true}}); err != nil {
		t.Fatal(err)
	}
	v := view(t, d)
	p1, ok := byPerson(v, "p1")
	if !ok || p1.Action != model.ActionDelete {
		t.Fatalf("应有删除主张: %+v", v.Disputes)
	}
	if p1.Content == nil {
		t.Fatal("View 删除主张 Content 不能是 nil")
	}
	if len(p1.Content) != 0 {
		t.Fatalf("删除主张 Content 应为空数组: %#v", p1.Content)
	}
	raw, err := json.Marshal(p1)
	if err != nil {
		t.Fatal(err)
	}
	if !json.Valid(raw) || !containsBytes(raw, []byte(`"content":[]`)) {
		t.Fatalf("JSON 应是 content:[], 得到 %s", raw)
	}
	b, err := bson.Marshal(p1)
	if err != nil {
		t.Fatal(err)
	}
	var again model.Dispute
	if err := bson.Unmarshal(b, &again); err != nil {
		t.Fatal(err)
	}
	if again.Content == nil {
		t.Fatal("BSON 往返后 Content 不能是 nil")
	}
}

func TestDeleteDefersForLiveInsert(t *testing.T) {
	d := New("t")
	if err := d.EnsureLines(2); err != nil {
		t.Fatal(err)
	}
	anchor := view(t, d).Lines[0].ID
	if err := d.Submit("p1", anchor, model.ActionInsert, []string{"ins"}); err != nil {
		t.Fatal(err)
	}
	if _, ok := d.live[claimKey{anchor, model.ActionInsert}]; !ok {
		t.Fatal("前置：应有插入 live")
	}
	before := view(t, d)
	if err := d.DeleteIfIdle("p2", anchor, nil); err != nil {
		t.Fatal(err)
	}
	after := view(t, d)
	if _, ok := d.live[claimKey{anchor, model.ActionInsert}]; !ok {
		t.Fatal("有 live 插入时删锚点应暂缓，live 仍在")
	}
	found := false
	for _, ln := range after.Lines {
		if ln.Content == "ins" {
			found = true
		}
	}
	if !found {
		t.Fatalf("插入正文不应丢失: before=%d after=%+v", len(before.Lines), after.Lines)
	}
	var del int
	for _, item := range after.Disputes {
		if item.Action == model.ActionDelete {
			del++
		}
	}
	if del != 1 {
		t.Fatalf("应挂删除主张且不决议: %+v", after.Disputes)
	}
}

func TestNonEmptyDeleteKeepsFollow(t *testing.T) {
	d := New("t")
	line := view(t, d).Lines[0].ID
	for _, pair := range []struct{ p, text string }{{"p1", "a"}, {"p2", "b"}, {"p3", "c"}} {
		if err := d.Submit(pair.p, line, model.ActionEdit, []string{pair.text}); err != nil {
			t.Fatal(err)
		}
	}
	v := view(t, d)
	p1, _ := byPerson(v, "p1")
	if _, err := d.RequestFollow("p3", p1.ID, 1); err != nil {
		t.Fatal(err)
	}
	if _, err := d.AnswerFollow("p1", "p3", p1.ID, true); err != nil {
		t.Fatal(err)
	}
	mid := view(t, d)
	leader, _ := byPerson(mid, "p1")
	if len(leader.Followers) != 1 || leader.Followers[0] != "p3" {
		t.Fatalf("前置追随: %+v", leader.Followers)
	}
	d.lines[line].Content = "still"
	if err := d.Submit("p3", line, model.ActionDelete, nil); err != ErrNotEmpty {
		t.Fatalf("非空应失败，得到 %v", err)
	}
	after := view(t, d)
	leader, _ = byPerson(after, "p1")
	if len(leader.Followers) != 1 || leader.Followers[0] != "p3" {
		t.Fatalf("非空删失败应保持追随: %+v", leader.Followers)
	}
}

func TestDeleteClaimDetachesPriorFollow(t *testing.T) {
	d := New("t")
	line := view(t, d).Lines[0].ID
	for _, pair := range []struct{ p, text string }{{"p1", "a"}, {"p2", "b"}, {"p3", "c"}} {
		if err := d.Submit(pair.p, line, model.ActionEdit, []string{pair.text}); err != nil {
			t.Fatal(err)
		}
	}
	v := view(t, d)
	p1, _ := byPerson(v, "p1")
	if _, err := d.RequestFollow("p3", p1.ID, 1); err != nil {
		t.Fatal(err)
	}
	if _, err := d.AnswerFollow("p1", "p3", p1.ID, true); err != nil {
		t.Fatal(err)
	}
	mid := view(t, d)
	leader, _ := byPerson(mid, "p1")
	if len(leader.Followers) != 1 || leader.Followers[0] != "p3" {
		t.Fatalf("前置：p3 应追随 p1: %+v", leader)
	}
	if err := d.DeleteIfIdle("p3", line, nil); err != nil {
		t.Fatal(err)
	}
	after := view(t, d)
	leader, _ = byPerson(after, "p1")
	if len(leader.Followers) != 0 {
		t.Fatalf("提出删除主张应取消追随: %+v", leader.Followers)
	}
	p3, ok := byPerson(after, "p3")
	if !ok || p3.Action != model.ActionDelete {
		t.Fatalf("p3 应是删除主张: %+v", after.Disputes)
	}
}

func containsBytes(b, sub []byte) bool {
	return bytes.Contains(b, sub)
}

func idPtr(id model.ID) *model.ID { return &id }

func textsOf(lines []model.Line) []string {
	out := make([]string, len(lines))
	for i, ln := range lines {
		out[i] = ln.Content
	}
	return out
}

func TestSyncedInsertStacksBeforeFirst(t *testing.T) {
	d := New("t")
	if err := d.EnsureLines(2); err != nil {
		t.Fatal(err)
	}
	before := view(t, d)
	anchor, tail := before.Lines[0].ID, before.Lines[1].ID
	a1, a2 := model.NewID(), model.NewID()
	if err := d.SubmitWith("甲", anchor, model.ActionInsert, []string{"A1", "A2"}, SubmitOpts{
		AfterSeen: idPtr(tail),
		LineIDs:   []model.ID{a1, a2},
	}); err != nil {
		t.Fatal(err)
	}
	c1, c2 := model.NewID(), model.NewID()
	if err := d.SubmitWith("丙", anchor, model.ActionInsert, []string{"C1", "C2"}, SubmitOpts{
		AfterSeen: idPtr(a1),
		LineIDs:   []model.ID{c1, c2},
	}); err != nil {
		t.Fatal(err)
	}
	after := view(t, d)
	if len(after.Disputes) != 0 {
		t.Fatalf("已同步堆叠不应拉争议: %+v", after.Disputes)
	}
	want := []string{"", "C1", "C2", "A1", "A2", ""}
	if got := textsOf(after.Lines); len(got) != 6 || got[1] != "C1" || got[2] != "C2" || got[3] != "A1" || got[4] != "A2" {
		t.Fatalf("顺序应为锚点,C1,C2,A1,A2,原后文: %v want前缀 %v", got, want)
	}
	if after.Lines[0].ID != anchor || after.Lines[5].ID != tail {
		t.Fatalf("两端旧行应保留: %+v", after.Lines)
	}
	if after.Lines[1].ID != c1 || after.Lines[3].ID != a1 {
		t.Fatalf("应使用预生 ID: %+v", after.Lines)
	}
}

func TestUnsyncedInsertOpensDispute(t *testing.T) {
	d := New("t")
	if err := d.EnsureLines(2); err != nil {
		t.Fatal(err)
	}
	before := view(t, d)
	anchor, tail := before.Lines[0].ID, before.Lines[1].ID
	if err := d.SubmitWith("甲", anchor, model.ActionInsert, []string{"A1", "A2"}, SubmitOpts{
		AfterSeen: idPtr(tail),
	}); err != nil {
		t.Fatal(err)
	}
	if err := d.SubmitWith("丙", anchor, model.ActionInsert, []string{"C1", "C2"}, SubmitOpts{
		AfterSeen: idPtr(tail),
	}); err != nil {
		t.Fatal(err)
	}
	after := view(t, d)
	if len(after.Lines) != 2 || after.Lines[0].Next != tail {
		t.Fatalf("未同步应回到原后文: %+v", after.Lines)
	}
	if len(after.Disputes) != 2 {
		t.Fatalf("应两份整段争议: %+v", after.Disputes)
	}
}

func TestStackedStaleOpensLayeredDispute(t *testing.T) {
	d := New("t")
	if err := d.EnsureLines(2); err != nil {
		t.Fatal(err)
	}
	before := view(t, d)
	anchor, tail := before.Lines[0].ID, before.Lines[1].ID
	a1, a2 := model.NewID(), model.NewID()
	if err := d.SubmitWith("甲", anchor, model.ActionInsert, []string{"A1", "A2"}, SubmitOpts{
		AfterSeen: idPtr(tail),
		LineIDs:   []model.ID{a1, a2},
	}); err != nil {
		t.Fatal(err)
	}
	c1, c2 := model.NewID(), model.NewID()
	if err := d.SubmitWith("丙", anchor, model.ActionInsert, []string{"C1", "C2"}, SubmitOpts{
		AfterSeen: idPtr(a1),
		LineIDs:   []model.ID{c1, c2},
	}); err != nil {
		t.Fatal(err)
	}
	if err := d.SubmitWith("丁", anchor, model.ActionInsert, []string{"D1"}, SubmitOpts{
		AfterSeen: idPtr(tail),
	}); err != nil {
		t.Fatal(err)
	}
	after := view(t, d)
	if len(after.Lines) != 2 || after.Lines[0].Next != tail {
		t.Fatalf("应收回堆叠段: %+v", after.Lines)
	}
	if len(after.Disputes) != 3 {
		t.Fatalf("甲/丙层叠/丁 三份: %+v", after.Disputes)
	}
	jia, _ := byPerson(after, "甲")
	bing, _ := byPerson(after, "丙")
	ding, _ := byPerson(after, "丁")
	if len(jia.Content) != 2 || jia.Content[0] != "A1" || jia.Content[1] != "A2" {
		t.Fatalf("甲候选 A: %+v", jia.Content)
	}
	if len(bing.Content) != 4 || bing.Content[0] != "C1" || bing.Content[1] != "C2" || bing.Content[2] != "A1" || bing.Content[3] != "A2" {
		t.Fatalf("丙候选 C+A: %+v", bing.Content)
	}
	if len(ding.Content) != 1 || ding.Content[0] != "D1" {
		t.Fatalf("丁候选 D: %+v", ding.Content)
	}
}

func TestStackedSameContentNoDispute(t *testing.T) {
	d := New("t")
	if err := d.EnsureLines(2); err != nil {
		t.Fatal(err)
	}
	before := view(t, d)
	anchor, tail := before.Lines[0].ID, before.Lines[1].ID
	a1 := model.NewID()
	if err := d.SubmitWith("甲", anchor, model.ActionInsert, []string{"A"}, SubmitOpts{
		AfterSeen: idPtr(tail),
		LineIDs:   []model.ID{a1},
	}); err != nil {
		t.Fatal(err)
	}
	c1 := model.NewID()
	if err := d.SubmitWith("丙", anchor, model.ActionInsert, []string{"C"}, SubmitOpts{
		AfterSeen: idPtr(a1),
		LineIDs:   []model.ID{c1},
	}); err != nil {
		t.Fatal(err)
	}
	mid := view(t, d)
	if err := d.SubmitWith("丁", anchor, model.ActionInsert, []string{"C", "A"}, SubmitOpts{
		AfterSeen: idPtr(tail),
	}); err != nil {
		t.Fatal(err)
	}
	after := view(t, d)
	if len(after.Disputes) != 0 {
		t.Fatalf("与当前块相同不开争议: %+v", after.Disputes)
	}
	if textsOf(after.Lines)[1] != "C" || textsOf(after.Lines)[2] != "A" {
		t.Fatalf("链应不变: mid=%v after=%v", textsOf(mid.Lines), textsOf(after.Lines))
	}
}

func TestStackedFollowLayeredWins(t *testing.T) {
	d := New("t")
	if err := d.EnsureLines(2); err != nil {
		t.Fatal(err)
	}
	before := view(t, d)
	anchor, tail := before.Lines[0].ID, before.Lines[1].ID
	a1 := model.NewID()
	if err := d.SubmitWith("甲", anchor, model.ActionInsert, []string{"A"}, SubmitOpts{
		AfterSeen: idPtr(tail),
		LineIDs:   []model.ID{a1},
	}); err != nil {
		t.Fatal(err)
	}
	c1 := model.NewID()
	if err := d.SubmitWith("丙", anchor, model.ActionInsert, []string{"C"}, SubmitOpts{
		AfterSeen: idPtr(a1),
		LineIDs:   []model.ID{c1},
	}); err != nil {
		t.Fatal(err)
	}
	if err := d.SubmitWith("丁", anchor, model.ActionInsert, []string{"D"}, SubmitOpts{
		AfterSeen: idPtr(tail),
	}); err != nil {
		t.Fatal(err)
	}
	v := view(t, d)
	bing, _ := byPerson(v, "丙")
	jia, _ := byPerson(v, "甲")
	ding, _ := byPerson(v, "丁")
	if _, err := d.RequestFollow("甲", bing.ID, 1); err != nil {
		t.Fatal(err)
	}
	if _, err := d.AnswerFollow("丙", "甲", bing.ID, true); err != nil {
		t.Fatal(err)
	}
	if _, err := d.RequestFollow("丁", bing.ID, 2); err != nil {
		t.Fatal(err)
	}
	if _, err := d.AnswerFollow("丙", "丁", bing.ID, true); err != nil {
		t.Fatal(err)
	}
	_ = jia
	_ = ding
	after := view(t, d)
	if len(after.Disputes) != 0 {
		t.Fatalf("追随收口后应无争议: %+v", after.Disputes)
	}
	got := textsOf(after.Lines)
	if len(got) != 4 || got[1] != "C" || got[2] != "A" || after.Lines[3].ID != tail {
		t.Fatalf("胜出 C+A 后链应为 X,C,A,T: %+v", after.Lines)
	}
}

func TestStackedArrivalOrderSwapKeepsText(t *testing.T) {
	// 丁先于丙的同步堆叠到达：未同步争议保留甲/丁文字。
	d := New("t")
	if err := d.EnsureLines(2); err != nil {
		t.Fatal(err)
	}
	before := view(t, d)
	anchor, tail := before.Lines[0].ID, before.Lines[1].ID
	a1 := model.NewID()
	if err := d.SubmitWith("甲", anchor, model.ActionInsert, []string{"A"}, SubmitOpts{
		AfterSeen: idPtr(tail),
		LineIDs:   []model.ID{a1},
	}); err != nil {
		t.Fatal(err)
	}
	if err := d.SubmitWith("丁", anchor, model.ActionInsert, []string{"D"}, SubmitOpts{
		AfterSeen: idPtr(tail),
	}); err != nil {
		t.Fatal(err)
	}
	v := view(t, d)
	jia, _ := byPerson(v, "甲")
	ding, _ := byPerson(v, "丁")
	if len(jia.Content) != 1 || jia.Content[0] != "A" || len(ding.Content) != 1 || ding.Content[0] != "D" {
		t.Fatalf("对调到达不得丢字: %+v", v.Disputes)
	}
	if len(v.Lines) != 2 || v.Lines[0].Next != tail {
		t.Fatalf("应收回收尾: %+v", v.Lines)
	}
}

func TestLineIDsAllowImmediateEdit(t *testing.T) {
	d := New("t")
	if err := d.EnsureLines(1); err != nil {
		t.Fatal(err)
	}
	anchor := view(t, d).Lines[0].ID
	newID := model.NewID()
	empty := model.ID{}
	if err := d.SubmitWith("甲", anchor, model.ActionInsert, []string{"hello"}, SubmitOpts{
		AfterSeen: idPtr(empty),
		LineIDs:   []model.ID{newID},
	}); err != nil {
		t.Fatal(err)
	}
	if err := d.Submit("甲", newID, model.ActionEdit, []string{"hello!"}); err != nil {
		t.Fatal(err)
	}
	after := view(t, d)
	if len(after.Lines) != 2 || after.Lines[1].ID != newID || after.Lines[1].Content != "hello!" {
		t.Fatalf("预生 ID 应可立即续写: %+v", after.Lines)
	}
}

func TestSuspendSplicedInsertKeepsHistory(t *testing.T) {
	d := New("t")
	if err := d.EnsureLines(2); err != nil {
		t.Fatal(err)
	}
	before := view(t, d)
	anchor, tail := before.Lines[0].ID, before.Lines[1].ID
	a1 := model.NewID()
	if err := d.SubmitWith("甲", anchor, model.ActionInsert, []string{"A1"}, SubmitOpts{
		AfterSeen: idPtr(tail),
		LineIDs:   []model.ID{a1},
	}); err != nil {
		t.Fatal(err)
	}
	if err := d.SetSuspended("甲", anchor, model.ActionInsert, true); err != nil {
		t.Fatal(err)
	}
	if err := d.SubmitWith("丙", anchor, model.ActionInsert, []string{"C1"}, SubmitOpts{
		AfterSeen: idPtr(tail),
	}); err != nil {
		t.Fatal(err)
	}
	after := view(t, d)
	if len(after.Lines) != 2 || after.Lines[0].Next != tail {
		t.Fatalf("归档单段未同步应收回正文: %+v", after.Lines)
	}
	if len(after.Disputes) != 2 {
		t.Fatalf("甲归档段与丙应两份争议: %+v", after.Disputes)
	}
}

func TestPromoteChildEditOnUnsyncedInsert(t *testing.T) {
	d := New("t")
	if err := d.EnsureLines(2); err != nil {
		t.Fatal(err)
	}
	before := view(t, d)
	anchor, tail := before.Lines[0].ID, before.Lines[1].ID
	a1, a2 := model.NewID(), model.NewID()
	if err := d.SubmitWith("甲", anchor, model.ActionInsert, []string{"A1", "A2"}, SubmitOpts{
		AfterSeen: idPtr(tail),
		LineIDs:   []model.ID{a1, a2},
	}); err != nil {
		t.Fatal(err)
	}
	if err := d.Submit("乙", a2, model.ActionEdit, []string{"A2改"}); err != nil {
		t.Fatal(err)
	}
	if err := d.SubmitWith("丙", anchor, model.ActionInsert, []string{"C1"}, SubmitOpts{
		AfterSeen: idPtr(tail),
	}); err != nil {
		t.Fatal(err)
	}
	after := view(t, d)
	if len(after.Lines) != 2 || after.Lines[0].Next != tail {
		t.Fatalf("应收回插入段: %+v", after.Lines)
	}
	if len(after.Disputes) != 3 {
		t.Fatalf("甲乙丙三份整段争议: %+v", after.Disputes)
	}
	jia, _ := byPerson(after, "甲")
	yi, _ := byPerson(after, "乙")
	bing, _ := byPerson(after, "丙")
	if jia.Action != model.ActionInsert || yi.Action != model.ActionInsert || bing.Action != model.ActionInsert {
		t.Fatalf("均应为插在后面: %+v", after.Disputes)
	}
	if len(jia.Content) != 2 || jia.Content[0] != "A1" || jia.Content[1] != "A2" {
		t.Fatalf("甲保留原段: %+v", jia.Content)
	}
	if len(yi.Content) != 2 || yi.Content[0] != "A1" || yi.Content[1] != "A2改" {
		t.Fatalf("乙整段含改字: %+v", yi.Content)
	}
	if len(bing.Content) != 1 || bing.Content[0] != "C1" {
		t.Fatalf("丙新段: %+v", bing.Content)
	}
}

func TestPromoteSuspendedChildEdit(t *testing.T) {
	d := New("t")
	if err := d.EnsureLines(2); err != nil {
		t.Fatal(err)
	}
	before := view(t, d)
	anchor, tail := before.Lines[0].ID, before.Lines[1].ID
	a1, a2 := model.NewID(), model.NewID()
	if err := d.SubmitWith("甲", anchor, model.ActionInsert, []string{"A1", "A2"}, SubmitOpts{
		AfterSeen: idPtr(tail),
		LineIDs:   []model.ID{a1, a2},
	}); err != nil {
		t.Fatal(err)
	}
	if err := d.Submit("乙", a2, model.ActionEdit, []string{"挂起后"}); err != nil {
		t.Fatal(err)
	}
	if err := d.SetSuspended("乙", a2, model.ActionEdit, true); err != nil {
		t.Fatal(err)
	}
	mid := view(t, d)
	yiEdit, ok := byPerson(mid, "乙")
	if !ok || yiEdit.Action != model.ActionEdit {
		t.Fatalf("前置：乙应有挂起的改这行: %+v", mid.Disputes)
	}
	oldID := yiEdit.ID
	if err := d.SubmitWith("丙", anchor, model.ActionInsert, []string{"C1"}, SubmitOpts{
		AfterSeen: idPtr(tail),
	}); err != nil {
		t.Fatal(err)
	}
	after := view(t, d)
	if len(after.Disputes) != 3 {
		t.Fatalf("三份整段: %+v", after.Disputes)
	}
	yi, ok := byPerson(after, "乙")
	if !ok || yi.Action != model.ActionInsert || yi.ID != oldID {
		t.Fatalf("乙应保留原争议 ID 并升为插在后面: %+v", after.Disputes)
	}
	if !slicesContains(after.Suspended, oldID.Hex()) {
		t.Fatalf("挂起状态应保留: %+v", after.Suspended)
	}
	if len(yi.Content) != 2 || yi.Content[1] != "挂起后" {
		t.Fatalf("乙整段内容: %+v", yi.Content)
	}
}

func TestPromoteUnsafePasteRejected(t *testing.T) {
	d := New("t")
	if err := d.EnsureLines(2); err != nil {
		t.Fatal(err)
	}
	before := view(t, d)
	anchor, tail := before.Lines[0].ID, before.Lines[1].ID
	a1, a2 := model.NewID(), model.NewID()
	if err := d.SubmitWith("甲", anchor, model.ActionInsert, []string{"A1", "A2"}, SubmitOpts{
		AfterSeen: idPtr(tail),
		LineIDs:   []model.ID{a1, a2},
	}); err != nil {
		t.Fatal(err)
	}
	if err := d.Submit("乙", a2, model.ActionEdit, []string{"x", "y"}); err != nil {
		t.Fatal(err)
	}
	mid := view(t, d)
	midTexts := textsOf(mid.Lines)
	err := d.SubmitWith("丙", anchor, model.ActionInsert, []string{"C1"}, SubmitOpts{
		AfterSeen: idPtr(tail),
	})
	if err != ErrPromoteUnsafe {
		t.Fatalf("多行粘贴依赖应拒绝: %v", err)
	}
	after := view(t, d)
	if len(after.Lines) != len(mid.Lines) || len(after.Disputes) != len(mid.Disputes) {
		t.Fatalf("拒绝后状态应不变 lines=%d→%d disputes=%d→%d",
			len(mid.Lines), len(after.Lines), len(mid.Disputes), len(after.Disputes))
	}
	got := textsOf(after.Lines)
	for i := range midTexts {
		if got[i] != midTexts[i] {
			t.Fatalf("正文应完整保留: mid=%v after=%v", midTexts, got)
		}
	}
	if midTexts[1] != "A1" || midTexts[2] != "x" || midTexts[3] != "y" {
		t.Fatalf("前置粘贴链应为 A1,x,y: %v", midTexts)
	}
}

func TestCollidingLineIDsNoSideEffects(t *testing.T) {
	d := New("t")
	if err := d.EnsureLines(2); err != nil {
		t.Fatal(err)
	}
	before := view(t, d)
	anchor, tail := before.Lines[0].ID, before.Lines[1].ID
	if err := d.Submit("p1", anchor, model.ActionEdit, []string{"e1"}); err != nil {
		t.Fatal(err)
	}
	if err := d.Submit("p2", anchor, model.ActionEdit, []string{"e2"}); err != nil {
		t.Fatal(err)
	}
	v := view(t, d)
	p1, _ := byPerson(v, "p1")
	if _, err := d.RequestFollow("丁", p1.ID, 1); err != nil {
		t.Fatal(err)
	}
	if _, err := d.AnswerFollow("p1", "丁", p1.ID, true); err != nil {
		t.Fatal(err)
	}
	a1, a2 := model.NewID(), model.NewID()
	if err := d.SubmitWith("甲", anchor, model.ActionInsert, []string{"A1", "A2"}, SubmitOpts{
		AfterSeen: idPtr(tail),
		LineIDs:   []model.ID{a1, a2},
	}); err != nil {
		t.Fatal(err)
	}
	live := d.live[claimKey{anchor, model.ActionInsert}]
	if live == nil || live.person != "甲" {
		t.Fatal("前置：甲应有 live 插入")
	}
	collide := model.NewID()
	err := d.SubmitWith("丙", anchor, model.ActionInsert, []string{"C1", "C2"}, SubmitOpts{
		AfterSeen: idPtr(a1),
		LineIDs:   []model.ID{collide, a1}, // a1 已在链上
	})
	if err != ErrLineIDs {
		t.Fatalf("冲突 LineIDs 应失败: %v", err)
	}
	after := view(t, d)
	leader, _ := byPerson(after, "p1")
	if len(leader.Followers) != 1 || leader.Followers[0] != "丁" {
		t.Fatalf("不得取消追随: %+v", leader.Followers)
	}
	live = d.live[claimKey{anchor, model.ActionInsert}]
	if live == nil || live.person != "甲" {
		t.Fatalf("不得归档甲的 live")
	}
	if len(d.insertHistory[claimKey{anchor, model.ActionInsert}]) != 0 {
		t.Fatalf("不得写入 insertHistory: %d", len(d.insertHistory[claimKey{anchor, model.ActionInsert}]))
	}
}

func TestBrokenSegmentPromoteNoMutation(t *testing.T) {
	d := New("t")
	if err := d.EnsureLines(2); err != nil {
		t.Fatal(err)
	}
	before := view(t, d)
	anchor, tail := before.Lines[0].ID, before.Lines[1].ID
	a1, a2 := model.NewID(), model.NewID()
	if err := d.SubmitWith("甲", anchor, model.ActionInsert, []string{"A1", "A2"}, SubmitOpts{
		AfterSeen: idPtr(tail),
		LineIDs:   []model.ID{a1, a2},
	}); err != nil {
		t.Fatal(err)
	}
	if err := d.Submit("乙", a2, model.ActionEdit, []string{"改"}); err != nil {
		t.Fatal(err)
	}
	mid := view(t, d)
	disputeN := len(d.disputes)
	// 段首 Prev 脱离锚点，oldNext 仍正确 → AfterSeen 核对通过，unlink 预检失败。
	d.lines[a1].Prev = model.NewID()
	err := d.SubmitWith("丙", anchor, model.ActionInsert, []string{"C1"}, SubmitOpts{
		AfterSeen: idPtr(tail),
	})
	if err != ErrBroken {
		t.Fatalf("坏段链应拒绝提升: %v", err)
	}
	if len(d.disputes) != disputeN {
		t.Fatalf("不得改争议: mid=%d after=%d", disputeN, len(d.disputes))
	}
	if d.live[claimKey{anchor, model.ActionInsert}] == nil || d.live[claimKey{a2, model.ActionEdit}] == nil {
		t.Fatal("live 主张应仍在")
	}
	if d.lines[a2].Content != "改" || d.lines[a1].Content != "A1" {
		t.Fatalf("正文应不变: a1=%q a2=%q", d.lines[a1].Content, d.lines[a2].Content)
	}
	_ = mid
}

func TestResubmitOldLineIDsNoOverwrite(t *testing.T) {
	d := New("t")
	if err := d.EnsureLines(2); err != nil {
		t.Fatal(err)
	}
	before := view(t, d)
	anchor, tail := before.Lines[0].ID, before.Lines[1].ID
	a1, a2 := model.NewID(), model.NewID()
	if err := d.SubmitWith("甲", anchor, model.ActionInsert, []string{"A1", "A2"}, SubmitOpts{
		AfterSeen: idPtr(tail),
		LineIDs:   []model.ID{a1, a2},
	}); err != nil {
		t.Fatal(err)
	}
	c1, c2 := model.NewID(), model.NewID()
	if err := d.SubmitWith("丙", anchor, model.ActionInsert, []string{"C1", "C2"}, SubmitOpts{
		AfterSeen: idPtr(a1),
		LineIDs:   []model.ID{c1, c2},
	}); err != nil {
		t.Fatal(err)
	}
	mid := view(t, d)
	if err := d.SubmitWith("甲", anchor, model.ActionInsert, []string{"A1", "A2"}, SubmitOpts{
		AfterSeen: idPtr(tail),
		LineIDs:   []model.ID{a1, a2},
	}); err != nil {
		t.Fatal(err)
	}
	after := view(t, d)
	if len(after.Lines) != len(mid.Lines) || after.Lines[1].ID != c1 {
		t.Fatalf("重发旧 ID 不得覆盖新段: mid=%v after=%v", mid.Lines, after.Lines)
	}
	if d.live[claimKey{anchor, model.ActionInsert}] == nil || d.live[claimKey{anchor, model.ActionInsert}].person != "丙" {
		t.Fatalf("最新 live 仍应是丙")
	}
}

func TestStaleRejectDoesNotDetachFollow(t *testing.T) {
	d := New("t")
	if err := d.EnsureLines(2); err != nil {
		t.Fatal(err)
	}
	before := view(t, d)
	anchor, tail := before.Lines[0].ID, before.Lines[1].ID
	if err := d.SubmitWith("甲", anchor, model.ActionInsert, []string{"A1"}, SubmitOpts{AfterSeen: idPtr(tail)}); err != nil {
		t.Fatal(err)
	}
	if err := d.SubmitWith("乙", anchor, model.ActionInsert, []string{"B1"}, SubmitOpts{AfterSeen: idPtr(tail)}); err != nil {
		t.Fatal(err)
	}
	v := view(t, d)
	jia, _ := byPerson(v, "甲")
	if _, err := d.RequestFollow("丁", jia.ID, 1); err != nil {
		t.Fatal(err)
	}
	if _, err := d.AnswerFollow("甲", "丁", jia.ID, true); err != nil {
		t.Fatal(err)
	}
	mid := view(t, d)
	leader, _ := byPerson(mid, "甲")
	if len(leader.Followers) != 1 || leader.Followers[0] != "丁" {
		t.Fatalf("前置追随: %+v", leader)
	}
	err := d.SubmitWith("丁", anchor, model.ActionInsert, []string{"D1"}, SubmitOpts{
		AfterSeen: idPtr(model.NewID()),
	})
	if err != ErrStaleInsert {
		t.Fatalf("无匹配段时应拒绝: %v", err)
	}
	after := view(t, d)
	leader, _ = byPerson(after, "甲")
	if len(leader.Followers) != 1 || leader.Followers[0] != "丁" {
		t.Fatalf("过期拒绝不得取消追随: %+v", leader.Followers)
	}
}

func slicesContains(ss []string, want string) bool {
	for _, s := range ss {
		if s == want {
			return true
		}
	}
	return false
}

func roundTripLoad(t *testing.T, d *Doc) *Doc {
	t.Helper()
	v := view(t, d)
	raws := make([]bson.Raw, 0, 1+len(v.Lines)+len(v.Disputes))
	for _, item := range append([]any{v.Article}, append(anySliceLines(v.Lines), anySliceDisputes(v.Disputes)...)...) {
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
	return loaded
}

func anySliceLines(lines []model.Line) []any {
	out := make([]any, len(lines))
	for i := range lines {
		out[i] = lines[i]
	}
	return out
}

func anySliceDisputes(ds []model.Dispute) []any {
	out := make([]any, len(ds))
	for i := range ds {
		out[i] = ds[i]
	}
	return out
}

func TestOriginRoundTripPromoteThreeWays(t *testing.T) {
	d := New("t")
	if err := d.EnsureLines(2); err != nil {
		t.Fatal(err)
	}
	before := view(t, d)
	anchor, tail := before.Lines[0].ID, before.Lines[1].ID
	a1, a2 := model.NewID(), model.NewID()
	if err := d.SubmitWith("甲", anchor, model.ActionInsert, []string{"A1", "A2"}, SubmitOpts{
		AfterSeen: idPtr(tail),
		LineIDs:   []model.ID{a1, a2},
	}); err != nil {
		t.Fatal(err)
	}
	if err := d.Submit("乙", a2, model.ActionEdit, []string{"B"}); err != nil {
		t.Fatal(err)
	}
	mid := view(t, d)
	if mid.Lines[1].InsertOrigin == nil || mid.Lines[1].InsertOrigin.Person != "甲" {
		t.Fatalf("段首应有插入来源: %+v", mid.Lines[1])
	}
	if mid.Lines[2].RecentEdit == nil || mid.Lines[2].RecentEdit.Person != "乙" || mid.Lines[2].RecentEdit.Before != "A2" {
		t.Fatalf("A2 应有最近编辑: %+v", mid.Lines[2])
	}
	// 空串编辑前也要能进 BSON。
	emptyBefore, err := bson.Marshal(model.RecentEdit{Person: "测", Before: ""})
	if err != nil || !containsBytes(emptyBefore, []byte("编辑前")) {
		t.Fatalf("编辑前空串应写出: %v %s", err, emptyBefore)
	}

	loaded := roundTripLoad(t, d)
	if err := loaded.SubmitWith("丙", anchor, model.ActionInsert, []string{"C"}, SubmitOpts{
		AfterSeen: idPtr(tail),
	}); err != nil {
		t.Fatal(err)
	}
	after := view(t, loaded)
	if len(after.Lines) != 2 || after.Lines[0].Next != tail {
		t.Fatalf("应收回插入段: %+v", after.Lines)
	}
	if len(after.Disputes) != 3 {
		t.Fatalf("甲乙丙三份整段: %+v", after.Disputes)
	}
	jia, _ := byPerson(after, "甲")
	yi, _ := byPerson(after, "乙")
	bing, _ := byPerson(after, "丙")
	if len(jia.Content) != 2 || jia.Content[0] != "A1" || jia.Content[1] != "A2" {
		t.Fatalf("甲原段: %+v", jia.Content)
	}
	if len(yi.Content) != 2 || yi.Content[0] != "A1" || yi.Content[1] != "B" {
		t.Fatalf("乙整段: %+v", yi.Content)
	}
	if len(bing.Content) != 1 || bing.Content[0] != "C" {
		t.Fatalf("丙新段: %+v", bing.Content)
	}
}

func TestOriginRoundTripStackedDispute(t *testing.T) {
	d := New("t")
	if err := d.EnsureLines(2); err != nil {
		t.Fatal(err)
	}
	before := view(t, d)
	anchor, tail := before.Lines[0].ID, before.Lines[1].ID
	a1, a2 := model.NewID(), model.NewID()
	if err := d.SubmitWith("甲", anchor, model.ActionInsert, []string{"A1", "A2"}, SubmitOpts{
		AfterSeen: idPtr(tail),
		LineIDs:   []model.ID{a1, a2},
	}); err != nil {
		t.Fatal(err)
	}
	c1, c2 := model.NewID(), model.NewID()
	if err := d.SubmitWith("丙", anchor, model.ActionInsert, []string{"C1", "C2"}, SubmitOpts{
		AfterSeen: idPtr(a1),
		LineIDs:   []model.ID{c1, c2},
	}); err != nil {
		t.Fatal(err)
	}
	loaded := roundTripLoad(t, d)
	again := view(t, loaded)
	if again.Lines[1].InsertOrigin == nil || again.Lines[1].InsertOrigin.Person != "丙" {
		t.Fatalf("新段段首来源: %+v", again.Lines[1])
	}
	if again.Lines[3].InsertOrigin == nil || again.Lines[3].InsertOrigin.Person != "甲" {
		t.Fatalf("旧段段首来源: %+v", again.Lines[3])
	}
	if len(loaded.insertHistory[claimKey{anchor, model.ActionInsert}]) != 2 {
		t.Fatalf("两段应进 history: %d", len(loaded.insertHistory[claimKey{anchor, model.ActionInsert}]))
	}
	if err := loaded.SubmitWith("丁", anchor, model.ActionInsert, []string{"D1"}, SubmitOpts{
		AfterSeen: idPtr(tail),
	}); err != nil {
		t.Fatal(err)
	}
	after := view(t, loaded)
	if len(after.Lines) != 2 || after.Lines[0].Next != tail {
		t.Fatalf("View→Load 后再 D 应收堆叠: %+v", after.Lines)
	}
	if len(after.Disputes) != 3 {
		t.Fatalf("三份争议: %+v", after.Disputes)
	}
	jia, _ := byPerson(after, "甲")
	bing, _ := byPerson(after, "丙")
	ding, _ := byPerson(after, "丁")
	if len(jia.Content) != 2 || jia.Content[0] != "A1" {
		t.Fatalf("甲: %+v", jia.Content)
	}
	if len(bing.Content) != 4 || bing.Content[0] != "C1" || bing.Content[2] != "A1" {
		t.Fatalf("丙 C+A: %+v", bing.Content)
	}
	if len(ding.Content) != 1 || ding.Content[0] != "D1" {
		t.Fatalf("丁: %+v", ding.Content)
	}
}

func TestRecentEditRoundTrip(t *testing.T) {
	d := New("t")
	line := view(t, d).Lines[0].ID
	if err := d.Submit("甲", line, model.ActionEdit, []string{"正文"}); err != nil {
		t.Fatal(err)
	}
	loaded := roundTripLoad(t, d)
	live := loaded.live[claimKey{line, model.ActionEdit}]
	if live == nil || live.person != "甲" || live.before != "" || live.content[0] != "正文" {
		t.Fatalf("单行编辑应恢复: %+v", live)
	}
	v := view(t, loaded)
	if v.Lines[0].RecentEdit == nil || v.Lines[0].RecentEdit.Before != "" {
		t.Fatalf("View 应带最近编辑空串前文: %+v", v.Lines[0].RecentEdit)
	}
}

func TestLoadLegacyLinesWithoutOrigin(t *testing.T) {
	a := model.Article{ID: model.NewID(), Title: "旧"}
	id1, id2 := model.NewID(), model.NewID()
	lines := []model.Line{
		{ID: id1, Next: id2, Content: "x"},
		{ID: id2, Prev: id1, Content: "y"},
	}
	d, err := Load(a, lines, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(d.insertHistory) != 0 || len(d.live) != 0 {
		t.Fatalf("无字段旧数据不应造 live/history")
	}
}

func TestEditBelief(t *testing.T) {
	d := New("t")
	line := view(t, d).Lines[0].ID
	got, err := d.EditBelief("甲", line)
	if err != nil || len(got) != 1 || got[0] != "" {
		t.Fatalf("空正式行: %v %+v", err, got)
	}
	if err := d.Submit("甲", line, model.ActionEdit, []string{"本地"}); err != nil {
		t.Fatal(err)
	}
	got, err = d.EditBelief("甲", line)
	if err != nil || len(got) != 1 || got[0] != "本地" {
		t.Fatalf("live: %v %+v", err, got)
	}
	got[0] = "篡改"
	got2, _ := d.EditBelief("甲", line)
	if got2[0] != "本地" {
		t.Fatal("应返回副本")
	}
	ownID := model.NewID()
	if err := d.ReceiveForeignEditClaim("甲", model.Dispute{
		ID: model.NewID(), RealLine: line, Action: model.ActionEdit, Person: "乙", Content: []string{"远端"},
	}, ownID); err != nil {
		t.Fatal(err)
	}
	got, err = d.EditBelief("甲", line)
	if err != nil || len(got) != 1 || got[0] != "本地" {
		t.Fatalf("已登记候选优先: %v %+v", err, got)
	}
	if _, err := d.EditBelief("甲", model.NewID()); err != ErrLine {
		t.Fatalf("缺行: %v", err)
	}
}

func TestInsertBelief(t *testing.T) {
	d := New("t")
	anchor := view(t, d).Lines[0].ID
	got, err := d.InsertBelief("甲", anchor, model.ActionInsert)
	if err != nil || len(got) != 0 {
		t.Fatalf("无插入应空: %v %+v", err, got)
	}
	id1, id2 := model.NewID(), model.NewID()
	if err := d.SubmitWith("甲", anchor, model.ActionInsert, []string{"A1", "A2"}, SubmitOpts{
		AfterSeen: &model.ID{},
		LineIDs:   []model.ID{id1, id2},
	}); err != nil {
		t.Fatal(err)
	}
	got, err = d.InsertBelief("甲", anchor, model.ActionInsert)
	if err != nil || len(got) != 2 || got[0] != "A1" || got[1] != "A2" {
		t.Fatalf("live 多行: %v %+v", err, got)
	}
	got[0] = "篡改"
	got2, _ := d.InsertBelief("甲", anchor, model.ActionInsert)
	if got2[0] != "A1" {
		t.Fatal("应返回副本")
	}
	formal := view(t, d).Lines[0].Content
	if formal == "A1" {
		t.Fatal("锚点正式正文不应变成插入段")
	}

	id3 := model.NewID()
	if err := d.SubmitWith("乙", anchor, model.ActionInsert, []string{"B"}, SubmitOpts{
		AfterSeen: &id1, // 同步堆叠：完整段 B+A
		LineIDs:   []model.ID{id3},
	}); err != nil {
		t.Fatal(err)
	}
	wantFull := []string{"B", "A1", "A2"}
	for _, who := range []string{"甲", "乙", "观察者"} {
		got, err = d.InsertBelief(who, anchor, model.ActionInsert)
		if err != nil || !slices.Equal(got, wantFull) {
			t.Fatalf("%s 完整段: %v %+v", who, err, got)
		}
	}

	candID := model.NewID()
	d.disputes[candID] = &model.Dispute{
		ID: candID, RealLine: anchor, Action: model.ActionInsert, Person: "甲", Content: []string{"候选段"},
	}
	got, err = d.InsertBelief("甲", anchor, model.ActionInsert)
	if err != nil || len(got) != 1 || got[0] != "候选段" {
		t.Fatalf("已登记候选优先: %v %+v", err, got)
	}

	d2 := New("t")
	head := view(t, d2).Lines[0].ID
	beforeID := model.NewID()
	if err := d2.SubmitWith("甲", head, model.ActionInsertBefore, []string{"X"}, SubmitOpts{
		BeforeSeen: &model.ID{},
		LineIDs:    []model.ID{beforeID},
	}); err != nil {
		t.Fatal(err)
	}
	got, err = d2.InsertBelief("甲", head, model.ActionInsertBefore)
	if err != nil || len(got) != 1 || got[0] != "X" {
		t.Fatalf("前插: %v %+v", err, got)
	}
	got, err = d2.InsertBelief("甲", head, model.ActionInsert)
	if err != nil || len(got) != 0 {
		t.Fatalf("反方向应空: %v %+v", err, got)
	}
	if _, err := d.InsertBelief("甲", model.NewID(), model.ActionInsert); err != ErrLine {
		t.Fatalf("缺行: %v", err)
	}
	if _, err := d.InsertBelief("甲", anchor, model.ActionEdit); err != ErrAction {
		t.Fatalf("非插入: %v", err)
	}
}

func TestInsertBeliefAfterStackedCAAndObserver(t *testing.T) {
	d := New("t")
	if err := d.EnsureLines(2); err != nil {
		t.Fatal(err)
	}
	v := view(t, d)
	anchor, tail := v.Lines[0].ID, v.Lines[1].ID
	aID := model.NewID()
	if err := d.SubmitWith("甲", anchor, model.ActionInsert, []string{"A"}, SubmitOpts{
		AfterSeen: &tail,
		LineIDs:   []model.ID{aID},
	}); err != nil {
		t.Fatal(err)
	}
	got, err := d.InsertBelief("观察者", anchor, model.ActionInsert)
	if err != nil || !slices.Equal(got, []string{"A"}) {
		t.Fatalf("仅已同步 A 的观察者: %v %+v", err, got)
	}
	cID := model.NewID()
	if err := d.SubmitWith("丙", anchor, model.ActionInsert, []string{"C"}, SubmitOpts{
		AfterSeen: &aID,
		LineIDs:   []model.ID{cID},
	}); err != nil {
		t.Fatal(err)
	}
	wantCA := []string{"C", "A"}
	for _, who := range []string{"丙", "甲", "观察者"} {
		got, err = d.InsertBelief(who, anchor, model.ActionInsert)
		if err != nil || !slices.Equal(got, wantCA) {
			t.Fatalf("%s 应为 C+A: %v %+v", who, err, got)
		}
	}

	dDing := New("t")
	if err := dDing.EnsureLines(2); err != nil {
		t.Fatal(err)
	}
	vd := view(t, dDing)
	a2, t2 := vd.Lines[0].ID, vd.Lines[1].ID
	if err := dDing.SubmitWith("丁", a2, model.ActionInsert, []string{"D"}, SubmitOpts{
		AfterSeen: &t2,
		LineIDs:   []model.ID{model.NewID()},
	}); err != nil {
		t.Fatal(err)
	}
	got, err = dDing.InsertBelief("丁", a2, model.ActionInsert)
	if err != nil || !slices.Equal(got, []string{"D"}) {
		t.Fatalf("丁未同步只见 D: %v %+v", err, got)
	}
}

func TestInsertBeliefBeforeStackedSymmetric(t *testing.T) {
	d := New("t")
	head := view(t, d).Lines[0].ID
	aID := model.NewID()
	if err := d.SubmitWith("甲", head, model.ActionInsertBefore, []string{"A"}, SubmitOpts{
		BeforeSeen: &model.ID{},
		LineIDs:    []model.ID{aID},
	}); err != nil {
		t.Fatal(err)
	}
	got, err := d.InsertBelief("观察者", head, model.ActionInsertBefore)
	if err != nil || !slices.Equal(got, []string{"A"}) {
		t.Fatalf("前插观察者 A: %v %+v", err, got)
	}
	cID := model.NewID()
	if err := d.SubmitWith("丙", head, model.ActionInsertBefore, []string{"C"}, SubmitOpts{
		BeforeSeen: &aID, // 同步堆叠：新段更靠近锚 → 阅读序 A,C
		LineIDs:    []model.ID{cID},
	}); err != nil {
		t.Fatal(err)
	}
	wantAC := []string{"A", "C"}
	for _, who := range []string{"丙", "甲", "观察者"} {
		got, err = d.InsertBelief(who, head, model.ActionInsertBefore)
		if err != nil || !slices.Equal(got, wantAC) {
			t.Fatalf("%s 前插完整段 A+C: %v %+v", who, err, got)
		}
	}
	got, err = d.InsertBelief("丙", head, model.ActionInsert)
	if err != nil || len(got) != 0 {
		t.Fatalf("反方向应空: %v %+v", err, got)
	}
}

func TestReceiveForeignEditClaim(t *testing.T) {
	d := New("t")
	line := view(t, d).Lines[0].ID
	if err := d.Submit("甲", line, model.ActionEdit, []string{"本地"}); err != nil {
		t.Fatal(err)
	}
	before := view(t, d)
	if before.Lines[0].Content != "本地" || len(before.Disputes) != 0 {
		t.Fatalf("接收前应只有正文: %+v", before)
	}
	liveBefore := d.live[claimKey{line, model.ActionEdit}]
	histBefore := len(d.insertHistory)

	foreignID, ownID := model.NewID(), model.NewID()
	foreign := model.Dispute{
		ID:       foreignID,
		RealLine: line,
		Action:   model.ActionEdit,
		Person:   "乙",
		Content:  []string{"远端"},
	}
	if err := d.ReceiveForeignEditClaim("甲", foreign, ownID); err != nil {
		t.Fatal(err)
	}
	after := view(t, d)
	if after.Lines[0].Content != "本地" || after.Lines[0].Prev != before.Lines[0].Prev || after.Lines[0].Next != before.Lines[0].Next {
		t.Fatalf("正式正文与链应不变: %+v", after.Lines[0])
	}
	if len(after.Disputes) != 2 {
		t.Fatalf("本人+外来各一份: %+v", after.Disputes)
	}
	jia, ok1 := byPerson(after, "甲")
	yi, ok2 := byPerson(after, "乙")
	if !ok1 || !ok2 || jia.ID != ownID || yi.ID != foreignID {
		t.Fatalf("稳定 ID: 甲=%+v 乙=%+v", jia, yi)
	}
	if len(jia.Content) != 1 || jia.Content[0] != "本地" || len(yi.Content) != 1 || yi.Content[0] != "远端" {
		t.Fatalf("候选内容: 甲=%+v 乙=%+v", jia.Content, yi.Content)
	}
	if d.live[claimKey{line, model.ActionEdit}] != liveBefore || len(d.insertHistory) != histBefore {
		t.Fatal("不得动 live / insertHistory")
	}

	foreign.Content = []string{"远端改"}
	if err := d.ReceiveForeignEditClaim("甲", foreign, model.NewID()); err != nil {
		t.Fatal(err)
	}
	again := view(t, d)
	if len(again.Disputes) != 2 {
		t.Fatalf("重复 ID 仍各一份: %+v", again.Disputes)
	}
	yi, _ = byPerson(again, "乙")
	jia, _ = byPerson(again, "甲")
	if yi.Content[0] != "远端改" || jia.ID != ownID || jia.Content[0] != "本地" {
		t.Fatalf("更新外来、本人 ID/内容保持: 甲=%+v 乙=%+v", jia, yi)
	}

	snap := len(d.disputes)
	if err := d.ReceiveForeignEditClaim("甲", model.Dispute{
		ID: model.NewID(), RealLine: line, Action: model.ActionEdit, Person: "甲", Content: []string{"自CC"},
	}, model.NewID()); err != nil {
		t.Fatal(err)
	}
	if len(d.disputes) != snap {
		t.Fatal("本人 CC 应忽略")
	}
}

func TestReceiveForeignEditClaimKeepsLiveMultiline(t *testing.T) {
	d := New("t")
	line := view(t, d).Lines[0].ID
	tail := model.NewID()
	if err := d.SubmitWith("甲", line, model.ActionEdit, []string{"hello", "world"}, SubmitOpts{LineIDs: []model.ID{tail}}); err != nil {
		t.Fatal(err)
	}
	before := view(t, d)
	foreignID, ownID := model.NewID(), model.NewID()
	if err := d.ReceiveForeignEditClaim("甲", model.Dispute{
		ID: foreignID, RealLine: line, Action: model.ActionEdit, Person: "乙", Content: []string{"other"},
	}, ownID); err != nil {
		t.Fatal(err)
	}
	after := view(t, d)
	if len(after.Lines) != len(before.Lines) {
		t.Fatalf("行数不变: before=%d after=%d", len(before.Lines), len(after.Lines))
	}
	for i := range before.Lines {
		if after.Lines[i].Content != before.Lines[i].Content || after.Lines[i].Prev != before.Lines[i].Prev || after.Lines[i].Next != before.Lines[i].Next {
			t.Fatalf("链不变 [%d]: %+v vs %+v", i, before.Lines[i], after.Lines[i])
		}
	}
	jia, _ := byPerson(after, "甲")
	if len(jia.Content) != 2 || jia.Content[0] != "hello" || jia.Content[1] != "world" {
		t.Fatalf("本人候选应保留 live 整段: %+v", jia.Content)
	}
}

func TestReceiveForeignEditClaimRejectsBadInput(t *testing.T) {
	d := New("t")
	line := view(t, d).Lines[0].ID
	if err := d.Submit("甲", line, model.ActionEdit, []string{"x"}); err != nil {
		t.Fatal(err)
	}
	ownID := model.NewID()
	bad := []model.Dispute{
		{ID: model.NewID(), RealLine: line, Action: model.ActionInsert, Person: "乙", Content: []string{"y"}},
		{ID: model.ID{}, RealLine: line, Action: model.ActionEdit, Person: "乙", Content: []string{"y"}},
		{ID: model.NewID(), RealLine: model.ID{}, Action: model.ActionEdit, Person: "乙", Content: []string{"y"}},
		{ID: model.NewID(), RealLine: model.NewID(), Action: model.ActionEdit, Person: "乙", Content: []string{"y"}},
	}
	for i, foreign := range bad {
		if err := d.ReceiveForeignEditClaim("甲", foreign, ownID); err == nil {
			t.Fatalf("坏输入[%d]应失败", i)
		}
		if len(d.disputes) != 0 {
			t.Fatalf("坏输入[%d]不得留半成品: %+v", i, d.disputes)
		}
	}
	if err := d.ReceiveForeignEditClaim("甲", model.Dispute{
		ID: model.NewID(), RealLine: line, Action: model.ActionEdit, Person: "乙", Content: []string{"y"},
	}, model.ID{}); err == nil || len(d.disputes) != 0 {
		t.Fatalf("ownID 为零不得半成品: err=%v disputes=%d", err, len(d.disputes))
	}
	foreignID := model.NewID()
	if err := d.ReceiveForeignEditClaim("甲", model.Dispute{
		ID: foreignID, RealLine: line, Action: model.ActionEdit, Person: "乙", Content: []string{"y"},
	}, ownID); err != nil {
		t.Fatal(err)
	}
	if err := d.ReceiveForeignEditClaim("甲", model.Dispute{
		ID: foreignID, RealLine: line, Action: model.ActionEdit, Person: "丙", Content: []string{"z"},
	}, model.NewID()); err == nil {
		t.Fatal("同 ID 换人应拒绝")
	}
	if len(view(t, d).Disputes) != 2 {
		t.Fatalf("换人拒绝后份数不变: %+v", view(t, d).Disputes)
	}
	otherLine := model.NewID()
	d.lines[otherLine] = &model.Line{ID: otherLine}
	if err := d.ReceiveForeignEditClaim("甲", model.Dispute{
		ID: foreignID, RealLine: otherLine, Action: model.ActionEdit, Person: "乙", Content: []string{"z"},
	}, model.NewID()); err == nil {
		t.Fatal("同 ID 换行应拒绝")
	}
}

func TestReceiveForeignDeleteClaim(t *testing.T) {
	d := New("t")
	line := view(t, d).Lines[0].ID
	if err := d.Submit("甲", line, model.ActionEdit, []string{"保留"}); err != nil {
		t.Fatal(err)
	}
	before := view(t, d)
	liveBefore := d.live[claimKey{line, model.ActionEdit}]
	histBefore := len(d.insertHistory)

	foreignID, ownID := model.NewID(), model.NewID()
	foreign := model.Dispute{
		ID: foreignID, RealLine: line, Action: model.ActionDelete, Person: "乙", Content: nil,
	}
	if err := d.ReceiveForeignDeleteClaim("甲", foreign, ownID); err != nil {
		t.Fatal(err)
	}
	after := view(t, d)
	if after.Lines[0].Content != "保留" || after.Lines[0].Prev != before.Lines[0].Prev || after.Lines[0].Next != before.Lines[0].Next {
		t.Fatalf("正式链原样: %+v", after.Lines[0])
	}
	if len(after.Disputes) != 2 {
		t.Fatalf("本人 Edit + 外来 Delete: %+v", after.Disputes)
	}
	jia, ok1 := byPerson(after, "甲")
	yi, ok2 := byPerson(after, "乙")
	if !ok1 || !ok2 || jia.ID != ownID || yi.ID != foreignID {
		t.Fatalf("稳定 ID: 甲=%+v 乙=%+v", jia, yi)
	}
	if jia.Action != model.ActionEdit || len(jia.Content) != 1 || jia.Content[0] != "保留" {
		t.Fatalf("本人保留行: %+v", jia)
	}
	if yi.Action != model.ActionDelete || len(yi.Content) != 0 {
		t.Fatalf("外来删这行: %+v", yi)
	}
	if d.live[claimKey{line, model.ActionEdit}] != liveBefore || len(d.insertHistory) != histBefore {
		t.Fatal("不得动 live / insertHistory")
	}

	// 幂等：同 foreign ID 再来仍各一份
	if err := d.ReceiveForeignDeleteClaim("甲", foreign, model.NewID()); err != nil {
		t.Fatal(err)
	}
	again := view(t, d)
	if len(again.Disputes) != 2 {
		t.Fatalf("重复仍各一份: %+v", again.Disputes)
	}
	jia, _ = byPerson(again, "甲")
	yi, _ = byPerson(again, "乙")
	if jia.ID != ownID || yi.ID != foreignID || yi.Action != model.ActionDelete {
		t.Fatalf("幂等保 ID/动作: 甲=%+v 乙=%+v", jia, yi)
	}

	snap := len(d.disputes)
	if err := d.ReceiveForeignDeleteClaim("甲", model.Dispute{
		ID: model.NewID(), RealLine: line, Action: model.ActionDelete, Person: "甲", Content: nil,
	}, model.NewID()); err != nil {
		t.Fatal(err)
	}
	if len(d.disputes) != snap {
		t.Fatal("本人 CC 应忽略")
	}
}

func TestReceiveForeignDeleteClaimKeepsOwnEditID(t *testing.T) {
	d := New("t")
	line := view(t, d).Lines[0].ID
	if err := d.Submit("甲", line, model.ActionEdit, []string{"本地"}); err != nil {
		t.Fatal(err)
	}
	editOwnID, editForeignID := model.NewID(), model.NewID()
	if err := d.ReceiveForeignEditClaim("甲", model.Dispute{
		ID: editForeignID, RealLine: line, Action: model.ActionEdit, Person: "乙", Content: []string{"远端"},
	}, editOwnID); err != nil {
		t.Fatal(err)
	}
	delForeignID := model.NewID()
	if err := d.ReceiveForeignDeleteClaim("甲", model.Dispute{
		ID: delForeignID, RealLine: line, Action: model.ActionDelete, Person: "丙", Content: nil,
	}, model.NewID()); err != nil {
		t.Fatal(err)
	}
	v := view(t, d)
	if len(v.Disputes) != 3 {
		t.Fatalf("甲Edit+乙Edit+丙Delete: %+v", v.Disputes)
	}
	jia, _ := byPerson(v, "甲")
	if jia.ID != editOwnID || jia.Action != model.ActionEdit || jia.Content[0] != "本地" {
		t.Fatalf("本人 Edit 不漂: %+v", jia)
	}
}

func TestReceiveForeignDeleteClaimKeepsOwnDeleteID(t *testing.T) {
	d := New("t")
	if err := d.EnsureLines(2); err != nil {
		t.Fatal(err)
	}
	line := view(t, d).Lines[1].ID
	ownDelID := model.NewID()
	if err := d.StoreExplicitClaim(model.Dispute{
		ID: ownDelID, RealLine: line, Action: model.ActionDelete, Person: "甲", Content: nil,
	}); err != nil {
		t.Fatal(err)
	}
	foreignID := model.NewID()
	if err := d.ReceiveForeignDeleteClaim("甲", model.Dispute{
		ID: foreignID, RealLine: line, Action: model.ActionDelete, Person: "乙", Content: nil,
	}, model.NewID()); err != nil {
		t.Fatal(err)
	}
	v := view(t, d)
	jia, _ := byPerson(v, "甲")
	yi, _ := byPerson(v, "乙")
	if jia.ID != ownDelID || jia.Action != model.ActionDelete {
		t.Fatalf("本人 Delete 不漂: %+v", jia)
	}
	if yi.ID != foreignID || yi.Action != model.ActionDelete {
		t.Fatalf("外来 Delete: %+v", yi)
	}
	if len(v.Disputes) != 2 {
		t.Fatalf("恰两份: %+v", v.Disputes)
	}
}

func TestReceiveForeignBodyClaimActionSwitchSameID(t *testing.T) {
	d := New("t")
	line := view(t, d).Lines[0].ID
	if err := d.Submit("甲", line, model.ActionEdit, []string{"行"}); err != nil {
		t.Fatal(err)
	}
	foreignID, ownID := model.NewID(), model.NewID()
	if err := d.ReceiveForeignEditClaim("甲", model.Dispute{
		ID: foreignID, RealLine: line, Action: model.ActionEdit, Person: "乙",
		Content: []string{"改"}, Followers: []string{"丙"},
	}, ownID); err != nil {
		t.Fatal(err)
	}
	if err := d.ReceiveForeignDeleteClaim("甲", model.Dispute{
		ID: foreignID, RealLine: line, Action: model.ActionDelete, Person: "乙",
		Content: nil, Followers: nil,
	}, model.NewID()); err != nil {
		t.Fatal(err)
	}
	v := view(t, d)
	if len(v.Disputes) != 2 {
		t.Fatalf("切换后仍两份: %+v", v.Disputes)
	}
	jia, _ := byPerson(v, "甲")
	yi, _ := byPerson(v, "乙")
	if jia.ID != ownID || jia.Action != model.ActionEdit {
		t.Fatalf("本人不变: %+v", jia)
	}
	if yi.ID != foreignID || yi.Action != model.ActionDelete || len(yi.Followers) != 1 || yi.Followers[0] != "丙" {
		t.Fatalf("同 ID Edit→Delete 保 Followers: %+v", yi)
	}

	if err := d.ReceiveForeignEditClaim("甲", model.Dispute{
		ID: foreignID, RealLine: line, Action: model.ActionEdit, Person: "乙",
		Content: []string{"又改"}, Followers: []string{},
	}, model.NewID()); err != nil {
		t.Fatal(err)
	}
	yi, _ = byPerson(view(t, d), "乙")
	if yi.ID != foreignID || yi.Action != model.ActionEdit || yi.Content[0] != "又改" || yi.Followers[0] != "丙" {
		t.Fatalf("同 ID Delete→Edit: %+v", yi)
	}
}

func TestReceiveForeignEditClaimKeepsFollowMetaOnContentCC(t *testing.T) {
	d := New("t")
	line := view(t, d).Lines[0].ID
	if err := d.Submit("甲", line, model.ActionEdit, []string{"本地"}); err != nil {
		t.Fatal(err)
	}
	foreignID, ownID := model.NewID(), model.NewID()
	if err := d.ReceiveForeignEditClaim("甲", model.Dispute{
		ID: foreignID, RealLine: line, Action: model.ActionEdit, Person: "乙",
		Content: []string{"远端"}, Followers: []string{"旁观"},
		Pending: []model.PendingConfirm{{From: "丙", To: "乙", ClientTs: 9}},
	}, ownID); err != nil {
		t.Fatal(err)
	}
	out, err := d.RequestFollow("甲", foreignID, 1)
	if err != nil || out.Status != FollowPending {
		t.Fatalf("甲追随乙: %+v err=%v", out, err)
	}
	yi, _ := byPerson(view(t, d), "乙")
	if len(yi.Followers) != 1 || yi.Followers[0] != "旁观" || len(yi.Pending) != 2 {
		t.Fatalf("初登记 Followers+Pending: %+v", yi)
	}

	if err := d.ReceiveForeignEditClaim("甲", model.Dispute{
		ID: foreignID, RealLine: line, Action: model.ActionEdit, Person: "乙",
		Content: []string{"远端改"}, Followers: nil, Pending: nil,
	}, model.NewID()); err != nil {
		t.Fatal(err)
	}
	yi, _ = byPerson(view(t, d), "乙")
	if yi.Content[0] != "远端改" {
		t.Fatalf("应更新正文: %+v", yi)
	}
	if len(yi.Followers) != 1 || yi.Followers[0] != "旁观" {
		t.Fatalf("空 metadata 不得擦 Followers: %+v", yi)
	}
	if len(yi.Pending) != 2 {
		t.Fatalf("空 metadata 不得擦 Pending: %+v", yi.Pending)
	}

	if err := d.ReceiveForeignEditClaim("甲", model.Dispute{
		ID: foreignID, RealLine: line, Action: model.ActionEdit, Person: "乙",
		Content: []string{"再改"}, Followers: []string{}, Pending: []model.PendingConfirm{},
	}, model.NewID()); err != nil {
		t.Fatal(err)
	}
	yi, _ = byPerson(view(t, d), "乙")
	if len(yi.Followers) != 1 || yi.Followers[0] != "旁观" || len(yi.Pending) != 2 {
		t.Fatalf("显式空 metadata 仍保追随: %+v", yi)
	}
}

func TestReceiveForeignDeleteClaimRequestFollowPending(t *testing.T) {
	d := New("t")
	line := view(t, d).Lines[0].ID
	if err := d.Submit("甲", line, model.ActionEdit, []string{"x"}); err != nil {
		t.Fatal(err)
	}
	foreignID, ownID := model.NewID(), model.NewID()
	if err := d.ReceiveForeignDeleteClaim("甲", model.Dispute{
		ID: foreignID, RealLine: line, Action: model.ActionDelete, Person: "乙", Content: nil,
	}, ownID); err != nil {
		t.Fatal(err)
	}
	out, err := d.RequestFollow("甲", foreignID, 1)
	if err != nil || out.Status != FollowPending || out.DisputeID != foreignID {
		t.Fatalf("追随删除候选进 Pending: out=%+v err=%v", out, err)
	}
}

func TestReceiveForeignDeleteClaimRejectsBadInput(t *testing.T) {
	d := New("t")
	line := view(t, d).Lines[0].ID
	if err := d.Submit("甲", line, model.ActionEdit, []string{"x"}); err != nil {
		t.Fatal(err)
	}
	ownID := model.NewID()
	bad := []model.Dispute{
		{ID: model.NewID(), RealLine: line, Action: model.ActionEdit, Person: "乙", Content: []string{"y"}},
		{ID: model.ID{}, RealLine: line, Action: model.ActionDelete, Person: "乙", Content: nil},
		{ID: model.NewID(), RealLine: model.ID{}, Action: model.ActionDelete, Person: "乙", Content: nil},
		{ID: model.NewID(), RealLine: model.NewID(), Action: model.ActionDelete, Person: "乙", Content: nil},
		{ID: model.NewID(), RealLine: line, Action: model.ActionDelete, Person: "乙", Content: []string{"非空"}},
	}
	for i, foreign := range bad {
		if err := d.ReceiveForeignDeleteClaim("甲", foreign, ownID); err == nil {
			t.Fatalf("坏输入[%d]应失败", i)
		}
		if len(d.disputes) != 0 {
			t.Fatalf("坏输入[%d]零突变: %+v", i, d.disputes)
		}
	}
	if err := d.ReceiveForeignDeleteClaim("甲", model.Dispute{
		ID: model.NewID(), RealLine: line, Action: model.ActionDelete, Person: "乙", Content: nil,
	}, model.ID{}); err == nil || len(d.disputes) != 0 {
		t.Fatalf("ownID 为零不得半成品: err=%v disputes=%d", err, len(d.disputes))
	}

	foreignID := model.NewID()
	if err := d.ReceiveForeignDeleteClaim("甲", model.Dispute{
		ID: foreignID, RealLine: line, Action: model.ActionDelete, Person: "乙", Content: nil,
	}, ownID); err != nil {
		t.Fatal(err)
	}
	// 同人同槽不同 ID
	if err := d.ReceiveForeignDeleteClaim("甲", model.Dispute{
		ID: model.NewID(), RealLine: line, Action: model.ActionDelete, Person: "乙", Content: nil,
	}, model.NewID()); err == nil {
		t.Fatal("同人同槽双候选应拒")
	}
	if len(view(t, d).Disputes) != 2 {
		t.Fatalf("拒绝后份数不变: %+v", view(t, d).Disputes)
	}
	if err := d.ReceiveForeignDeleteClaim("甲", model.Dispute{
		ID: foreignID, RealLine: line, Action: model.ActionDelete, Person: "丙", Content: nil,
	}, model.NewID()); err == nil {
		t.Fatal("同 ID 换人应拒绝")
	}
}

func TestReceiveForeignInsertClaimStackedAfter(t *testing.T) {
	d := New("t")
	if err := d.EnsureLines(2); err != nil {
		t.Fatal(err)
	}
	before := view(t, d)
	anchor, tail := before.Lines[0].ID, before.Lines[1].ID
	aID := model.NewID()
	if err := d.SubmitWith("甲", anchor, model.ActionInsert, []string{"A"}, SubmitOpts{
		AfterSeen: idPtr(tail), LineIDs: []model.ID{aID},
	}); err != nil {
		t.Fatal(err)
	}
	cID := model.NewID()
	if err := d.SubmitWith("丙", anchor, model.ActionInsert, []string{"C"}, SubmitOpts{
		AfterSeen: idPtr(aID), LineIDs: []model.ID{cID},
	}); err != nil {
		t.Fatal(err)
	}
	foreignID, ownID := model.NewID(), model.NewID()
	foreign := model.Dispute{
		ID: foreignID, RealLine: anchor, Action: model.ActionInsert, Person: "丁",
		Content: []string{"D"}, Followers: []string{"旁观"}, BaseIDs: []model.ID{model.NewID()},
	}
	if err := d.ReceiveForeignInsertClaim("丙", foreign, ownID); err != nil {
		t.Fatal(err)
	}
	after := view(t, d)
	if len(after.Lines) != 2 || after.Lines[0].ID != anchor || after.Lines[0].Next != tail || after.Lines[1].ID != tail {
		t.Fatalf("链只留锚+原后继: %+v", after.Lines)
	}
	if len(after.Disputes) != 3 {
		t.Fatalf("甲/丙/丁 三份: %+v", after.Disputes)
	}
	jia, _ := byPerson(after, "甲")
	bing, _ := byPerson(after, "丙")
	ding, _ := byPerson(after, "丁")
	if !slices.Equal(jia.Content, []string{"A"}) {
		t.Fatalf("甲候选 A: %+v", jia.Content)
	}
	if !slices.Equal(bing.Content, []string{"C", "A"}) || bing.ID != ownID {
		t.Fatalf("丙=C+A ownID: %+v", bing)
	}
	if !slices.Equal(ding.Content, []string{"D"}) || ding.ID != foreignID {
		t.Fatalf("丁=D foreign.ID: %+v", ding)
	}
	if len(ding.Followers) != 1 || ding.Followers[0] != "旁观" || len(ding.BaseIDs) != 1 {
		t.Fatalf("外来 Followers/BaseIDs 应复制: %+v", ding)
	}
	if d.live[claimKey{anchor, model.ActionInsert}] != nil || len(d.insertHistory[claimKey{anchor, model.ActionInsert}]) != 0 {
		t.Fatal("提升后本锚后插 live/history 应空")
	}
}

func TestReceiveForeignInsertClaimStackedBefore(t *testing.T) {
	d := New("t")
	head := view(t, d).Lines[0].ID
	aID := model.NewID()
	if err := d.SubmitWith("甲", head, model.ActionInsertBefore, []string{"A"}, SubmitOpts{
		BeforeSeen: &model.ID{}, LineIDs: []model.ID{aID},
	}); err != nil {
		t.Fatal(err)
	}
	cID := model.NewID()
	if err := d.SubmitWith("丙", head, model.ActionInsertBefore, []string{"C"}, SubmitOpts{
		BeforeSeen: idPtr(aID), LineIDs: []model.ID{cID},
	}); err != nil {
		t.Fatal(err)
	}
	foreignID, ownID := model.NewID(), model.NewID()
	if err := d.ReceiveForeignInsertClaim("丙", model.Dispute{
		ID: foreignID, RealLine: head, Action: model.ActionInsertBefore, Person: "丁", Content: []string{"D"},
	}, ownID); err != nil {
		t.Fatal(err)
	}
	after := view(t, d)
	if len(after.Lines) != 1 || after.Lines[0].ID != head || !after.Lines[0].Prev.IsZero() {
		t.Fatalf("前插提升后只留锚: %+v", after.Lines)
	}
	jia, _ := byPerson(after, "甲")
	bing, _ := byPerson(after, "丙")
	ding, _ := byPerson(after, "丁")
	if !slices.Equal(jia.Content, []string{"A"}) || !slices.Equal(bing.Content, []string{"A", "C"}) || bing.ID != ownID {
		t.Fatalf("前插层叠 甲=A 丙=A+C: 甲=%+v 丙=%+v", jia, bing)
	}
	if ding.ID != foreignID || !slices.Equal(ding.Content, []string{"D"}) {
		t.Fatalf("丁: %+v", ding)
	}
}

func TestReceiveForeignInsertClaimObserverOwnIsIntegrated(t *testing.T) {
	d := New("t")
	if err := d.EnsureLines(2); err != nil {
		t.Fatal(err)
	}
	v := view(t, d)
	anchor, tail := v.Lines[0].ID, v.Lines[1].ID
	aID := model.NewID()
	if err := d.SubmitWith("甲", anchor, model.ActionInsert, []string{"A"}, SubmitOpts{
		AfterSeen: idPtr(tail), LineIDs: []model.ID{aID},
	}); err != nil {
		t.Fatal(err)
	}
	foreignID, ownID := model.NewID(), model.NewID()
	if err := d.ReceiveForeignInsertClaim("观察者", model.Dispute{
		ID: foreignID, RealLine: anchor, Action: model.ActionInsert, Person: "丁", Content: []string{"D"},
	}, ownID); err != nil {
		t.Fatal(err)
	}
	after := view(t, d)
	obs, _ := byPerson(after, "观察者")
	jia, _ := byPerson(after, "甲")
	ding, _ := byPerson(after, "丁")
	if obs.ID != ownID || !slices.Equal(obs.Content, []string{"A"}) {
		t.Fatalf("本人没插但已整合 A: %+v", obs)
	}
	if !slices.Equal(jia.Content, []string{"A"}) || ding.ID != foreignID {
		t.Fatalf("甲/丁: %+v %+v", jia, ding)
	}
}

func TestReceiveForeignInsertClaimReplayAndRebind(t *testing.T) {
	d := New("t")
	if err := d.EnsureLines(2); err != nil {
		t.Fatal(err)
	}
	v := view(t, d)
	anchor, tail := v.Lines[0].ID, v.Lines[1].ID
	aID := model.NewID()
	if err := d.SubmitWith("甲", anchor, model.ActionInsert, []string{"A"}, SubmitOpts{
		AfterSeen: idPtr(tail), LineIDs: []model.ID{aID},
	}); err != nil {
		t.Fatal(err)
	}
	foreignID, ownID := model.NewID(), model.NewID()
	if err := d.ReceiveForeignInsertClaim("丙", model.Dispute{
		ID: foreignID, RealLine: anchor, Action: model.ActionInsert, Person: "丁", Content: []string{"D"},
	}, ownID); err != nil {
		t.Fatal(err)
	}
	jia, _ := byPerson(view(t, d), "甲")
	randomAID := jia.ID

	foreign := model.Dispute{
		ID: foreignID, RealLine: anchor, Action: model.ActionInsert, Person: "丁", Content: []string{"D2"},
	}
	if err := d.ReceiveForeignInsertClaim("丙", foreign, model.NewID()); err != nil {
		t.Fatal(err)
	}
	again := view(t, d)
	if len(again.Disputes) != 3 {
		t.Fatalf("重放不增份: %+v", again.Disputes)
	}
	ding, _ := byPerson(again, "丁")
	if ding.Content[0] != "D2" || ding.ID != foreignID {
		t.Fatalf("按 ID 更新: %+v", ding)
	}

	stableAID := model.NewID()
	if err := d.ReceiveForeignInsertClaim("丙", model.Dispute{
		ID: stableAID, RealLine: anchor, Action: model.ActionInsert, Person: "甲", Content: []string{"A-stable"},
	}, ownID); err != nil {
		t.Fatal(err)
	}
	final := view(t, d)
	if len(final.Disputes) != 3 {
		t.Fatalf("甲重绑不增份: %+v", final.Disputes)
	}
	jia, _ = byPerson(final, "甲")
	if jia.ID != stableAID || jia.ID == randomAID || !slices.Equal(jia.Content, []string{"A-stable"}) {
		t.Fatalf("甲应重绑稳定 ID: old=%s got=%+v", randomAID.Hex(), jia)
	}
}

func TestReceiveForeignInsertClaimPreservesOtherAnchor(t *testing.T) {
	d := New("t")
	if err := d.EnsureLines(3); err != nil {
		t.Fatal(err)
	}
	v := view(t, d)
	a0, a1, a2 := v.Lines[0].ID, v.Lines[1].ID, v.Lines[2].ID
	insID := model.NewID()
	if err := d.SubmitWith("甲", a0, model.ActionInsert, []string{"A"}, SubmitOpts{
		AfterSeen: idPtr(a1), LineIDs: []model.ID{insID},
	}); err != nil {
		t.Fatal(err)
	}
	if err := d.Submit("乙", a2, model.ActionEdit, []string{"改尾"}); err != nil {
		t.Fatal(err)
	}
	if d.live[claimKey{a2, model.ActionEdit}] == nil || d.live[claimKey{a2, model.ActionEdit}].spliced {
		t.Fatal("前置：a2 应有未拼接 edit live")
	}
	beforeID := model.NewID()
	if err := d.SubmitWith("戊", a1, model.ActionInsertBefore, []string{"前"}, SubmitOpts{
		BeforeSeen: idPtr(insID), LineIDs: []model.ID{beforeID},
	}); err != nil {
		t.Fatal(err)
	}
	if d.live[claimKey{a1, model.ActionInsertBefore}] == nil {
		t.Fatal("前置：a1 前插 live")
	}
	extraID := model.NewID()
	d.disputes[extraID] = &model.Dispute{
		ID: extraID, RealLine: a2, Action: model.ActionEdit, Person: "己",
		Content: []string{"挂"}, Followers: []string{},
	}
	d.suspended[extraID] = true
	d.pending = append(d.pending, followPend{from: "庚", to: "己", dispute: extraID, ts: 1})
	pendN, susN := len(d.pending), len(d.suspended)

	foreignID, ownID := model.NewID(), model.NewID()
	if err := d.ReceiveForeignInsertClaim("甲", model.Dispute{
		ID: foreignID, RealLine: a0, Action: model.ActionInsert, Person: "丁", Content: []string{"D"},
	}, ownID); err != nil {
		t.Fatal(err)
	}
	if got := d.live[claimKey{a2, model.ActionEdit}]; got == nil || got.person != "乙" || got.content[0] != "改尾" || got.spliced {
		t.Fatalf("另一锚点未拼接 edit live 应保留: %+v", got)
	}
	if got := d.live[claimKey{a1, model.ActionInsertBefore}]; got == nil || got.person != "戊" {
		t.Fatalf("反方向前插 live 应保留: %+v", got)
	}
	if len(d.pending) != pendN || len(d.suspended) != susN || !d.suspended[extraID] {
		t.Fatalf("pending/suspended 应保留 pend=%d sus=%d", len(d.pending), len(d.suspended))
	}
	if d.disputes[extraID] == nil || d.disputes[extraID].Content[0] != "挂" {
		t.Fatal("无关挂起争议应保留")
	}
}

func TestReceiveForeignInsertClaimRejectZeroMutation(t *testing.T) {
	d := New("t")
	if err := d.EnsureLines(2); err != nil {
		t.Fatal(err)
	}
	before := view(t, d)
	anchor, tail := before.Lines[0].ID, before.Lines[1].ID
	a1, a2 := model.NewID(), model.NewID()
	if err := d.SubmitWith("甲", anchor, model.ActionInsert, []string{"A1", "A2"}, SubmitOpts{
		AfterSeen: idPtr(tail), LineIDs: []model.ID{a1, a2},
	}); err != nil {
		t.Fatal(err)
	}
	if err := d.Submit("乙", a2, model.ActionEdit, []string{"x", "y"}); err != nil {
		t.Fatal(err)
	}
	snapView := view(t, d)
	snapTexts := textsOf(snapView.Lines)
	liveKeys := len(d.live)
	histKeys := len(d.insertHistory)
	dispN := len(d.disputes)
	livePtr := d.live[claimKey{anchor, model.ActionInsert}]

	err := d.ReceiveForeignInsertClaim("丙", model.Dispute{
		ID: model.NewID(), RealLine: anchor, Action: model.ActionInsert, Person: "丁", Content: []string{"D"},
	}, model.NewID())
	if err != ErrPromoteUnsafe {
		t.Fatalf("子多行粘贴应 ErrPromoteUnsafe: %v", err)
	}
	after := view(t, d)
	if len(after.Lines) != len(snapView.Lines) || len(after.Disputes) != len(snapView.Disputes) {
		t.Fatalf("View 零突变 lines/disputes")
	}
	if !slices.Equal(textsOf(after.Lines), snapTexts) {
		t.Fatalf("正文零突变: before=%v after=%v", snapTexts, textsOf(after.Lines))
	}
	if len(d.live) != liveKeys || len(d.insertHistory) != histKeys || len(d.disputes) != dispN {
		t.Fatalf("内部 map 零突变 live=%d hist=%d disp=%d", len(d.live), len(d.insertHistory), len(d.disputes))
	}
	if d.live[claimKey{anchor, model.ActionInsert}] != livePtr {
		t.Fatal("本锚 live 指针应未替换")
	}
}

func TestReceiveForeignInsertClaimRequestFollowStableID(t *testing.T) {
	d := New("t")
	if err := d.EnsureLines(2); err != nil {
		t.Fatal(err)
	}
	v := view(t, d)
	anchor, tail := v.Lines[0].ID, v.Lines[1].ID
	aID := model.NewID()
	if err := d.SubmitWith("甲", anchor, model.ActionInsert, []string{"A"}, SubmitOpts{
		AfterSeen: idPtr(tail), LineIDs: []model.ID{aID},
	}); err != nil {
		t.Fatal(err)
	}
	foreignID, ownID := model.NewID(), model.NewID()
	if err := d.ReceiveForeignInsertClaim("甲", model.Dispute{
		ID: foreignID, RealLine: anchor, Action: model.ActionInsert, Person: "丁",
		Content: []string{"D"}, Followers: []string{},
	}, ownID); err != nil {
		t.Fatal(err)
	}
	out, err := d.RequestFollow("甲", foreignID, 1)
	if err != nil || out.DisputeID != foreignID {
		t.Fatalf("RequestFollow foreign.ID: out=%+v err=%v", out, err)
	}
	out2, err := d.RequestFollow("丁", ownID, 2)
	if err != nil || out2.DisputeID != ownID {
		t.Fatalf("RequestFollow ownID: out=%+v err=%v", out2, err)
	}
}

func TestReceiveForeignInsertClaimNoSegJustForeign(t *testing.T) {
	d := New("t")
	anchor := view(t, d).Lines[0].ID
	beforeN := len(view(t, d).Lines)
	foreignID := model.NewID()
	if err := d.ReceiveForeignInsertClaim("甲", model.Dispute{
		ID: foreignID, RealLine: anchor, Action: model.ActionInsert, Person: "丁", Content: []string{"D"},
	}, model.ID{}); err != nil {
		t.Fatal(err)
	}
	after := view(t, d)
	if len(after.Lines) != beforeN {
		t.Fatalf("无段不得加空白正式行: %d→%d", beforeN, len(after.Lines))
	}
	if len(after.Disputes) != 1 {
		t.Fatalf("只登记 foreign: %+v", after.Disputes)
	}
	ding, _ := byPerson(after, "丁")
	_, hasSelf := byPerson(after, "甲")
	if ding.ID != foreignID || hasSelf {
		t.Fatalf("无本人候选: %+v", after.Disputes)
	}
	snap := len(d.disputes)
	if err := d.ReceiveForeignInsertClaim("甲", model.Dispute{
		ID: model.NewID(), RealLine: anchor, Action: model.ActionInsert, Person: "甲", Content: []string{"自"},
	}, model.NewID()); err != nil || len(d.disputes) != snap {
		t.Fatal("本人 CC 忽略")
	}
}

func TestReceiveForeignInsertClaimKeepsFollowMetaOnContentCC(t *testing.T) {
	d := New("t")
	anchor := view(t, d).Lines[0].ID
	foreignID := model.NewID()
	if err := d.ReceiveForeignInsertClaim("甲", model.Dispute{
		ID: foreignID, RealLine: anchor, Action: model.ActionInsert, Person: "丁",
		Content: []string{"D"}, Followers: []string{"旁观"},
		Pending: []model.PendingConfirm{{From: "丙", To: "丁", ClientTs: 3}},
	}, model.ID{}); err != nil {
		t.Fatal(err)
	}
	out, err := d.RequestFollow("甲", foreignID, 1)
	if err != nil || out.Status != FollowPending {
		t.Fatalf("甲追随丁: %+v err=%v", out, err)
	}
	if err := d.ReceiveForeignInsertClaim("甲", model.Dispute{
		ID: foreignID, RealLine: anchor, Action: model.ActionInsert, Person: "丁",
		Content: []string{"D2"}, Followers: nil, Pending: nil,
	}, model.ID{}); err != nil {
		t.Fatal(err)
	}
	ding, _ := byPerson(view(t, d), "丁")
	if ding.Content[0] != "D2" || len(ding.Followers) != 1 || ding.Followers[0] != "旁观" || len(ding.Pending) != 2 {
		t.Fatalf("Insert 同 ID 空 metadata 保追随: %+v", ding)
	}
}

func TestReceiveForeignInsertClaimRejectsBadInput(t *testing.T) {
	d := New("t")
	anchor := view(t, d).Lines[0].ID
	ownID := model.NewID()
	bad := []model.Dispute{
		{ID: model.NewID(), RealLine: anchor, Action: model.ActionEdit, Person: "乙", Content: []string{"y"}},
		{ID: model.ID{}, RealLine: anchor, Action: model.ActionInsert, Person: "乙", Content: []string{"y"}},
		{ID: model.NewID(), RealLine: model.ID{}, Action: model.ActionInsert, Person: "乙", Content: []string{"y"}},
		{ID: model.NewID(), RealLine: model.NewID(), Action: model.ActionInsert, Person: "乙", Content: []string{"y"}},
	}
	for i, foreign := range bad {
		if err := d.ReceiveForeignInsertClaim("甲", foreign, ownID); err == nil {
			t.Fatalf("坏输入[%d]应失败", i)
		}
		if len(d.disputes) != 0 {
			t.Fatalf("坏输入[%d]不得半成品", i)
		}
	}
	aID := model.NewID()
	if err := d.SubmitWith("甲", anchor, model.ActionInsert, []string{"A"}, SubmitOpts{
		AfterSeen: &model.ID{}, LineIDs: []model.ID{aID},
	}); err != nil {
		t.Fatal(err)
	}
	if err := d.ReceiveForeignInsertClaim("甲", model.Dispute{
		ID: model.NewID(), RealLine: anchor, Action: model.ActionInsert, Person: "丁", Content: []string{"D"},
	}, model.ID{}); err == nil {
		t.Fatal("有段时 ownID 为零应拒绝")
	}
	if len(view(t, d).Disputes) != 0 || d.live[claimKey{anchor, model.ActionInsert}] == nil {
		t.Fatal("ownID 为零拒绝后正式段应仍在")
	}
}
