package document

import (
	"testing"

	"github.com/KDreamelf/collaborative-editor/internal/model"
)

func TestStoreExplicitClaim(t *testing.T) {
	d := New("t")
	line := view(t, d).Lines[0].ID
	if err := d.Submit("甲", line, model.ActionEdit, []string{"本地"}); err != nil {
		t.Fatal(err)
	}
	before := view(t, d)
	if before.Lines[0].Content != "本地" || len(before.Disputes) != 0 {
		t.Fatalf("普通 Edit 后应 0 Dispute: %+v", before)
	}
	liveBefore := d.live[claimKey{line, model.ActionEdit}]
	histBefore := len(d.insertHistory)

	claimID := model.NewID()
	claim := model.Dispute{
		ID:        claimID,
		RealLine:  line,
		Action:    model.ActionEdit,
		Person:    "甲",
		Content:   []string{"主张"},
		BaseIDs:   []model.ID{line},
		Followers: []string{"乙"},
		Pending:   []model.PendingConfirm{{From: "丙", To: "甲", ClientTs: 1}},
	}
	if err := d.StoreExplicitClaim(claim); err != nil {
		t.Fatal(err)
	}
	after := view(t, d)
	if after.Lines[0].Content != "本地" || after.Lines[0].Prev != before.Lines[0].Prev || after.Lines[0].Next != before.Lines[0].Next {
		t.Fatalf("正式链不变: %+v", after.Lines[0])
	}
	if len(after.Disputes) != 1 {
		t.Fatalf("Store 后恰 1: %+v", after.Disputes)
	}
	got, ok := byPerson(after, "甲")
	if !ok || got.ID != claimID || got.Content[0] != "主张" || len(got.Followers) != 1 || got.Followers[0] != "乙" {
		t.Fatalf("候选: %+v", got)
	}
	if len(got.BaseIDs) != 1 || got.BaseIDs[0] != line || len(got.Pending) != 1 || got.Pending[0].From != "丙" {
		t.Fatalf("BaseIDs/Pending: %+v", got)
	}
	if d.live[claimKey{line, model.ActionEdit}] != liveBefore || len(d.insertHistory) != histBefore {
		t.Fatal("不得动 live / insertHistory")
	}

	claim.Content = []string{"主张改"}
	claim.Followers = nil
	claim.Pending = nil
	claim.BaseIDs = []model.ID{line, model.NewID()}
	if err := d.StoreExplicitClaim(claim); err != nil {
		t.Fatal(err)
	}
	again := view(t, d)
	if len(again.Disputes) != 1 {
		t.Fatalf("重复更新仍 1: %+v", again.Disputes)
	}
	got, _ = byPerson(again, "甲")
	if got.Content[0] != "主张改" || len(got.BaseIDs) != 2 {
		t.Fatalf("应更新 Content/BaseIDs: %+v", got)
	}
	if len(got.Followers) != 1 || got.Followers[0] != "乙" || len(got.Pending) != 1 || got.Pending[0].From != "丙" {
		t.Fatalf("nil metadata 不得擦追随: %+v", got)
	}

	claim.Content = []string{"再改"}
	claim.Followers = []string{}
	claim.Pending = []model.PendingConfirm{}
	claim.BaseIDs = []model.ID{line}
	if err := d.StoreExplicitClaim(claim); err != nil {
		t.Fatal(err)
	}
	emptyMeta := view(t, d)
	got, _ = byPerson(emptyMeta, "甲")
	if got.Content[0] != "再改" || len(got.BaseIDs) != 1 || got.BaseIDs[0] != line {
		t.Fatalf("显式空 metadata 仍应更新正文: %+v", got)
	}
	if len(got.Followers) != 1 || got.Followers[0] != "乙" || len(got.Pending) != 1 || got.Pending[0].From != "丙" {
		t.Fatalf("显式空 metadata 不得擦追随: %+v", got)
	}
}

func TestStoreExplicitClaimEmptyInsertAndDelete(t *testing.T) {
	d := New("t")
	line := view(t, d).Lines[0].ID

	insID := model.NewID()
	if err := d.StoreExplicitClaim(model.Dispute{
		ID: insID, RealLine: line, Action: model.ActionInsert, Person: "甲", Content: nil,
	}); err != nil {
		t.Fatal(err)
	}
	beforeID := model.NewID()
	if err := d.StoreExplicitClaim(model.Dispute{
		ID: beforeID, RealLine: line, Action: model.ActionInsertBefore, Person: "乙", Content: []string{},
	}); err != nil {
		t.Fatal(err)
	}
	delID := model.NewID()
	if err := d.StoreExplicitClaim(model.Dispute{
		ID: delID, RealLine: line, Action: model.ActionDelete, Person: "丙", Content: nil,
	}); err != nil {
		t.Fatal(err)
	}
	v := view(t, d)
	if len(v.Disputes) != 3 {
		t.Fatalf("空插入/删行合法共 3: %+v", v.Disputes)
	}
	for _, item := range v.Disputes {
		if len(item.Content) != 0 {
			t.Fatalf("应为空 Content: %+v", item)
		}
	}
}

func TestStoreExplicitClaimEditDeleteSwitchSameID(t *testing.T) {
	d := New("t")
	line := view(t, d).Lines[0].ID
	claimID := model.NewID()
	if err := d.StoreExplicitClaim(model.Dispute{
		ID: claimID, RealLine: line, Action: model.ActionEdit, Person: "甲",
		Content: []string{"改"}, Followers: []string{"乙"},
		Pending: []model.PendingConfirm{{From: "丙", To: "甲", ClientTs: 1}},
	}); err != nil {
		t.Fatal(err)
	}
	if err := d.StoreExplicitClaim(model.Dispute{
		ID: claimID, RealLine: line, Action: model.ActionDelete, Person: "甲",
		Content: nil, Followers: nil, Pending: nil,
	}); err != nil {
		t.Fatal(err)
	}
	got, ok := byPerson(view(t, d), "甲")
	if !ok || got.ID != claimID || got.Action != model.ActionDelete || len(got.Content) != 0 {
		t.Fatalf("同 ID Edit→Delete: %+v", got)
	}
	if len(got.Followers) != 1 || got.Followers[0] != "乙" || len(got.Pending) != 1 || got.Pending[0].From != "丙" {
		t.Fatalf("切换保留身份/元数据: %+v", got)
	}

	if err := d.StoreExplicitClaim(model.Dispute{
		ID: claimID, RealLine: line, Action: model.ActionEdit, Person: "甲",
		Content: []string{"又改"}, Followers: []string{}, Pending: []model.PendingConfirm{},
	}); err != nil {
		t.Fatal(err)
	}
	got, _ = byPerson(view(t, d), "甲")
	if got.Action != model.ActionEdit || got.Content[0] != "又改" || got.Followers[0] != "乙" || got.Pending[0].From != "丙" {
		t.Fatalf("同 ID Delete→Edit: %+v", got)
	}
}

func TestStoreExplicitClaimRejects(t *testing.T) {
	d := New("t")
	line := view(t, d).Lines[0].ID
	if err := d.Submit("甲", line, model.ActionEdit, []string{"x"}); err != nil {
		t.Fatal(err)
	}
	before := view(t, d)
	liveBefore := d.live[claimKey{line, model.ActionEdit}]

	claimID := model.NewID()
	if err := d.StoreExplicitClaim(model.Dispute{
		ID: claimID, RealLine: line, Action: model.ActionEdit, Person: "甲", Content: []string{"a"},
	}); err != nil {
		t.Fatal(err)
	}
	insID := model.NewID()
	if err := d.StoreExplicitClaim(model.Dispute{
		ID: insID, RealLine: line, Action: model.ActionInsert, Person: "戊", Content: []string{"i"},
	}); err != nil {
		t.Fatal(err)
	}

	bad := []model.Dispute{
		{ID: claimID, RealLine: line, Action: model.ActionEdit, Person: "乙", Content: []string{"b"}},                // 同 ID 换人
		{ID: claimID, RealLine: line, Action: model.ActionInsert, Person: "甲", Content: []string{"跨方向"}},           // 同 ID 跨方向
		{ID: insID, RealLine: line, Action: model.ActionInsertBefore, Person: "戊", Content: []string{"换向"}},       // Insert↔Before
		{ID: model.NewID(), RealLine: model.NewID(), Action: model.ActionEdit, Person: "丙", Content: []string{"c"}}, // 坏行
		{ID: model.ID{}, RealLine: line, Action: model.ActionEdit, Person: "丁", Content: []string{"d"}},
		{ID: model.NewID(), RealLine: model.ID{}, Action: model.ActionEdit, Person: "丁", Content: []string{"d"}},
		{ID: model.NewID(), RealLine: line, Action: model.ActionEdit, Person: "", Content: []string{"d"}},
		{ID: model.NewID(), RealLine: line, Action: "乱", Person: "丁", Content: []string{"d"}},
		{ID: model.NewID(), RealLine: line, Action: model.ActionEdit, Person: "丁", Content: nil},             // Edit 空内容
		{ID: model.NewID(), RealLine: line, Action: model.ActionEdit, Person: "甲", Content: []string{"第二份"}}, // 同人同槽不同 ID
		{ID: model.NewID(), RealLine: line, Action: model.ActionDelete, Person: "甲", Content: nil},           // 同人 body 槽不同 ID
	}
	for i, claim := range bad {
		snap := len(d.disputes)
		if err := d.StoreExplicitClaim(claim); err == nil {
			t.Fatalf("坏输入[%d]应失败", i)
		}
		if len(d.disputes) != snap {
			t.Fatalf("坏输入[%d]零突变: %d→%d", i, snap, len(d.disputes))
		}
	}

	after := view(t, d)
	if after.Lines[0].Content != before.Lines[0].Content || after.Lines[0].Prev != before.Lines[0].Prev || after.Lines[0].Next != before.Lines[0].Next {
		t.Fatalf("拒绝后链不变: %+v", after.Lines[0])
	}
	if len(after.Disputes) != 2 {
		t.Fatalf("仍恰 2: %+v", after.Disputes)
	}
	if d.live[claimKey{line, model.ActionEdit}] != liveBefore {
		t.Fatal("live 不变")
	}
}
