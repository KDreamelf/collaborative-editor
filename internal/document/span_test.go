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
