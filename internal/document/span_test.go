package document

import (
	"testing"

	"github.com/KDreamelf/collaborative-editor/internal/model"
)

func setupSpanDoc(t *testing.T) (d *Doc, l1, l2, l3 model.ID) {
	t.Helper()
	d = New("span")
	if err := d.EnsureLines(3); err != nil {
		t.Fatal(err)
	}
	v := view(t, d)
	l1, l2, l3 = v.Lines[0].ID, v.Lines[1].ID, v.Lines[2].ID
	if err := d.Submit("种子", l1, model.ActionEdit, []string{"L1"}); err != nil {
		t.Fatal(err)
	}
	if err := d.Submit("种子", l2, model.ActionEdit, []string{"L2"}); err != nil {
		t.Fatal(err)
	}
	if err := d.Submit("种子", l3, model.ActionEdit, []string{"L3"}); err != nil {
		t.Fatal(err)
	}
	// 清掉种子 live，只留正文基准。
	delete(d.live, claimKey{l1, model.ActionEdit})
	delete(d.live, claimKey{l2, model.ActionEdit})
	delete(d.live, claimKey{l3, model.ActionEdit})
	d.clearRecentEdit(l1)
	d.clearRecentEdit(l2)
	d.clearRecentEdit(l3)
	return d, l1, l2, l3
}

// ① 连续正文基准匹配 → 直接入链；View→BSON→Load 恢复编辑来源。
func TestSpanEditSyncedWriteAndLoad(t *testing.T) {
	d, l1, l2, l3 := setupSpanDoc(t)
	newID := model.NewID()
	if err := d.SubmitSpanEdit("甲", SpanEditOpts{
		BaseIDs:     []model.ID{l1, l2},
		BaseTexts:   []string{"L1", "L2"},
		AfterSeen:   l3,
		Replacement: []string{"X", "Y"},
		LineIDs:     []model.ID{newID},
	}); err != nil {
		t.Fatal(err)
	}
	after := view(t, d)
	if len(after.Lines) != 3 || after.Lines[0].Content != "X" || after.Lines[1].Content != "Y" || after.Lines[2].Content != "L3" {
		t.Fatalf("应替换跨度为 X,Y 且 L3 只出现一次: %+v", after.Lines)
	}
	if after.Lines[0].ID != l1 || after.Lines[1].ID != newID || after.Lines[2].ID != l3 {
		t.Fatalf("首行 ID 稳定、新行用预生 ID: %+v", after.Lines)
	}
	if after.Lines[0].EditOrigin == nil || after.Lines[0].EditOrigin.Person != "甲" {
		t.Fatalf("应有编辑来源: %+v", after.Lines[0].EditOrigin)
	}
	if len(after.Disputes) != 0 {
		t.Fatalf("同步入链无争议: %+v", after.Disputes)
	}

	loaded := roundTripLoad(t, d)
	live := loaded.live[claimKey{l1, model.ActionEdit}]
	if live == nil || live.person != "甲" || !sameStrings(live.content, []string{"X", "Y"}) {
		t.Fatalf("Load 应恢复跨度 live: %+v", live)
	}
	if !sameIDs(live.baseIDs, []model.ID{l1, l2}) || !sameStrings(live.baseTexts, []string{"L1", "L2"}) {
		t.Fatalf("Load 应恢复基准: %+v", live)
	}
	lv := view(t, loaded)
	if lv.Lines[0].EditOrigin == nil || lv.Lines[0].EditOrigin.Person != "甲" {
		t.Fatalf("View 应带编辑来源: %+v", lv.Lines[0].EditOrigin)
	}
}

func TestSpanEditSyncedReplaceWithOneLine(t *testing.T) {
	d, l1, l2, l3 := setupSpanDoc(t)
	if err := d.SubmitSpanEdit("甲", SpanEditOpts{
		BaseIDs:     []model.ID{l1, l2},
		BaseTexts:   []string{"L1", "L2"},
		AfterSeen:   l3,
		Replacement: []string{"ONLY"},
	}); err != nil {
		t.Fatal(err)
	}
	after := view(t, d)
	if len(after.Lines) != 2 || after.Lines[0].Content != "ONLY" || after.Lines[1].Content != "L3" {
		t.Fatalf("两行缩一行: %+v", after.Lines)
	}
	if after.Lines[0].ID != l1 || after.Lines[1].ID != l3 {
		t.Fatalf("首行与后文 ID 稳定: %+v", after.Lines)
	}
}

// ② 另一人基于旧快照提交不同结果 → 收回、恢复基准、两人整段候选。
func TestSpanEditStaleOpensTwoCandidates(t *testing.T) {
	d, l1, l2, l3 := setupSpanDoc(t)
	yID := model.NewID()
	if err := d.SubmitSpanEdit("甲", SpanEditOpts{
		BaseIDs:     []model.ID{l1, l2},
		BaseTexts:   []string{"L1", "L2"},
		AfterSeen:   l3,
		Replacement: []string{"A1", "A2"},
		LineIDs:     []model.ID{yID},
	}); err != nil {
		t.Fatal(err)
	}
	pID := model.NewID()
	if err := d.SubmitSpanEdit("乙", SpanEditOpts{
		BaseIDs:     []model.ID{l1, l2},
		BaseTexts:   []string{"L1", "L2"},
		AfterSeen:   l3,
		Replacement: []string{"B1", "B2"},
		LineIDs:     []model.ID{pID},
	}); err != nil {
		t.Fatal(err)
	}
	after := view(t, d)
	if len(after.Lines) != 3 || after.Lines[0].Content != "L1" || after.Lines[1].Content != "L2" || after.Lines[2].Content != "L3" {
		t.Fatalf("应恢复共同基准 L1,L2,L3: %+v", after.Lines)
	}
	if after.Lines[0].ID != l1 || after.Lines[1].ID != l2 || after.Lines[2].ID != l3 {
		t.Fatalf("旧行 ID 必须稳定恢复: %+v", after.Lines)
	}
	if len(after.Disputes) != 2 {
		t.Fatalf("两人各一份: %+v", after.Disputes)
	}
	jia, _ := byPerson(after, "甲")
	yi, _ := byPerson(after, "乙")
	if !sameStrings(jia.Content, []string{"A1", "A2"}) || !sameStrings(yi.Content, []string{"B1", "B2"}) {
		t.Fatalf("整段候选: 甲=%+v 乙=%+v", jia.Content, yi.Content)
	}
	if !sameIDs(jia.BaseIDs, []model.ID{l1, l2}) || !sameIDs(yi.BaseIDs, []model.ID{l1, l2}) {
		t.Fatalf("争议应带跨度基准行: 甲=%+v 乙=%+v", jia.BaseIDs, yi.BaseIDs)
	}
}

// ③ 同一人本地快照未更新时连续改自己的整段 → 原地更新，不生第二份。
func TestSpanEditSamePersonUpdatesInPlace(t *testing.T) {
	d, l1, l2, l3 := setupSpanDoc(t)
	id1 := model.NewID()
	if err := d.SubmitSpanEdit("甲", SpanEditOpts{
		BaseIDs:     []model.ID{l1, l2},
		BaseTexts:   []string{"L1", "L2"},
		AfterSeen:   l3,
		Replacement: []string{"V1", "V2"},
		LineIDs:     []model.ID{id1},
	}); err != nil {
		t.Fatal(err)
	}
	id2 := model.NewID()
	if err := d.SubmitSpanEdit("甲", SpanEditOpts{
		BaseIDs:     []model.ID{l1, l2},
		BaseTexts:   []string{"L1", "L2"},
		AfterSeen:   l3,
		Replacement: []string{"V3", "V4"},
		LineIDs:     []model.ID{id2},
	}); err != nil {
		t.Fatal(err)
	}
	after := view(t, d)
	if len(after.Disputes) != 0 {
		t.Fatalf("不应生第二份争议: %+v", after.Disputes)
	}
	if len(after.Lines) != 3 || after.Lines[0].Content != "V3" || after.Lines[1].Content != "V4" || after.Lines[2].Content != "L3" {
		t.Fatalf("应原地换成 V3,V4: %+v", after.Lines)
	}
	if after.Lines[1].ID != id2 {
		t.Fatalf("新行应用新预生 ID: %v", after.Lines[1].ID)
	}
	live := d.live[claimKey{l1, model.ActionEdit}]
	if live == nil || live.person != "甲" || !sameStrings(live.content, []string{"V3", "V4"}) {
		t.Fatalf("live 应更新为最新: %+v", live)
	}
}

// ④ 追随接受整段赢家后正确替换原跨度；后方独立正文只出现一次。
func TestSpanEditFollowResolve(t *testing.T) {
	d, l1, l2, l3 := setupSpanDoc(t)
	if err := d.SubmitSpanEdit("甲", SpanEditOpts{
		BaseIDs:     []model.ID{l1, l2},
		BaseTexts:   []string{"L1", "L2"},
		AfterSeen:   l3,
		Replacement: []string{"A1", "A2"},
		LineIDs:     []model.ID{model.NewID()},
	}); err != nil {
		t.Fatal(err)
	}
	if err := d.SubmitSpanEdit("乙", SpanEditOpts{
		BaseIDs:     []model.ID{l1, l2},
		BaseTexts:   []string{"L1", "L2"},
		AfterSeen:   l3,
		Replacement: []string{"B-only"},
	}); err != nil {
		t.Fatal(err)
	}
	v := view(t, d)
	jia := byPersonMust(t, v, "甲")
	if _, err := d.RequestFollow("乙", jia.ID, 1); err != nil {
		t.Fatal(err)
	}
	if _, err := d.AnswerFollow("甲", "乙", jia.ID, true); err != nil {
		t.Fatal(err)
	}
	won := view(t, d)
	if len(won.Disputes) != 0 {
		t.Fatalf("追随后争议应收口: %+v", won.Disputes)
	}
	if len(won.Lines) != 3 || won.Lines[0].Content != "A1" || won.Lines[1].Content != "A2" || won.Lines[2].Content != "L3" {
		t.Fatalf("赢家整段入链且 L3 一次: %+v", won.Lines)
	}
	if won.Lines[0].ID != l1 || won.Lines[2].ID != l3 {
		t.Fatalf("首行与后方正文 ID 稳定: %+v", won.Lines)
	}
	nL3 := 0
	for _, ln := range won.Lines {
		if ln.Content == "L3" {
			nL3++
		}
	}
	if nL3 != 1 {
		t.Fatalf("L3 应只出现一次: %+v", won.Lines)
	}
}

// ⑤ 跨度内未决插入 → 明确错误，零变化。
func TestSpanEditBlockedByPendingInsert(t *testing.T) {
	d, l1, l2, l3 := setupSpanDoc(t)
	if err := d.Submit("丙", l1, model.ActionInsert, []string{"ins"}); err != nil {
		t.Fatal(err)
	}
	before := view(t, d)
	err := d.SubmitSpanEdit("甲", SpanEditOpts{
		BaseIDs:     []model.ID{l1, l2},
		BaseTexts:   []string{"L1", "L2"},
		AfterSeen:   l3,
		Replacement: []string{"X"},
	})
	if err != ErrSpanBlocked {
		t.Fatalf("应拒绝: %v", err)
	}
	after := view(t, d)
	if len(after.Lines) != len(before.Lines) || len(after.Disputes) != len(before.Disputes) {
		t.Fatalf("拒绝不得改正文/争议: before=%+v after=%+v", before, after)
	}
	if d.live[claimKey{l1, model.ActionInsert}] == nil {
		t.Fatal("不得动插入 live")
	}
}

func TestSpanEditBlockedByInnerDispute(t *testing.T) {
	d, l1, l2, l3 := setupSpanDoc(t)
	if err := d.Submit("甲", l2, model.ActionEdit, []string{"x"}); err != nil {
		t.Fatal(err)
	}
	if err := d.Submit("乙", l2, model.ActionEdit, []string{"y"}); err != nil {
		t.Fatal(err)
	}
	before := view(t, d)
	err := d.SubmitSpanEdit("丙", SpanEditOpts{
		BaseIDs:     []model.ID{l1, l2},
		BaseTexts:   []string{"L1", "L2"},
		AfterSeen:   l3,
		Replacement: []string{"Z"},
	})
	if err != ErrSpanBlocked {
		t.Fatalf("子争议应拒绝: %v", err)
	}
	after := view(t, d)
	if after.Lines[0].Content != before.Lines[0].Content || len(after.Disputes) != len(before.Disputes) {
		t.Fatalf("零变化: before=%+v after=%+v", before, after)
	}
}

// 甲先单行改首行（快照未刷新），再跨 L1+L2 整段：回退本人 live 后写入。
func TestSpanEditFoldOwnFirstLineLive(t *testing.T) {
	d, l1, l2, l3 := setupSpanDoc(t)
	if err := d.SubmitWith("甲", l1, model.ActionEdit, []string{"abcd"}, SubmitOpts{BaseContent: strPtr("L1")}); err != nil {
		t.Fatal(err)
	}
	live := d.live[claimKey{l1, model.ActionEdit}]
	if live == nil || live.before != "L1" || live.content[0] != "abcd" {
		t.Fatalf("前置单行 live: %+v", live)
	}
	if err := d.SubmitSpanEdit("甲", SpanEditOpts{
		BaseIDs:     []model.ID{l1, l2},
		BaseTexts:   []string{"L1", "L2"},
		AfterSeen:   l3,
		Replacement: []string{"X"},
	}); err != nil {
		t.Fatal(err)
	}
	after := view(t, d)
	if len(after.Disputes) != 0 {
		t.Fatalf("不应争议: %+v", after.Disputes)
	}
	if len(after.Lines) != 2 || after.Lines[0].Content != "X" || after.Lines[1].Content != "L3" {
		t.Fatalf("整段应为 X|L3: %+v", after.Lines)
	}
	if after.Lines[0].ID != l1 || after.Lines[1].ID != l3 {
		t.Fatalf("首行与后文 ID: %+v", after.Lines)
	}
	spanLive := d.live[claimKey{l1, model.ActionEdit}]
	if spanLive == nil || spanLive.person != "甲" || !sameStrings(spanLive.content, []string{"X"}) || !sameIDs(spanLive.baseIDs, []model.ID{l1, l2}) {
		t.Fatalf("应换成本人跨度 live: %+v", spanLive)
	}
}

// 甲先单行改内行，再跨选区整段；经 View→Load 后仍可回退续写。
func TestSpanEditFoldOwnInnerLineLiveAfterLoad(t *testing.T) {
	d, l1, l2, l3 := setupSpanDoc(t)
	if err := d.SubmitWith("甲", l2, model.ActionEdit, []string{"inner"}, SubmitOpts{BaseContent: strPtr("L2")}); err != nil {
		t.Fatal(err)
	}
	loaded := roundTripLoad(t, d)
	live := loaded.live[claimKey{l2, model.ActionEdit}]
	if live == nil || live.person != "甲" || live.before != "L2" {
		t.Fatalf("Load 应恢复内行 live: %+v", live)
	}
	if err := loaded.SubmitSpanEdit("甲", SpanEditOpts{
		BaseIDs:     []model.ID{l1, l2},
		BaseTexts:   []string{"L1", "L2"},
		AfterSeen:   l3,
		Replacement: []string{"X", "Y"},
		LineIDs:     []model.ID{model.NewID()},
	}); err != nil {
		t.Fatal(err)
	}
	after := view(t, loaded)
	if len(after.Disputes) != 0 || len(after.Lines) != 3 {
		t.Fatalf("应整段入链无争议: %+v", after)
	}
	if after.Lines[0].Content != "X" || after.Lines[1].Content != "Y" || after.Lines[2].Content != "L3" {
		t.Fatalf("结果: %+v", after.Lines)
	}
	if loaded.live[claimKey{l2, model.ActionEdit}] != nil {
		t.Fatal("内行单行 live 应已回退清除")
	}
	spanLive := loaded.live[claimKey{l1, model.ActionEdit}]
	if spanLive == nil || !sameIDs(spanLive.baseIDs, []model.ID{l1, l2}) {
		t.Fatalf("跨度 live 在首行: %+v", spanLive)
	}
}

// 甲改内行，乙旧快照跨段：甲提升整段候选，正式链回基线。
func TestSpanEditPromoteOthersInnerLive(t *testing.T) {
	d, l1, l2, l3 := setupSpanDoc(t)
	// 基线改成 一/二/三 语义：沿用 L1/L2/L3 字面即可。
	if err := d.SubmitWith("甲", l2, model.ActionEdit, []string{"甲二"}, SubmitOpts{BaseContent: strPtr("L2")}); err != nil {
		t.Fatal(err)
	}
	if err := d.SubmitSpanEdit("乙", SpanEditOpts{
		BaseIDs:     []model.ID{l1, l2},
		BaseTexts:   []string{"L1", "L2"},
		AfterSeen:   l3,
		Replacement: []string{"乙整段"},
	}); err != nil {
		t.Fatal(err)
	}
	after := view(t, d)
	if len(after.Lines) != 3 || after.Lines[0].Content != "L1" || after.Lines[1].Content != "L2" || after.Lines[2].Content != "L3" {
		t.Fatalf("正式链应回基线: %+v", after.Lines)
	}
	if after.Lines[0].ID != l1 || after.Lines[1].ID != l2 || after.Lines[2].ID != l3 {
		t.Fatalf("ID 不变: %+v", after.Lines)
	}
	if len(after.Disputes) != 2 {
		t.Fatalf("甲乙各一份: %+v", after.Disputes)
	}
	jia, _ := byPerson(after, "甲")
	yi, _ := byPerson(after, "乙")
	if !sameStrings(jia.Content, []string{"L1", "甲二"}) {
		t.Fatalf("甲候选: %+v", jia.Content)
	}
	if !sameStrings(yi.Content, []string{"乙整段"}) {
		t.Fatalf("乙候选: %+v", yi.Content)
	}
	if !sameIDs(jia.BaseIDs, []model.ID{l1, l2}) || !sameIDs(yi.BaseIDs, []model.ID{l1, l2}) {
		t.Fatalf("同跨度 BaseIDs: 甲=%+v 乙=%+v", jia.BaseIDs, yi.BaseIDs)
	}
}

func TestSpanEditPromoteOthersFirstLineLive(t *testing.T) {
	d, l1, l2, l3 := setupSpanDoc(t)
	if err := d.SubmitWith("甲", l1, model.ActionEdit, []string{"甲一"}, SubmitOpts{BaseContent: strPtr("L1")}); err != nil {
		t.Fatal(err)
	}
	if err := d.SubmitSpanEdit("乙", SpanEditOpts{
		BaseIDs:     []model.ID{l1, l2},
		BaseTexts:   []string{"L1", "L2"},
		AfterSeen:   l3,
		Replacement: []string{"乙整段"},
	}); err != nil {
		t.Fatal(err)
	}
	after := view(t, d)
	if after.Lines[0].Content != "L1" || after.Lines[1].Content != "L2" {
		t.Fatalf("回基线: %+v", after.Lines)
	}
	jia, _ := byPerson(after, "甲")
	yi, _ := byPerson(after, "乙")
	if !sameStrings(jia.Content, []string{"甲一", "L2"}) || !sameStrings(yi.Content, []string{"乙整段"}) {
		t.Fatalf("甲=%+v 乙=%+v", jia.Content, yi.Content)
	}
}

// 两人各改一行 + 第三人跨段：三人各一份，不合成无人版本。
func TestSpanEditPromoteTwoAuthorsLineLivesAfterLoad(t *testing.T) {
	d, l1, l2, l3 := setupSpanDoc(t)
	if err := d.SubmitWith("甲", l1, model.ActionEdit, []string{"甲一"}, SubmitOpts{BaseContent: strPtr("L1")}); err != nil {
		t.Fatal(err)
	}
	if err := d.SubmitWith("丙", l2, model.ActionEdit, []string{"丙二"}, SubmitOpts{BaseContent: strPtr("L2")}); err != nil {
		t.Fatal(err)
	}
	loaded := roundTripLoad(t, d)
	if loaded.live[claimKey{l1, model.ActionEdit}] == nil || loaded.live[claimKey{l2, model.ActionEdit}] == nil {
		t.Fatal("Load 应恢复两行 live")
	}
	if err := loaded.SubmitSpanEdit("乙", SpanEditOpts{
		BaseIDs:     []model.ID{l1, l2},
		BaseTexts:   []string{"L1", "L2"},
		AfterSeen:   l3,
		Replacement: []string{"乙整段"},
	}); err != nil {
		t.Fatal(err)
	}
	after := view(t, loaded)
	if len(after.Disputes) != 3 {
		t.Fatalf("甲丙乙三份: %+v", after.Disputes)
	}
	jia, _ := byPerson(after, "甲")
	bing, _ := byPerson(after, "丙")
	yi, _ := byPerson(after, "乙")
	if !sameStrings(jia.Content, []string{"甲一", "L2"}) {
		t.Fatalf("甲只叠自己的行: %+v", jia.Content)
	}
	if !sameStrings(bing.Content, []string{"L1", "丙二"}) {
		t.Fatalf("丙只叠自己的行: %+v", bing.Content)
	}
	if !sameStrings(yi.Content, []string{"乙整段"}) {
		t.Fatalf("乙: %+v", yi.Content)
	}
	if after.Lines[0].Content != "L1" || after.Lines[1].Content != "L2" || after.Lines[2].Content != "L3" {
		t.Fatalf("基线恢复: %+v", after.Lines)
	}
}

// 甲已入链 [X,Y]，乙同旧快照同结果：保持正式链，零争议。
func TestSpanEditSameReplacementNoDispute(t *testing.T) {
	d, l1, l2, l3 := setupSpanDoc(t)
	yID := model.NewID()
	if err := d.SubmitSpanEdit("甲", SpanEditOpts{
		BaseIDs:     []model.ID{l1, l2},
		BaseTexts:   []string{"L1", "L2"},
		AfterSeen:   l3,
		Replacement: []string{"X", "Y"},
		LineIDs:     []model.ID{yID},
	}); err != nil {
		t.Fatal(err)
	}
	before := view(t, d)
	if err := d.SubmitSpanEdit("乙", SpanEditOpts{
		BaseIDs:     []model.ID{l1, l2},
		BaseTexts:   []string{"L1", "L2"},
		AfterSeen:   l3,
		Replacement: []string{"X", "Y"},
		LineIDs:     []model.ID{model.NewID()},
	}); err != nil {
		t.Fatal(err)
	}
	after := view(t, d)
	if len(after.Disputes) != 0 {
		t.Fatalf("同结果不应开争议: %+v", after.Disputes)
	}
	if len(after.Lines) != len(before.Lines) {
		t.Fatalf("正式链行数不变: before=%d after=%d", len(before.Lines), len(after.Lines))
	}
	for i := range before.Lines {
		if after.Lines[i].ID != before.Lines[i].ID || after.Lines[i].Content != before.Lines[i].Content {
			t.Fatalf("正式链不变: before=%+v after=%+v", before.Lines, after.Lines)
		}
	}
	live := d.live[claimKey{l1, model.ActionEdit}]
	if live == nil || live.person != "甲" {
		t.Fatalf("甲 live 应保留: %+v", live)
	}
}

// 子行已改时，按当前结果段比，不拿 live.content 误判相等。
func TestSpanEditSameLiveContentButChildChangedOpensDispute(t *testing.T) {
	d, l1, l2, l3 := setupSpanDoc(t)
	yID := model.NewID()
	if err := d.SubmitSpanEdit("甲", SpanEditOpts{
		BaseIDs:     []model.ID{l1, l2},
		BaseTexts:   []string{"L1", "L2"},
		AfterSeen:   l3,
		Replacement: []string{"X", "Y"},
		LineIDs:     []model.ID{yID},
	}); err != nil {
		t.Fatal(err)
	}
	if err := d.Submit("丙", yID, model.ActionEdit, []string{"Z"}); err != nil {
		t.Fatal(err)
	}
	// 乙仍交 live 原文 [X,Y]，当前结果是 [X,Z] → 应开争议而非短路。
	if err := d.SubmitSpanEdit("乙", SpanEditOpts{
		BaseIDs:     []model.ID{l1, l2},
		BaseTexts:   []string{"L1", "L2"},
		AfterSeen:   l3,
		Replacement: []string{"X", "Y"},
		LineIDs:     []model.ID{model.NewID()},
	}); err != nil {
		t.Fatal(err)
	}
	after := view(t, d)
	if len(after.Disputes) != 3 {
		t.Fatalf("应甲/丙提升/乙 三份: %+v", after.Disputes)
	}
}

// A 整段入链 → B 改结果行 Y → C 旧快照整段：B 提升为独立候选，不丢主张。
func TestSpanEditPromoteChildEditOnResult(t *testing.T) {
	d, l1, l2, l3 := setupSpanDoc(t)
	yID := model.NewID()
	if err := d.SubmitSpanEdit("甲", SpanEditOpts{
		BaseIDs:     []model.ID{l1, l2},
		BaseTexts:   []string{"L1", "L2"},
		AfterSeen:   l3,
		Replacement: []string{"X", "Y"},
		LineIDs:     []model.ID{yID},
	}); err != nil {
		t.Fatal(err)
	}
	if err := d.Submit("乙", yID, model.ActionEdit, []string{"Z"}); err != nil {
		t.Fatal(err)
	}
	mid := view(t, d)
	if mid.Lines[1].Content != "Z" {
		t.Fatalf("前置：Y 应为 Z: %+v", mid.Lines)
	}

	cID := model.NewID()
	if err := d.SubmitSpanEdit("丙", SpanEditOpts{
		BaseIDs:     []model.ID{l1, l2},
		BaseTexts:   []string{"L1", "L2"},
		AfterSeen:   l3,
		Replacement: []string{"C1", "C2"},
		LineIDs:     []model.ID{cID},
	}); err != nil {
		t.Fatal(err)
	}
	after := view(t, d)
	if len(after.Lines) != 3 || after.Lines[0].Content != "L1" || after.Lines[1].Content != "L2" || after.Lines[2].Content != "L3" {
		t.Fatalf("应恢复基准 L1,L2,L3: %+v", after.Lines)
	}
	if after.Lines[0].ID != l1 || after.Lines[1].ID != l2 {
		t.Fatalf("旧行 ID 稳定: %+v", after.Lines)
	}
	if len(after.Disputes) != 3 {
		t.Fatalf("甲乙丙三份: %+v", after.Disputes)
	}
	jia, _ := byPerson(after, "甲")
	yi, _ := byPerson(after, "乙")
	bing, _ := byPerson(after, "丙")
	if !sameStrings(jia.Content, []string{"X", "Y"}) {
		t.Fatalf("甲=[X,Y]: %+v", jia.Content)
	}
	if !sameStrings(yi.Content, []string{"X", "Z"}) {
		t.Fatalf("乙提升=[X,Z]: %+v", yi.Content)
	}
	if !sameStrings(bing.Content, []string{"C1", "C2"}) {
		t.Fatalf("丙=[C1,C2]: %+v", bing.Content)
	}
	bases := []model.ID{l1, l2}
	if !sameIDs(jia.BaseIDs, bases) || !sameIDs(yi.BaseIDs, bases) || !sameIDs(bing.BaseIDs, bases) {
		t.Fatalf("三份同跨度 BaseIDs: 甲=%+v 乙=%+v 丙=%+v", jia.BaseIDs, yi.BaseIDs, bing.BaseIDs)
	}
}

func TestSpanEditPromoteChildEditAfterLoad(t *testing.T) {
	d, l1, l2, l3 := setupSpanDoc(t)
	yID := model.NewID()
	if err := d.SubmitSpanEdit("甲", SpanEditOpts{
		BaseIDs:     []model.ID{l1, l2},
		BaseTexts:   []string{"L1", "L2"},
		AfterSeen:   l3,
		Replacement: []string{"X", "Y"},
		LineIDs:     []model.ID{yID},
	}); err != nil {
		t.Fatal(err)
	}
	if err := d.Submit("乙", yID, model.ActionEdit, []string{"Z"}); err != nil {
		t.Fatal(err)
	}
	loaded := roundTripLoad(t, d)
	if err := loaded.SubmitSpanEdit("丙", SpanEditOpts{
		BaseIDs:     []model.ID{l1, l2},
		BaseTexts:   []string{"L1", "L2"},
		AfterSeen:   l3,
		Replacement: []string{"C1"},
	}); err != nil {
		t.Fatal(err)
	}
	after := view(t, loaded)
	if len(after.Disputes) != 3 {
		t.Fatalf("Load 后仍三份: %+v", after.Disputes)
	}
	yi, _ := byPerson(after, "乙")
	if !sameStrings(yi.Content, []string{"X", "Z"}) {
		t.Fatalf("乙仍提升: %+v", yi.Content)
	}
	if after.Lines[0].Content != "L1" || after.Lines[1].Content != "L2" {
		t.Fatalf("基准恢复: %+v", after.Lines)
	}
}

// 结果段多行粘贴不能提升：拒绝且零变化。
func TestSpanEditBlockedByResultMultiline(t *testing.T) {
	d, l1, l2, l3 := setupSpanDoc(t)
	yID := model.NewID()
	if err := d.SubmitSpanEdit("甲", SpanEditOpts{
		BaseIDs:     []model.ID{l1, l2},
		BaseTexts:   []string{"L1", "L2"},
		AfterSeen:   l3,
		Replacement: []string{"X", "Y"},
		LineIDs:     []model.ID{yID},
	}); err != nil {
		t.Fatal(err)
	}
	if err := d.Submit("乙", yID, model.ActionEdit, []string{"Z1", "Z2"}); err != nil {
		t.Fatal(err)
	}
	before := view(t, d)
	beforeLiveY := d.live[claimKey{yID, model.ActionEdit}]
	if beforeLiveY == nil {
		t.Fatal("前置：乙 live 在 Y")
	}
	err := d.SubmitSpanEdit("丙", SpanEditOpts{
		BaseIDs:     []model.ID{l1, l2},
		BaseTexts:   []string{"L1", "L2"},
		AfterSeen:   l3,
		Replacement: []string{"C"},
	})
	if err != ErrSpanBlocked {
		t.Fatalf("多行子编辑应拒: %v", err)
	}
	after := view(t, d)
	if len(after.Lines) != len(before.Lines) || len(after.Disputes) != len(before.Disputes) {
		t.Fatalf("零变化 view: before=%+v after=%+v", before, after)
	}
	for i := range before.Lines {
		if after.Lines[i].ID != before.Lines[i].ID || after.Lines[i].Content != before.Lines[i].Content {
			t.Fatalf("正文字节级不变: before=%+v after=%+v", before.Lines, after.Lines)
		}
	}
	liveA := d.live[claimKey{l1, model.ActionEdit}]
	liveY := d.live[claimKey{yID, model.ActionEdit}]
	if liveA == nil || liveA.person != "甲" || liveY == nil || liveY.person != "乙" {
		t.Fatalf("不得丢甲/乙 live: A=%+v Y=%+v", liveA, liveY)
	}
}

// 首行已有单行争议时跨度进入：垫基准提升同组，不混 BaseIDs。
func TestSpanEditPromoteSingleLineDisputeGroup(t *testing.T) {
	d, l1, l2, l3 := setupSpanDoc(t)
	if err := d.Submit("甲", l1, model.ActionEdit, []string{"a"}); err != nil {
		t.Fatal(err)
	}
	if err := d.Submit("乙", l1, model.ActionEdit, []string{"b"}); err != nil {
		t.Fatal(err)
	}
	before := view(t, d)
	if len(before.Disputes) != 2 || before.Lines[0].Content != "L1" {
		t.Fatalf("前置单行争议: %+v", before)
	}
	if err := d.SubmitSpanEdit("丙", SpanEditOpts{
		BaseIDs:     []model.ID{l1, l2},
		BaseTexts:   []string{"L1", "L2"},
		AfterSeen:   l3,
		Replacement: []string{"C1", "C2"},
		LineIDs:     []model.ID{model.NewID()},
	}); err != nil {
		t.Fatal(err)
	}
	after := view(t, d)
	if len(after.Disputes) != 3 {
		t.Fatalf("提升后三份: %+v", after.Disputes)
	}
	jia, _ := byPerson(after, "甲")
	yi, _ := byPerson(after, "乙")
	bing, _ := byPerson(after, "丙")
	if !sameStrings(jia.Content, []string{"a", "L2"}) || !sameStrings(yi.Content, []string{"b", "L2"}) {
		t.Fatalf("单行应垫 L2: 甲=%+v 乙=%+v", jia.Content, yi.Content)
	}
	if !sameIDs(jia.BaseIDs, []model.ID{l1, l2}) || !sameIDs(yi.BaseIDs, []model.ID{l1, l2}) || !sameIDs(bing.BaseIDs, []model.ID{l1, l2}) {
		t.Fatalf("不得混组: 甲=%+v 乙=%+v 丙=%+v", jia.BaseIDs, yi.BaseIDs, bing.BaseIDs)
	}
}

func TestSpanEditRejectMixedDeleteDispute(t *testing.T) {
	d, l1, l2, l3 := setupSpanDoc(t)
	if err := d.Submit("甲", l1, model.ActionEdit, []string{"a"}); err != nil {
		t.Fatal(err)
	}
	if err := d.Submit("乙", l1, model.ActionEdit, []string{"b"}); err != nil {
		t.Fatal(err)
	}
	// 挂起后清正文再删，造删除争议同槽。
	if err := d.SetSuspended("甲", l1, model.ActionEdit, true); err != nil {
		t.Fatal(err)
	}
	if err := d.SetSuspended("乙", l1, model.ActionEdit, true); err != nil {
		t.Fatal(err)
	}
	ln := d.lines[l1]
	ln.Content = ""
	if err := d.DeleteIfIdle("丁", l1, nil); err != nil {
		t.Fatal(err)
	}
	before := view(t, d)
	err := d.SubmitSpanEdit("丙", SpanEditOpts{
		BaseIDs:     []model.ID{l1, l2},
		BaseTexts:   []string{"", "L2"},
		AfterSeen:   l3,
		Replacement: []string{"C"},
	})
	if err != ErrSpanBlocked {
		t.Fatalf("删行同槽应拒: %v", err)
	}
	after := view(t, d)
	if len(after.Disputes) != len(before.Disputes) {
		t.Fatalf("零变化争议数: before=%d after=%d", len(before.Disputes), len(after.Disputes))
	}
}

// A/B 打开跨度未决后，C 普通改成员行 L2：须提为整段候选；A/B 追随后不得丢 C 或擅自收口。
func TestSpanOpenDisputeMemberEditPromotesAndBlocksEarlyResolve(t *testing.T) {
	d, l1, l2, l3 := setupSpanDoc(t)
	if err := d.SubmitSpanEdit("甲", SpanEditOpts{
		BaseIDs:     []model.ID{l1, l2},
		BaseTexts:   []string{"L1", "L2"},
		AfterSeen:   l3,
		Replacement: []string{"A1", "A2"},
		LineIDs:     []model.ID{model.NewID()},
	}); err != nil {
		t.Fatal(err)
	}
	if err := d.SubmitSpanEdit("乙", SpanEditOpts{
		BaseIDs:     []model.ID{l1, l2},
		BaseTexts:   []string{"L1", "L2"},
		AfterSeen:   l3,
		Replacement: []string{"B1", "B2"},
		LineIDs:     []model.ID{model.NewID()},
	}); err != nil {
		t.Fatal(err)
	}
	mid := view(t, d)
	if len(mid.Disputes) != 2 || mid.Lines[0].Content != "L1" || mid.Lines[1].Content != "L2" {
		t.Fatalf("前置：基准恢复且甲乙两份: %+v", mid)
	}

	if err := d.SubmitWith("丙", l2, model.ActionEdit, []string{"C2"}, SubmitOpts{BaseContent: strPtr("L2")}); err != nil {
		t.Fatal(err)
	}
	after := view(t, d)
	if after.Lines[0].Content != "L1" || after.Lines[1].Content != "L2" {
		t.Fatalf("正式链须保持基准，不得写穿 L2: %+v", after.Lines)
	}
	if len(after.Disputes) != 3 {
		t.Fatalf("丙须进同跨度争议: %+v", after.Disputes)
	}
	bing, ok := byPerson(after, "丙")
	if !ok || !sameStrings(bing.Content, []string{"L1", "C2"}) {
		t.Fatalf("丙候选=基准全文只改 L2: %+v", bing.Content)
	}
	if !sameIDs(bing.BaseIDs, []model.ID{l1, l2}) || bing.RealLine != l1 {
		t.Fatalf("丙须挂首行同 BaseIDs: %+v", bing)
	}
	jia, _ := byPerson(after, "甲")
	yi, _ := byPerson(after, "乙")
	if !sameStrings(jia.Content, []string{"A1", "A2"}) || !sameStrings(yi.Content, []string{"B1", "B2"}) {
		t.Fatalf("不得抹甲乙: 甲=%+v 乙=%+v", jia.Content, yi.Content)
	}

	if _, err := d.RequestFollow("乙", jia.ID, 1); err != nil {
		t.Fatal(err)
	}
	if _, err := d.AnswerFollow("甲", "乙", jia.ID, true); err != nil {
		t.Fatal(err)
	}
	stuck := view(t, d)
	if len(stuck.Disputes) != 3 {
		t.Fatalf("丙未追随则不得收口: %+v", stuck.Disputes)
	}
	if stuck.Lines[0].Content != "L1" || stuck.Lines[1].Content != "L2" {
		t.Fatalf("未收口时正文仍基准: %+v", stuck.Lines)
	}
	bing2, _ := byPerson(stuck, "丙")
	if !sameStrings(bing2.Content, []string{"L1", "C2"}) {
		t.Fatalf("丙主张仍在: %+v", bing2.Content)
	}

	// 丙主动追随甲后才收口；赢家整段入链。
	if _, err := d.RequestFollow("丙", jia.ID, 1); err != nil {
		t.Fatal(err)
	}
	if _, err := d.AnswerFollow("甲", "丙", jia.ID, true); err != nil {
		t.Fatal(err)
	}
	won := view(t, d)
	if len(won.Disputes) != 0 {
		t.Fatalf("丙追随后应收口: %+v", won.Disputes)
	}
	if len(won.Lines) != 3 || won.Lines[0].Content != "A1" || won.Lines[1].Content != "A2" {
		t.Fatalf("甲整段入链: %+v", won.Lines)
	}
}

func TestSpanOpenDisputeMemberEditAfterLoad(t *testing.T) {
	d, l1, l2, l3 := setupSpanDoc(t)
	if err := d.SubmitSpanEdit("甲", SpanEditOpts{
		BaseIDs:     []model.ID{l1, l2},
		BaseTexts:   []string{"L1", "L2"},
		AfterSeen:   l3,
		Replacement: []string{"A1", "A2"},
		LineIDs:     []model.ID{model.NewID()},
	}); err != nil {
		t.Fatal(err)
	}
	if err := d.SubmitSpanEdit("乙", SpanEditOpts{
		BaseIDs:     []model.ID{l1, l2},
		BaseTexts:   []string{"L1", "L2"},
		AfterSeen:   l3,
		Replacement: []string{"B1", "B2"},
		LineIDs:     []model.ID{model.NewID()},
	}); err != nil {
		t.Fatal(err)
	}
	loaded := roundTripLoad(t, d)
	if err := loaded.SubmitWith("丙", l2, model.ActionEdit, []string{"C2"}, SubmitOpts{BaseContent: strPtr("L2")}); err != nil {
		t.Fatal(err)
	}
	after := view(t, loaded)
	if len(after.Disputes) != 3 {
		t.Fatalf("Load 后丙仍进争议: %+v", after.Disputes)
	}
	bing, _ := byPerson(after, "丙")
	if !sameStrings(bing.Content, []string{"L1", "C2"}) || !sameIDs(bing.BaseIDs, []model.ID{l1, l2}) {
		t.Fatalf("Load 后丙候选: %+v", bing)
	}
	if after.Lines[1].Content != "L2" {
		t.Fatalf("不得写穿: %+v", after.Lines)
	}
}

func TestSpanOpenDisputeMemberEditOnFirstLine(t *testing.T) {
	d, l1, l2, l3 := setupSpanDoc(t)
	if err := d.SubmitSpanEdit("甲", SpanEditOpts{
		BaseIDs:     []model.ID{l1, l2},
		BaseTexts:   []string{"L1", "L2"},
		AfterSeen:   l3,
		Replacement: []string{"A1", "A2"},
		LineIDs:     []model.ID{model.NewID()},
	}); err != nil {
		t.Fatal(err)
	}
	if err := d.SubmitSpanEdit("乙", SpanEditOpts{
		BaseIDs:     []model.ID{l1, l2},
		BaseTexts:   []string{"L1", "L2"},
		AfterSeen:   l3,
		Replacement: []string{"B1", "B2"},
		LineIDs:     []model.ID{model.NewID()},
	}); err != nil {
		t.Fatal(err)
	}
	if err := d.SubmitWith("丙", l1, model.ActionEdit, []string{"C1"}, SubmitOpts{BaseContent: strPtr("L1")}); err != nil {
		t.Fatal(err)
	}
	after := view(t, d)
	bing, ok := byPerson(after, "丙")
	if !ok || !sameStrings(bing.Content, []string{"C1", "L2"}) || !sameIDs(bing.BaseIDs, []model.ID{l1, l2}) {
		t.Fatalf("首行普通改也须整段候选: %+v", bing)
	}
	if len(after.Disputes) != 3 || after.Lines[0].Content != "L1" {
		t.Fatalf("三份且正文基准: lines=%+v disputes=%d", after.Lines, len(after.Disputes))
	}
}

func TestSpanOpenDisputeMemberDeleteRejected(t *testing.T) {
	d, l1, l2, l3 := setupSpanDoc(t)
	if err := d.SubmitSpanEdit("甲", SpanEditOpts{
		BaseIDs:     []model.ID{l1, l2},
		BaseTexts:   []string{"L1", "L2"},
		AfterSeen:   l3,
		Replacement: []string{"A1", "A2"},
		LineIDs:     []model.ID{model.NewID()},
	}); err != nil {
		t.Fatal(err)
	}
	if err := d.SubmitSpanEdit("乙", SpanEditOpts{
		BaseIDs:     []model.ID{l1, l2},
		BaseTexts:   []string{"L1", "L2"},
		AfterSeen:   l3,
		Replacement: []string{"B1", "B2"},
		LineIDs:     []model.ID{model.NewID()},
	}); err != nil {
		t.Fatal(err)
	}
	d.lines[l2].Content = ""
	before := view(t, d)
	err := d.DeleteIfIdle("丙", l2, nil)
	if err != ErrSpanBlocked {
		t.Fatalf("未决跨度成员删行应拒: %v", err)
	}
	after := view(t, d)
	if len(after.Disputes) != len(before.Disputes) {
		t.Fatalf("零变化: before=%d after=%d", len(before.Disputes), len(after.Disputes))
	}
	if after.Lines[0].ID != l1 || after.Lines[1].ID != l2 {
		t.Fatalf("链保全: %+v", after.Lines)
	}
}

func TestSpanOpenDisputeMemberMergeRejected(t *testing.T) {
	d, l1, l2, l3 := setupSpanDoc(t)
	if err := d.SubmitSpanEdit("甲", SpanEditOpts{
		BaseIDs:     []model.ID{l1, l2},
		BaseTexts:   []string{"L1", "L2"},
		AfterSeen:   l3,
		Replacement: []string{"A1", "A2"},
		LineIDs:     []model.ID{model.NewID()},
	}); err != nil {
		t.Fatal(err)
	}
	if err := d.SubmitSpanEdit("乙", SpanEditOpts{
		BaseIDs:     []model.ID{l1, l2},
		BaseTexts:   []string{"L1", "L2"},
		AfterSeen:   l3,
		Replacement: []string{"B1", "B2"},
		LineIDs:     []model.ID{model.NewID()},
	}); err != nil {
		t.Fatal(err)
	}
	before := view(t, d)
	err := d.MergeUp("丙", l2, nil)
	if err != ErrHasClaims && err != ErrSpanBlocked {
		t.Fatalf("未决跨度成员合并应拒: %v", err)
	}
	after := view(t, d)
	if len(after.Disputes) != len(before.Disputes) || after.Lines[1].Content != "L2" {
		t.Fatalf("零变化: before=%+v after=%+v", before, after)
	}
}

func TestSpanOpenDisputeInteriorInsertRejected(t *testing.T) {
	d, l1, l2, l3 := setupSpanDoc(t)
	if err := d.SubmitSpanEdit("甲", SpanEditOpts{
		BaseIDs:     []model.ID{l1, l2},
		BaseTexts:   []string{"L1", "L2"},
		AfterSeen:   l3,
		Replacement: []string{"A1", "A2"},
		LineIDs:     []model.ID{model.NewID()},
	}); err != nil {
		t.Fatal(err)
	}
	if err := d.SubmitSpanEdit("乙", SpanEditOpts{
		BaseIDs:     []model.ID{l1, l2},
		BaseTexts:   []string{"L1", "L2"},
		AfterSeen:   l3,
		Replacement: []string{"B1", "B2"},
		LineIDs:     []model.ID{model.NewID()},
	}); err != nil {
		t.Fatal(err)
	}
	before := view(t, d)
	err := d.SubmitWith("丙", l1, model.ActionInsert, []string{"IN"}, SubmitOpts{
		AfterSeen: idPtr(l2),
		LineIDs:   []model.ID{model.NewID()},
	})
	if err != ErrSpanBlocked {
		t.Fatalf("跨度内插入应拒: %v", err)
	}
	after := view(t, d)
	if len(after.Lines) != len(before.Lines) || len(after.Disputes) != len(before.Disputes) {
		t.Fatalf("零变化: before lines=%d disp=%d after lines=%d disp=%d",
			len(before.Lines), len(before.Disputes), len(after.Lines), len(after.Disputes))
	}
}

func openABSpanDispute(t *testing.T) (d *Doc, l1, l2, l3 model.ID) {
	t.Helper()
	d, l1, l2, l3 = setupSpanDoc(t)
	if err := d.SubmitSpanEdit("甲", SpanEditOpts{
		BaseIDs:     []model.ID{l1, l2},
		BaseTexts:   []string{"L1", "L2"},
		AfterSeen:   l3,
		Replacement: []string{"A1", "A2"},
		LineIDs:     []model.ID{model.NewID()},
	}); err != nil {
		t.Fatal(err)
	}
	if err := d.SubmitSpanEdit("乙", SpanEditOpts{
		BaseIDs:     []model.ID{l1, l2},
		BaseTexts:   []string{"L1", "L2"},
		AfterSeen:   l3,
		Replacement: []string{"B1", "B2"},
		LineIDs:     []model.ID{model.NewID()},
	}); err != nil {
		t.Fatal(err)
	}
	return d, l1, l2, l3
}

// 连续改两行须累积本人候选，不得每次从 baseTexts 重建抹掉先前行。
func TestSpanOpenDisputeMemberEditAccumulatesOwnCandidate(t *testing.T) {
	d, l1, l2, _ := openABSpanDispute(t)
	if err := d.SubmitWith("丙", l2, model.ActionEdit, []string{"C2"}, SubmitOpts{BaseContent: strPtr("L2")}); err != nil {
		t.Fatal(err)
	}
	if err := d.SubmitWith("丙", l1, model.ActionEdit, []string{"C1"}, SubmitOpts{BaseContent: strPtr("L1")}); err != nil {
		t.Fatal(err)
	}
	after := view(t, d)
	bing, ok := byPerson(after, "丙")
	if !ok || !sameStrings(bing.Content, []string{"C1", "C2"}) {
		t.Fatalf("须累积 C1+C2，不得抹 C2: %+v", bing.Content)
	}
	if after.Lines[0].Content != "L1" || after.Lines[1].Content != "L2" {
		t.Fatalf("正式链仍基准: %+v", after.Lines)
	}
	jia, _ := byPerson(after, "甲")
	yi, _ := byPerson(after, "乙")
	if !sameStrings(jia.Content, []string{"A1", "A2"}) || !sameStrings(yi.Content, []string{"B1", "B2"}) {
		t.Fatalf("不得抹甲乙")
	}
}

// 改回基准须按人 upsert 保留「原文」主张，不得 early-return 留下旧候选。
func TestSpanOpenDisputeMemberEditBackToBaseKeepsClaim(t *testing.T) {
	d, _, l2, _ := openABSpanDispute(t)
	if err := d.SubmitWith("丙", l2, model.ActionEdit, []string{"C2"}, SubmitOpts{BaseContent: strPtr("L2")}); err != nil {
		t.Fatal(err)
	}
	if err := d.SubmitWith("丙", l2, model.ActionEdit, []string{"L2"}, SubmitOpts{BaseContent: strPtr("L2")}); err != nil {
		t.Fatal(err)
	}
	after := view(t, d)
	bing, ok := byPerson(after, "丙")
	if !ok || !sameStrings(bing.Content, []string{"L1", "L2"}) {
		t.Fatalf("改回基准仍保留丙主张为原文: %+v", bing)
	}
	if len(after.Disputes) != 3 {
		t.Fatalf("三份争议仍在: %d", len(after.Disputes))
	}
}

// 跨度末行锚定插入也拒：胜出后末行删除，未决 insert 会被 unlink 清掉。
func TestSpanOpenDisputeTrailingMemberInsertRejected(t *testing.T) {
	d, _, l2, l3 := openABSpanDispute(t)
	before := view(t, d)
	err := d.SubmitWith("丙", l2, model.ActionInsert, []string{"IN"}, SubmitOpts{
		AfterSeen: idPtr(l3),
		LineIDs:   []model.ID{model.NewID()},
	})
	if err != ErrSpanBlocked {
		t.Fatalf("末行锚定插入应拒: %v", err)
	}
	after := view(t, d)
	if len(after.Lines) != len(before.Lines) || len(after.Disputes) != len(before.Disputes) {
		t.Fatalf("零变化: before lines=%d disp=%d after lines=%d disp=%d",
			len(before.Lines), len(before.Disputes), len(after.Lines), len(after.Disputes))
	}
	if _, ok := byPerson(after, "丙"); ok {
		t.Fatalf("不得留下丙插入争议")
	}
}

// 首次成员多行粘贴：基准全文 splice 成一份整段候选。
func TestSpanOpenDisputeMemberMultilinePasteFirst(t *testing.T) {
	d, l1, l2, _ := openABSpanDispute(t)
	if err := d.SubmitWith("丙", l2, model.ActionEdit, []string{"C2a", "C2b"}, SubmitOpts{
		BaseContent: strPtr("L2"),
		LineIDs:     []model.ID{model.NewID()},
	}); err != nil {
		t.Fatal(err)
	}
	after := view(t, d)
	bing, ok := byPerson(after, "丙")
	if !ok || !sameStrings(bing.Content, []string{"L1", "C2a", "C2b"}) {
		t.Fatalf("首次多行 splice: %+v", bing.Content)
	}
	if !sameIDs(bing.BaseIDs, []model.ID{l1, l2}) || after.Lines[1].Content != "L2" {
		t.Fatalf("挂同跨度且不写穿: %+v lines=%+v", bing, after.Lines)
	}
}

// 已有本人扩长候选后再改另一成员行：无法对位旧基准行 → 拒保全。
func TestSpanOpenDisputeMemberEditRejectsUnalignableOwn(t *testing.T) {
	d, l1, l2, _ := openABSpanDispute(t)
	if err := d.SubmitWith("丙", l2, model.ActionEdit, []string{"C2a", "C2b"}, SubmitOpts{
		BaseContent: strPtr("L2"),
		LineIDs:     []model.ID{model.NewID()},
	}); err != nil {
		t.Fatal(err)
	}
	before := view(t, d)
	err := d.SubmitWith("丙", l1, model.ActionEdit, []string{"C1"}, SubmitOpts{BaseContent: strPtr("L1")})
	if err != ErrSpanBlocked {
		t.Fatalf("扩长后无法对位应拒: %v", err)
	}
	after := view(t, d)
	bing, _ := byPerson(after, "丙")
	if !sameStrings(bing.Content, []string{"L1", "C2a", "C2b"}) {
		t.Fatalf("拒后本人候选保全: %+v", bing.Content)
	}
	if len(after.Disputes) != len(before.Disputes) || after.Lines[0].Content != "L1" {
		t.Fatalf("零变化")
	}
}

// TestUpdateLiveSpanThenPasteMulti：跨度缩成单行后再普通多行粘贴，必须整段写出尾行并更新 EditOrigin。
func TestUpdateLiveSpanThenPasteMulti(t *testing.T) {
	d, l1, l2, l3 := setupSpanDoc(t)
	if err := d.SubmitSpanEdit("甲", SpanEditOpts{
		BaseIDs:     []model.ID{l1, l2},
		BaseTexts:   []string{"L1", "L2"},
		AfterSeen:   l3,
		Replacement: []string{"X"},
	}); err != nil {
		t.Fatal(err)
	}
	qID := model.NewID()
	if err := d.SubmitWith("甲", l1, model.ActionEdit, []string{"P", "Q"}, SubmitOpts{
		BaseContent: strPtr("X"),
		LineIDs:     []model.ID{qID},
	}); err != nil {
		t.Fatal(err)
	}
	after := view(t, d)
	if len(after.Lines) != 3 || after.Lines[0].Content != "P" || after.Lines[1].Content != "Q" || after.Lines[2].Content != "L3" {
		t.Fatalf("应有 P,Q,L3: %+v", after.Lines)
	}
	if after.Lines[0].ID != l1 || after.Lines[1].ID != qID {
		t.Fatalf("首行稳定、Q 用预生 ID: %+v", after.Lines)
	}
	eo := after.Lines[0].EditOrigin
	if eo == nil || !sameStrings(eo.Content, []string{"P", "Q"}) || !sameIDs(eo.LineIDs, []model.ID{l1, qID}) {
		t.Fatalf("EditOrigin 应更新为 P,Q: %+v", eo)
	}
	if !sameIDs(eo.BaseIDs, []model.ID{l1, l2}) || !sameStrings(eo.BaseTexts, []string{"L1", "L2"}) {
		t.Fatalf("原跨度基准应保留: %+v", eo)
	}
	live := d.live[claimKey{l1, model.ActionEdit}]
	if live == nil || !sameStrings(live.content, []string{"P", "Q"}) || !sameIDs(live.ids, []model.ID{qID}) {
		t.Fatalf("live 应整段为 P,Q: %+v", live)
	}

	loaded := roundTripLoad(t, d)
	lv := view(t, loaded)
	if len(lv.Lines) != 3 || lv.Lines[0].Content != "P" || lv.Lines[1].Content != "Q" || lv.Lines[1].ID != qID {
		t.Fatalf("Load 后 Q 仍在: %+v", lv.Lines)
	}
	if lv.Lines[0].EditOrigin == nil || !sameStrings(lv.Lines[0].EditOrigin.Content, []string{"P", "Q"}) {
		t.Fatalf("Load 后 EditOrigin: %+v", lv.Lines[0].EditOrigin)
	}
	llive := loaded.live[claimKey{l1, model.ActionEdit}]
	if llive == nil || !sameStrings(llive.content, []string{"P", "Q"}) || !sameIDs(llive.ids, []model.ID{qID}) {
		t.Fatalf("Load 后 live: %+v", llive)
	}
}

// TestUpdateLiveEditPasteExpandAndShrink：WholeClaim 整份候选扩段/缩段须整体替换旧尾。
func TestUpdateLiveEditPasteExpandAndShrink(t *testing.T) {
	d := New("t")
	if err := d.EnsureLines(1); err != nil {
		t.Fatal(err)
	}
	head := view(t, d).Lines[0].ID
	qID := model.NewID()
	if err := d.SubmitWith("甲", head, model.ActionEdit, []string{"P", "Q"}, SubmitOpts{
		BaseContent: strPtr(""),
		LineIDs:     []model.ID{qID},
	}); err != nil {
		t.Fatal(err)
	}
	sID, tID := model.NewID(), model.NewID()
	if err := d.SubmitWith("甲", head, model.ActionEdit, []string{"R", "S", "T"}, SubmitOpts{
		BaseContent: strPtr("P"),
		LineIDs:     []model.ID{sID, tID},
		WholeClaim:  true,
	}); err != nil {
		t.Fatal(err)
	}
	exp := view(t, d)
	if len(exp.Lines) != 3 || exp.Lines[0].Content != "R" || exp.Lines[1].Content != "S" || exp.Lines[2].Content != "T" {
		t.Fatalf("扩成 R,S,T: %+v", exp.Lines)
	}
	if exp.Lines[1].ID != sID || exp.Lines[2].ID != tID {
		t.Fatalf("新尾用预生 ID: %+v", exp.Lines)
	}
	if _, ok := d.lines[qID]; ok {
		t.Fatalf("旧 Q 应卸链: 仍在")
	}

	if err := d.SubmitWith("甲", head, model.ActionEdit, []string{"R"}, SubmitOpts{
		BaseContent: strPtr("R"),
		WholeClaim:  true,
	}); err != nil {
		t.Fatal(err)
	}
	sh := view(t, d)
	if len(sh.Lines) != 1 || sh.Lines[0].Content != "R" || sh.Lines[0].ID != head {
		t.Fatalf("缩成单行 R: %+v", sh.Lines)
	}
	live := d.live[claimKey{head, model.ActionEdit}]
	if live == nil || !sameStrings(live.content, []string{"R"}) || len(live.ids) != 0 {
		t.Fatalf("缩后 live 无尾: %+v", live)
	}
}

// TestUpdateLiveEditKeepsTail：普通粘贴 [P,Q] 后 SubmitEdit 头改 R，须保留 Q。
func TestUpdateLiveEditKeepsTail(t *testing.T) {
	d := New("t")
	if err := d.EnsureLines(1); err != nil {
		t.Fatal(err)
	}
	head := view(t, d).Lines[0].ID
	qID := model.NewID()
	if err := d.SubmitWith("甲", head, model.ActionEdit, []string{"P", "Q"}, SubmitOpts{
		BaseContent: strPtr(""),
		LineIDs:     []model.ID{qID},
	}); err != nil {
		t.Fatal(err)
	}
	if err := d.SubmitWith("甲", head, model.ActionEdit, []string{"R"}, SubmitOpts{
		BaseContent: strPtr("P"),
	}); err != nil {
		t.Fatal(err)
	}
	after := view(t, d)
	if len(after.Lines) != 2 || after.Lines[0].Content != "R" || after.Lines[1].Content != "Q" || after.Lines[1].ID != qID {
		t.Fatalf("应有 R,Q 且 Q 同 ID: %+v", after.Lines)
	}
	live := d.live[claimKey{head, model.ActionEdit}]
	if live == nil || !sameStrings(live.content, []string{"R", "Q"}) || !sameIDs(live.ids, []model.ID{qID}) {
		t.Fatalf("live 仍含尾 Q: %+v", live)
	}
}

// TestUpdateLiveSpanHeadEditKeepsTail：跨度 [X,Y] 后普通头改 P，保留 Y 同 ID。
func TestUpdateLiveSpanHeadEditKeepsTail(t *testing.T) {
	d, l1, l2, l3 := setupSpanDoc(t)
	yID := model.NewID()
	if err := d.SubmitSpanEdit("甲", SpanEditOpts{
		BaseIDs:     []model.ID{l1, l2},
		BaseTexts:   []string{"L1", "L2"},
		AfterSeen:   l3,
		Replacement: []string{"X", "Y"},
		LineIDs:     []model.ID{yID},
	}); err != nil {
		t.Fatal(err)
	}
	if err := d.SubmitWith("甲", l1, model.ActionEdit, []string{"P"}, SubmitOpts{
		BaseContent: strPtr("X"),
	}); err != nil {
		t.Fatal(err)
	}
	after := view(t, d)
	if len(after.Lines) != 3 || after.Lines[0].Content != "P" || after.Lines[1].Content != "Y" || after.Lines[1].ID != yID || after.Lines[2].Content != "L3" {
		t.Fatalf("应有 P,Y,L3 且 Y 同 ID: %+v", after.Lines)
	}
	eo := after.Lines[0].EditOrigin
	if eo == nil || !sameStrings(eo.Content, []string{"P", "Y"}) || !sameIDs(eo.LineIDs, []model.ID{l1, yID}) {
		t.Fatalf("EditOrigin 应反映 P,Y: %+v", eo)
	}
	if !sameIDs(eo.BaseIDs, []model.ID{l1, l2}) {
		t.Fatalf("原跨度基准应保留: %+v", eo)
	}
}

// TestUpdateLiveSpanHeadPasteKeepsTail：跨度 [X,Y] 后普通头粘贴 [P,Q]，结果 [P,Q,Y] 且 Y 同 ID。
func TestUpdateLiveSpanHeadPasteKeepsTail(t *testing.T) {
	d, l1, l2, l3 := setupSpanDoc(t)
	yID := model.NewID()
	if err := d.SubmitSpanEdit("甲", SpanEditOpts{
		BaseIDs:     []model.ID{l1, l2},
		BaseTexts:   []string{"L1", "L2"},
		AfterSeen:   l3,
		Replacement: []string{"X", "Y"},
		LineIDs:     []model.ID{yID},
	}); err != nil {
		t.Fatal(err)
	}
	qID := model.NewID()
	if err := d.SubmitWith("甲", l1, model.ActionEdit, []string{"P", "Q"}, SubmitOpts{
		BaseContent: strPtr("X"),
		LineIDs:     []model.ID{qID},
	}); err != nil {
		t.Fatal(err)
	}
	after := view(t, d)
	if len(after.Lines) != 4 || after.Lines[0].Content != "P" || after.Lines[1].Content != "Q" || after.Lines[1].ID != qID || after.Lines[2].Content != "Y" || after.Lines[2].ID != yID || after.Lines[3].Content != "L3" {
		t.Fatalf("应有 P,Q,Y,L3: %+v", after.Lines)
	}
	eo := after.Lines[0].EditOrigin
	if eo == nil || !sameStrings(eo.Content, []string{"P", "Q", "Y"}) || !sameIDs(eo.LineIDs, []model.ID{l1, qID, yID}) {
		t.Fatalf("EditOrigin 应反映 P,Q,Y: %+v", eo)
	}
	live := d.live[claimKey{l1, model.ActionEdit}]
	if live == nil || !sameStrings(live.content, []string{"P", "Q", "Y"}) || !sameIDs(live.ids, []model.ID{qID, yID}) {
		t.Fatalf("live 应含 Q 与原 Y: %+v", live)
	}
	loaded := roundTripLoad(t, d)
	lv := view(t, loaded)
	if len(lv.Lines) != 4 || lv.Lines[1].ID != qID || lv.Lines[2].ID != yID {
		t.Fatalf("Load 后 ID 仍在: %+v", lv.Lines)
	}
}

// TestUpdateLiveEditSingleKeepsNoTail：普通单行续写只改头，不制造尾段。
func TestUpdateLiveEditSingleKeepsNoTail(t *testing.T) {
	d := New("t")
	if err := d.EnsureLines(1); err != nil {
		t.Fatal(err)
	}
	head := view(t, d).Lines[0].ID
	if err := d.SubmitWith("甲", head, model.ActionEdit, []string{"A"}, SubmitOpts{BaseContent: strPtr("")}); err != nil {
		t.Fatal(err)
	}
	if err := d.SubmitWith("甲", head, model.ActionEdit, []string{"B"}, SubmitOpts{BaseContent: strPtr("A")}); err != nil {
		t.Fatal(err)
	}
	after := view(t, d)
	if len(after.Lines) != 1 || after.Lines[0].Content != "B" {
		t.Fatalf("单行只改头: %+v", after.Lines)
	}
}

// TestUpdateLiveEditPasteForeignInnerRejects：WholeClaim 整段更新时段内他人改动不得吞掉。
func TestUpdateLiveEditPasteForeignInnerRejects(t *testing.T) {
	d := New("t")
	if err := d.EnsureLines(1); err != nil {
		t.Fatal(err)
	}
	head := view(t, d).Lines[0].ID
	qID := model.NewID()
	if err := d.SubmitWith("甲", head, model.ActionEdit, []string{"P", "Q"}, SubmitOpts{
		BaseContent: strPtr(""),
		LineIDs:     []model.ID{qID},
	}); err != nil {
		t.Fatal(err)
	}
	if err := d.SubmitWith("乙", qID, model.ActionEdit, []string{"Q2"}, SubmitOpts{BaseContent: strPtr("Q")}); err != nil {
		t.Fatal(err)
	}
	before := view(t, d)
	err := d.SubmitWith("甲", head, model.ActionEdit, []string{"R", "S"}, SubmitOpts{
		BaseContent: strPtr("P"),
		LineIDs:     []model.ID{model.NewID()},
		WholeClaim:  true,
	})
	if err != ErrPromoteUnsafe && err != ErrSpanBlocked {
		t.Fatalf("他人内行改动应拒: %v", err)
	}
	after := view(t, d)
	if len(after.Lines) != len(before.Lines) || after.Lines[0].Content != "P" || after.Lines[1].Content != "Q2" {
		t.Fatalf("拒绝须零变化: before=%+v after=%+v", before.Lines, after.Lines)
	}
	if d.live[claimKey{qID, model.ActionEdit}] == nil {
		t.Fatal("乙的内行 live 须保留")
	}
}

// span 缩成单行后再普通粘贴成 [P,Q]，供「选中完整结果段再跨度」回归共用。
func setupSpanThenPastePQ(t *testing.T) (d *Doc, l1, l2, l3, qID model.ID) {
	t.Helper()
	d, l1, l2, l3 = setupSpanDoc(t)
	if err := d.SubmitSpanEdit("甲", SpanEditOpts{
		BaseIDs:     []model.ID{l1, l2},
		BaseTexts:   []string{"L1", "L2"},
		AfterSeen:   l3,
		Replacement: []string{"X"},
	}); err != nil {
		t.Fatal(err)
	}
	qID = model.NewID()
	if err := d.SubmitWith("甲", l1, model.ActionEdit, []string{"P", "Q"}, SubmitOpts{
		BaseContent: strPtr("X"),
		LineIDs:     []model.ID{qID},
	}); err != nil {
		t.Fatal(err)
	}
	return d, l1, l2, l3, qID
}

// TestSpanEditOwnSyncedResultSegment：已同步（View/Load）后选中本人完整结果段再跨度，须成功且保留最初来源。
func TestSpanEditOwnSyncedResultSegment(t *testing.T) {
	d0, l1, l2, l3, qID := setupSpanThenPastePQ(t)
	d := roundTripLoad(t, d0)
	rID := model.NewID()
	if err := d.SubmitSpanEdit("甲", SpanEditOpts{
		BaseIDs:     []model.ID{l1, qID},
		BaseTexts:   []string{"P", "Q"},
		AfterSeen:   l3,
		Replacement: []string{"R", "S"},
		LineIDs:     []model.ID{rID},
	}); err != nil {
		t.Fatalf("选中完整结果应成功: %v", err)
	}
	after := view(t, d)
	if len(after.Lines) != 3 || after.Lines[0].Content != "R" || after.Lines[1].Content != "S" || after.Lines[2].Content != "L3" {
		t.Fatalf("应换成 R,S,L3: %+v", after.Lines)
	}
	if after.Lines[0].ID != l1 || after.Lines[1].ID != rID {
		t.Fatalf("首行稳定、新行用预生 ID: %+v", after.Lines)
	}
	eo := after.Lines[0].EditOrigin
	if eo == nil || !sameIDs(eo.BaseIDs, []model.ID{l1, l2}) || !sameStrings(eo.BaseTexts, []string{"L1", "L2"}) {
		t.Fatalf("须保留最初跨度来源: %+v", eo)
	}
	if !sameStrings(eo.Content, []string{"R", "S"}) {
		t.Fatalf("EditOrigin 内容应为 R,S: %+v", eo)
	}
	live := d.live[claimKey{l1, model.ActionEdit}]
	if live == nil || !sameIDs(live.baseIDs, []model.ID{l1, l2}) || !sameStrings(live.baseTexts, []string{"L1", "L2"}) {
		t.Fatalf("live 须保留旧基准: %+v", live)
	}
	if !sameStrings(live.content, []string{"R", "S"}) || !sameIDs(live.ids, []model.ID{rID}) {
		t.Fatalf("live 应更新为 R,S: %+v", live)
	}
	if len(after.Disputes) != 0 {
		t.Fatalf("不应新开争议: %+v", after.Disputes)
	}

	loaded := roundTripLoad(t, d)
	llive := loaded.live[claimKey{l1, model.ActionEdit}]
	if llive == nil || !sameIDs(llive.baseIDs, []model.ID{l1, l2}) || !sameStrings(llive.content, []string{"R", "S"}) {
		t.Fatalf("Load 后仍保留来源与结果: %+v", llive)
	}
	lv := view(t, loaded)
	if lv.Lines[0].EditOrigin == nil || !sameIDs(lv.Lines[0].EditOrigin.BaseIDs, []model.ID{l1, l2}) {
		t.Fatalf("Load 后 EditOrigin: %+v", lv.Lines[0].EditOrigin)
	}
}

// TestSpanEditOwnSyncedResultSegmentRedo：完整结果跨度后 Undo 回结果再 Redo 式重提，须仍成功。
func TestSpanEditOwnSyncedResultSegmentRedo(t *testing.T) {
	d, l1, l2, l3, qID := setupSpanThenPastePQ(t)
	if err := d.SubmitSpanEdit("甲", SpanEditOpts{
		BaseIDs:     []model.ID{l1, qID},
		BaseTexts:   []string{"P", "Q"},
		AfterSeen:   l3,
		Replacement: []string{"R"},
	}); err != nil {
		t.Fatal(err)
	}
	// Undo：原基准续写回 [P,Q]（选区 Undo 恢复正文）。
	q2 := model.NewID()
	if err := d.SubmitSpanEdit("甲", SpanEditOpts{
		BaseIDs:     []model.ID{l1, l2},
		BaseTexts:   []string{"L1", "L2"},
		AfterSeen:   l3,
		Replacement: []string{"P", "Q"},
		LineIDs:     []model.ID{q2},
	}); err != nil {
		t.Fatalf("Undo 回 P,Q: %v", err)
	}
	afterUndo := view(t, d)
	if len(afterUndo.Lines) != 3 || afterUndo.Lines[0].Content != "P" || afterUndo.Lines[1].Content != "Q" {
		t.Fatalf("Undo 后应有 P,Q: %+v", afterUndo.Lines)
	}
	qNow := afterUndo.Lines[1].ID
	// Redo：再以当前完整结果为基准跨到 R。
	if err := d.SubmitSpanEdit("甲", SpanEditOpts{
		BaseIDs:     []model.ID{l1, qNow},
		BaseTexts:   []string{"P", "Q"},
		AfterSeen:   l3,
		Replacement: []string{"R"},
	}); err != nil {
		t.Fatalf("Redo 式完整结果跨度应成功: %v", err)
	}
	after := view(t, d)
	if len(after.Lines) != 2 || after.Lines[0].Content != "R" || after.Lines[1].Content != "L3" {
		t.Fatalf("Redo 后应有 R,L3: %+v", after.Lines)
	}
	live := d.live[claimKey{l1, model.ActionEdit}]
	if live == nil || !sameIDs(live.baseIDs, []model.ID{l1, l2}) {
		t.Fatalf("Redo 后仍保留最初来源: %+v", live)
	}
}

// TestSpanEditOwnSyncedResultAfterHeadEdit：改首行保留尾后，选中完整结果再跨度。
func TestSpanEditOwnSyncedResultAfterHeadEdit(t *testing.T) {
	d, l1, l2, l3 := setupSpanDoc(t)
	yID := model.NewID()
	if err := d.SubmitSpanEdit("甲", SpanEditOpts{
		BaseIDs:     []model.ID{l1, l2},
		BaseTexts:   []string{"L1", "L2"},
		AfterSeen:   l3,
		Replacement: []string{"X", "Y"},
		LineIDs:     []model.ID{yID},
	}); err != nil {
		t.Fatal(err)
	}
	if err := d.SubmitWith("甲", l1, model.ActionEdit, []string{"P"}, SubmitOpts{
		BaseContent: strPtr("X"),
	}); err != nil {
		t.Fatal(err)
	}
	afterHead := view(t, d)
	if len(afterHead.Lines) != 3 || afterHead.Lines[0].Content != "P" || afterHead.Lines[1].Content != "Y" {
		t.Fatalf("头改后应有 P,Y: %+v", afterHead.Lines)
	}
	rID := model.NewID()
	if err := d.SubmitSpanEdit("甲", SpanEditOpts{
		BaseIDs:     []model.ID{l1, yID},
		BaseTexts:   []string{"P", "Y"},
		AfterSeen:   l3,
		Replacement: []string{"R", "S"},
		LineIDs:     []model.ID{rID},
	}); err != nil {
		t.Fatalf("改首行后完整结果跨度应成功: %v", err)
	}
	after := view(t, d)
	if len(after.Lines) != 3 || after.Lines[0].Content != "R" || after.Lines[1].Content != "S" {
		t.Fatalf("应换成 R,S: %+v", after.Lines)
	}
	eo := after.Lines[0].EditOrigin
	if eo == nil || !sameIDs(eo.BaseIDs, []model.ID{l1, l2}) || !sameStrings(eo.BaseTexts, []string{"L1", "L2"}) {
		t.Fatalf("须保留最初来源: %+v", eo)
	}
}

// TestSpanEditOwnSyncedResultPartialRejects：只选结果段一部分，须拒绝且零变化。
func TestSpanEditOwnSyncedResultPartialRejects(t *testing.T) {
	d, l1, l2, l3 := setupSpanDoc(t)
	yID, zID := model.NewID(), model.NewID()
	if err := d.SubmitSpanEdit("甲", SpanEditOpts{
		BaseIDs:     []model.ID{l1, l2},
		BaseTexts:   []string{"L1", "L2"},
		AfterSeen:   l3,
		Replacement: []string{"X", "Y", "Z"},
		LineIDs:     []model.ID{yID, zID},
	}); err != nil {
		t.Fatal(err)
	}
	before := view(t, d)
	err := d.SubmitSpanEdit("甲", SpanEditOpts{
		BaseIDs:     []model.ID{l1, yID},
		BaseTexts:   []string{"X", "Y"},
		AfterSeen:   zID,
		Replacement: []string{"R"},
	})
	if err != ErrStaleEdit {
		t.Fatalf("部分结果段应 ErrStaleEdit: %v", err)
	}
	after := view(t, d)
	if len(after.Lines) != len(before.Lines) || after.Lines[0].Content != "X" || after.Lines[1].Content != "Y" || after.Lines[2].Content != "Z" {
		t.Fatalf("拒绝须零变化: before=%+v after=%+v", before.Lines, after.Lines)
	}
	live := d.live[claimKey{l1, model.ActionEdit}]
	if live == nil || !sameStrings(live.content, []string{"X", "Y", "Z"}) {
		t.Fatalf("live 须保留: %+v", live)
	}
	_ = l2
}

// TestSpanEditOwnSyncedResultForeignInnerRejects：结果段含他人改动时不得吞掉。
func TestSpanEditOwnSyncedResultForeignInnerRejects(t *testing.T) {
	d, l1, l2, l3, qID := setupSpanThenPastePQ(t)
	if err := d.SubmitWith("乙", qID, model.ActionEdit, []string{"Q2"}, SubmitOpts{BaseContent: strPtr("Q")}); err != nil {
		t.Fatal(err)
	}
	before := view(t, d)
	err := d.SubmitSpanEdit("甲", SpanEditOpts{
		BaseIDs:     []model.ID{l1, qID},
		BaseTexts:   []string{"P", "Q2"},
		AfterSeen:   l3,
		Replacement: []string{"R"},
	})
	if err != ErrSpanBlocked && err != ErrStaleEdit {
		t.Fatalf("他人内行改动应拒: %v", err)
	}
	after := view(t, d)
	if len(after.Lines) != len(before.Lines) || after.Lines[0].Content != "P" || after.Lines[1].Content != "Q2" {
		t.Fatalf("拒绝须零变化: before=%+v after=%+v", before.Lines, after.Lines)
	}
	if d.live[claimKey{qID, model.ActionEdit}] == nil {
		t.Fatal("乙的内行 live 须保留")
	}
	live := d.live[claimKey{l1, model.ActionEdit}]
	if live == nil || !sameIDs(live.baseIDs, []model.ID{l1, l2}) {
		t.Fatalf("甲跨度 live 须保留: %+v", live)
	}
}
