package document

import (
	"slices"
	"testing"

	"github.com/KDreamelf/collaborative-editor/internal/model"
)

func TestBeforeInsertFirstLine(t *testing.T) {
	d := New("t")
	head := view(t, d).Lines[0].ID
	b1, b2 := model.NewID(), model.NewID()
	zero := model.ID{}
	if err := d.SubmitWith("甲", head, model.ActionInsertBefore, []string{"B1", "B2"}, SubmitOpts{
		BeforeSeen: &zero,
		LineIDs:    []model.ID{b1, b2},
	}); err != nil {
		t.Fatal(err)
	}
	after := view(t, d)
	if len(after.Lines) != 3 || after.Lines[0].ID != b1 || after.Lines[1].ID != b2 || after.Lines[2].ID != head {
		t.Fatalf("文首前插应 [B1,B2,原首行]: %+v", after.Lines)
	}
	if after.Lines[2].ID != head {
		t.Fatalf("原行 ID 不变")
	}
	o := after.Lines[0].InsertOrigin
	if o == nil || o.Action != model.ActionInsertBefore || o.Anchor != head || !o.OldPrev.IsZero() {
		t.Fatalf("InsertOrigin 前插文首: %+v", o)
	}
}

func TestBeforeInsertMidKeepsAnchorID(t *testing.T) {
	d := New("t")
	if err := d.EnsureLines(3); err != nil {
		t.Fatal(err)
	}
	v := view(t, d)
	l1, l2, l3 := v.Lines[0].ID, v.Lines[1].ID, v.Lines[2].ID
	x := model.NewID()
	if err := d.SubmitWith("甲", l2, model.ActionInsertBefore, []string{"X"}, SubmitOpts{
		BeforeSeen: idPtr(l1),
		LineIDs:    []model.ID{x},
	}); err != nil {
		t.Fatal(err)
	}
	after := view(t, d)
	if len(after.Lines) != 4 || after.Lines[0].ID != l1 || after.Lines[1].ID != x || after.Lines[2].ID != l2 || after.Lines[3].ID != l3 {
		t.Fatalf("中段前插保原 ID: %+v", after.Lines)
	}
	if after.Disputes != nil && len(after.Disputes) != 0 {
		t.Fatalf("同步前插无争议: %+v", after.Disputes)
	}
}

func TestBeforeInsertSyncedStack(t *testing.T) {
	d := New("t")
	if err := d.EnsureLines(2); err != nil {
		t.Fatal(err)
	}
	v := view(t, d)
	prev, anchor := v.Lines[0].ID, v.Lines[1].ID
	a := model.NewID()
	if err := d.SubmitWith("甲", anchor, model.ActionInsertBefore, []string{"A"}, SubmitOpts{
		BeforeSeen: idPtr(prev),
		LineIDs:    []model.ID{a},
	}); err != nil {
		t.Fatal(err)
	}
	b := model.NewID()
	if err := d.SubmitWith("丙", anchor, model.ActionInsertBefore, []string{"B"}, SubmitOpts{
		BeforeSeen: idPtr(a),
		LineIDs:    []model.ID{b},
	}); err != nil {
		t.Fatal(err)
	}
	after := view(t, d)
	got := textsOf(after.Lines)
	// [prev, A, B, anchor] — 新段更靠近锚点
	if len(got) != 4 || got[1] != "A" || got[2] != "B" {
		t.Fatalf("同步堆叠应为 [原前驱,A,B,anchor]: %v lines=%+v", got, after.Lines)
	}
	if after.Lines[0].ID != prev || after.Lines[3].ID != anchor {
		t.Fatalf("两端保留: %+v", after.Lines)
	}
	if after.Lines[1].ID != a || after.Lines[2].ID != b {
		t.Fatalf("预生 ID: %+v", after.Lines)
	}
}

func TestBeforeInsertUnsyncedDispute(t *testing.T) {
	d := New("t")
	if err := d.EnsureLines(2); err != nil {
		t.Fatal(err)
	}
	v := view(t, d)
	prev, anchor := v.Lines[0].ID, v.Lines[1].ID
	if err := d.SubmitWith("甲", anchor, model.ActionInsertBefore, []string{"A"}, SubmitOpts{
		BeforeSeen: idPtr(prev),
	}); err != nil {
		t.Fatal(err)
	}
	if err := d.SubmitWith("乙", anchor, model.ActionInsertBefore, []string{"B"}, SubmitOpts{
		BeforeSeen: idPtr(prev),
	}); err != nil {
		t.Fatal(err)
	}
	after := view(t, d)
	if len(after.Lines) != 2 || after.Lines[1].ID != anchor || after.Lines[1].Prev != prev {
		t.Fatalf("未同步应收回且锚点保留: %+v", after.Lines)
	}
	if len(after.Disputes) != 2 {
		t.Fatalf("两候选: %+v", after.Disputes)
	}
	for _, item := range after.Disputes {
		if item.RealLine != anchor || item.Action != model.ActionInsertBefore {
			t.Fatalf("争议锚下方原行+插在前面: %+v", item)
		}
		if len(item.Content) != 1 {
			t.Fatalf("Content 仅插入段: %+v", item)
		}
	}
}

func TestBeforeInsertViewLoadOldPacketConflict(t *testing.T) {
	d := New("t")
	if err := d.EnsureLines(2); err != nil {
		t.Fatal(err)
	}
	v := view(t, d)
	prev, anchor := v.Lines[0].ID, v.Lines[1].ID
	if err := d.SubmitWith("甲", anchor, model.ActionInsertBefore, []string{"A"}, SubmitOpts{
		BeforeSeen: idPtr(prev),
	}); err != nil {
		t.Fatal(err)
	}
	if err := d.SubmitWith("乙", anchor, model.ActionInsertBefore, []string{"B"}, SubmitOpts{
		BeforeSeen: idPtr(prev),
	}); err != nil {
		t.Fatal(err)
	}
	mid := view(t, d)
	if len(mid.Disputes) != 2 {
		t.Fatalf("前置两争议: %+v", mid.Disputes)
	}
	loaded := roundTripLoad(t, d)
	after := view(t, loaded)
	if len(after.Disputes) != 2 {
		t.Fatalf("View→Load 争议保留: %+v", after.Disputes)
	}
	jia, _ := byPerson(after, "甲")
	yi, _ := byPerson(after, "乙")
	if jia.Action != model.ActionInsertBefore || yi.Action != model.ActionInsertBefore {
		t.Fatalf("做法: jia=%v yi=%v", jia.Action, yi.Action)
	}
	if jia.RealLine != anchor || yi.RealLine != anchor {
		t.Fatalf("真实行=锚点: jia=%v yi=%v", jia.RealLine, yi.RealLine)
	}
	// Load 后继续旧基准冲突：再来者仍看原前驱
	if err := loaded.SubmitWith("丙", anchor, model.ActionInsertBefore, []string{"C"}, SubmitOpts{
		BeforeSeen: idPtr(prev),
	}); err != nil {
		t.Fatal(err)
	}
	again := view(t, loaded)
	if len(again.Disputes) != 3 {
		t.Fatalf("Load 后旧包续冲突三候选: %+v", again.Disputes)
	}
}

func TestBeforeInsertFollowResolve(t *testing.T) {
	d := New("t")
	if err := d.EnsureLines(2); err != nil {
		t.Fatal(err)
	}
	v := view(t, d)
	prev, anchor := v.Lines[0].ID, v.Lines[1].ID
	if err := d.SubmitWith("甲", anchor, model.ActionInsertBefore, []string{"A"}, SubmitOpts{
		BeforeSeen: idPtr(prev),
	}); err != nil {
		t.Fatal(err)
	}
	if err := d.SubmitWith("乙", anchor, model.ActionInsertBefore, []string{"B"}, SubmitOpts{
		BeforeSeen: idPtr(prev),
	}); err != nil {
		t.Fatal(err)
	}
	mid := view(t, d)
	jia, _ := byPerson(mid, "甲")
	if _, err := d.RequestFollow("乙", jia.ID, 1); err != nil {
		t.Fatal(err)
	}
	if _, err := d.AnswerFollow("甲", "乙", jia.ID, true); err != nil {
		t.Fatal(err)
	}
	after := view(t, d)
	if len(after.Disputes) != 0 {
		t.Fatalf("追随收口无争议: %+v", after.Disputes)
	}
	got := textsOf(after.Lines)
	if len(got) != 3 || got[0] != "" || got[1] != "A" || after.Lines[2].ID != anchor {
		t.Fatalf("胜出拼在锚前: %v %+v", got, after.Lines)
	}
	o := after.Lines[1].InsertOrigin
	if o == nil || o.Action != model.ActionInsertBefore || o.Anchor != anchor {
		t.Fatalf("胜出出处: %+v", o)
	}
}

func TestBeforeInsertPromoteChildEdit(t *testing.T) {
	d := New("t")
	if err := d.EnsureLines(2); err != nil {
		t.Fatal(err)
	}
	v := view(t, d)
	prev, anchor := v.Lines[0].ID, v.Lines[1].ID
	a1, a2 := model.NewID(), model.NewID()
	if err := d.SubmitWith("甲", anchor, model.ActionInsertBefore, []string{"A1", "A2"}, SubmitOpts{
		BeforeSeen: idPtr(prev),
		LineIDs:    []model.ID{a1, a2},
	}); err != nil {
		t.Fatal(err)
	}
	if err := d.Submit("乙", a2, model.ActionEdit, []string{"A2改"}); err != nil {
		t.Fatal(err)
	}
	if err := d.SubmitWith("丙", anchor, model.ActionInsertBefore, []string{"C1"}, SubmitOpts{
		BeforeSeen: idPtr(prev),
	}); err != nil {
		t.Fatal(err)
	}
	after := view(t, d)
	if len(after.Lines) != 2 || after.Lines[1].ID != anchor {
		t.Fatalf("应收回: %+v", after.Lines)
	}
	if len(after.Disputes) != 3 {
		t.Fatalf("甲乙丙三份: %+v", after.Disputes)
	}
	jia, _ := byPerson(after, "甲")
	yi, _ := byPerson(after, "乙")
	bing, _ := byPerson(after, "丙")
	if jia.Action != model.ActionInsertBefore || yi.Action != model.ActionInsertBefore || bing.Action != model.ActionInsertBefore {
		t.Fatalf("均插在前面: %+v", after.Disputes)
	}
	if len(yi.Content) != 2 || yi.Content[0] != "A1" || yi.Content[1] != "A2改" {
		t.Fatalf("乙整段含改字: %+v", yi.Content)
	}
}

func TestBeforeAndAfterInsertCoexist(t *testing.T) {
	d := New("t")
	if err := d.EnsureLines(2); err != nil {
		t.Fatal(err)
	}
	v := view(t, d)
	l1, l2 := v.Lines[0].ID, v.Lines[1].ID
	afterID := model.NewID()
	if err := d.SubmitWith("甲", l1, model.ActionInsert, []string{"AFTER"}, SubmitOpts{
		AfterSeen: idPtr(l2),
		LineIDs:   []model.ID{afterID},
	}); err != nil {
		t.Fatal(err)
	}
	beforeID := model.NewID()
	if err := d.SubmitWith("乙", l2, model.ActionInsertBefore, []string{"BEFORE"}, SubmitOpts{
		BeforeSeen: idPtr(afterID),
		LineIDs:    []model.ID{beforeID},
	}); err != nil {
		t.Fatal(err)
	}
	after := view(t, d)
	got := textsOf(after.Lines)
	// [l1, AFTER, BEFORE, l2]
	if len(got) != 4 || got[1] != "AFTER" || got[2] != "BEFORE" {
		t.Fatalf("上下行不同锚方向并存: %v", got)
	}
	if after.Lines[1].InsertOrigin == nil || model.InsertAction(after.Lines[1].InsertOrigin.Action) != model.ActionInsert {
		t.Fatalf("AFTER 出处方向: %+v", after.Lines[1].InsertOrigin)
	}
	if after.Lines[2].InsertOrigin == nil || after.Lines[2].InsertOrigin.Action != model.ActionInsertBefore {
		t.Fatalf("BEFORE 出处方向: %+v", after.Lines[2].InsertOrigin)
	}
}

// 先后插再前插：A.last.Next 变成 B≠OldNext=L2，Load 须仍恢复两段。
func TestBeforeAndAfterInsertCoexistLoadAfterThenBefore(t *testing.T) {
	d := New("t")
	if err := d.EnsureLines(2); err != nil {
		t.Fatal(err)
	}
	v := view(t, d)
	l1, l2 := v.Lines[0].ID, v.Lines[1].ID
	afterID, beforeID := model.NewID(), model.NewID()
	if err := d.SubmitWith("甲", l1, model.ActionInsert, []string{"AFTER"}, SubmitOpts{
		AfterSeen: idPtr(l2),
		LineIDs:   []model.ID{afterID},
	}); err != nil {
		t.Fatal(err)
	}
	if err := d.SubmitWith("乙", l2, model.ActionInsertBefore, []string{"BEFORE"}, SubmitOpts{
		BeforeSeen: idPtr(afterID),
		LineIDs:    []model.ID{beforeID},
	}); err != nil {
		t.Fatal(err)
	}
	assertCoexistLoadAndEdit(t, d, l1, l2, afterID, beforeID)
}

// 先前插再后插：B.first.Prev 变成 A≠OldPrev，Load 须仍恢复两段。
func TestBeforeAndAfterInsertCoexistLoadBeforeThenAfter(t *testing.T) {
	d := New("t")
	if err := d.EnsureLines(2); err != nil {
		t.Fatal(err)
	}
	v := view(t, d)
	l1, l2 := v.Lines[0].ID, v.Lines[1].ID
	beforeID, afterID := model.NewID(), model.NewID()
	if err := d.SubmitWith("乙", l2, model.ActionInsertBefore, []string{"BEFORE"}, SubmitOpts{
		BeforeSeen: idPtr(l1),
		LineIDs:    []model.ID{beforeID},
	}); err != nil {
		t.Fatal(err)
	}
	if err := d.SubmitWith("甲", l1, model.ActionInsert, []string{"AFTER"}, SubmitOpts{
		AfterSeen: idPtr(beforeID),
		LineIDs:   []model.ID{afterID},
	}); err != nil {
		t.Fatal(err)
	}
	assertCoexistLoadAndEdit(t, d, l1, l2, afterID, beforeID)
}

func assertCoexistLoadAndEdit(t *testing.T, d *Doc, l1, l2, afterID, beforeID model.ID) {
	t.Helper()
	loaded := roundTripLoad(t, d)
	after := view(t, loaded)
	got := textsOf(after.Lines)
	if len(got) != 4 || got[1] != "AFTER" || got[2] != "BEFORE" {
		t.Fatalf("View→Load 链: %v", got)
	}
	if after.Lines[0].ID != l1 || after.Lines[3].ID != l2 {
		t.Fatalf("锚点 ID: %+v", after.Lines)
	}
	ao := after.Lines[1].InsertOrigin
	if after.Lines[1].ID != afterID || ao == nil ||
		model.InsertAction(ao.Action) != model.ActionInsert || ao.OldNext.IsZero() {
		t.Fatalf("AFTER 出处: %+v", ao)
	}
	// OldNext：先后插= L2；先前插= beforeID（提交时所见后继）。
	if ao.OldNext != l2 && ao.OldNext != beforeID {
		t.Fatalf("AFTER OldNext 应为历史后继: %+v", ao)
	}
	bo := after.Lines[2].InsertOrigin
	if after.Lines[2].ID != beforeID || bo == nil || bo.Action != model.ActionInsertBefore {
		t.Fatalf("BEFORE 出处: %+v", bo)
	}
	// OldPrev：先后插= afterID；先前插= l1。
	if bo.OldPrev != afterID && bo.OldPrev != l1 {
		t.Fatalf("BEFORE OldPrev 应为原前驱: %+v", bo)
	}

	if err := loaded.Submit("甲", afterID, model.ActionEdit, []string{"AFTER改"}); err != nil {
		t.Fatal(err)
	}
	mid := view(t, loaded)
	if textsOf(mid.Lines)[2] != "BEFORE" || mid.Lines[2].ID != beforeID {
		t.Fatalf("改 AFTER 后 BEFORE 仍在: %v", textsOf(mid.Lines))
	}
	if err := loaded.Submit("乙", beforeID, model.ActionEdit, []string{"BEFORE改"}); err != nil {
		t.Fatal(err)
	}
	end := view(t, loaded)
	got = textsOf(end.Lines)
	if len(got) != 4 || got[1] != "AFTER改" || got[2] != "BEFORE改" {
		t.Fatalf("两方向续改不丢段: %v", got)
	}

	// 同人同步续写后插：updateLive 只拆自身 ids，BEFORE 仍在。
	newAfter := model.NewID()
	if err := loaded.SubmitWith("甲", l1, model.ActionInsert, []string{"AFTER新"}, SubmitOpts{
		AfterSeen: idPtr(afterID),
		LineIDs:   []model.ID{newAfter},
	}); err != nil {
		t.Fatal(err)
	}
	fin := view(t, loaded)
	got = textsOf(fin.Lines)
	if len(got) != 4 || got[1] != "AFTER新" || got[2] != "BEFORE改" || fin.Lines[2].ID != beforeID {
		t.Fatalf("换后插段前插仍在: %v lines=%+v", got, fin.Lines)
	}
}

// 邻接两方向时 checkSegmentUnlink 不得因 Old* 非紧邻而拒；unlink 只拆本段。
func TestCheckSegmentUnlinkCoexistNeighbors(t *testing.T) {
	d := New("t")
	if err := d.EnsureLines(2); err != nil {
		t.Fatal(err)
	}
	v := view(t, d)
	l1, l2 := v.Lines[0].ID, v.Lines[1].ID
	afterID, beforeID := model.NewID(), model.NewID()
	if err := d.SubmitWith("甲", l1, model.ActionInsert, []string{"AFTER"}, SubmitOpts{
		AfterSeen: idPtr(l2),
		LineIDs:   []model.ID{afterID},
	}); err != nil {
		t.Fatal(err)
	}
	if err := d.SubmitWith("乙", l2, model.ActionInsertBefore, []string{"BEFORE"}, SubmitOpts{
		BeforeSeen: idPtr(afterID),
		LineIDs:    []model.ID{beforeID},
	}); err != nil {
		t.Fatal(err)
	}
	afterLive := d.live[claimKey{l1, model.ActionInsert}]
	beforeLive := d.live[claimKey{l2, model.ActionInsertBefore}]
	if afterLive == nil || beforeLive == nil {
		t.Fatalf("两方向 live: after=%v before=%v", afterLive, beforeLive)
	}
	if err := d.checkSegmentUnlink(l1, afterLive, model.ActionInsert); err != nil {
		t.Fatalf("after 夹 before 仍可 unlink 预检: %v", err)
	}
	if err := d.checkSegmentUnlink(l2, beforeLive, model.ActionInsertBefore); err != nil {
		t.Fatalf("before 夹 after 仍可 unlink 预检: %v", err)
	}
	if err := d.unlinkSegment(afterLive.ids); err != nil {
		t.Fatal(err)
	}
	got := textsOf(view(t, d).Lines)
	if len(got) != 3 || got[1] != "BEFORE" {
		t.Fatalf("只拆 after，before 仍在: %v", got)
	}
}

// 旧快照 AfterSeen=L2：空隙里已有 A(after L1)+B(before L2) 时，C 应与 A 争议且 B 原位保留。
func TestCoexistOppositeForeignKeepsBeforeOnAfterStale(t *testing.T) {
	for _, order := range []string{"afterThenBefore", "beforeThenAfter"} {
		t.Run(order, func(t *testing.T) {
			d, l1, l2, afterID, beforeID := coexistAB(t, order)
			if err := d.SubmitWith("丙", l1, model.ActionInsert, []string{"C"}, SubmitOpts{
				AfterSeen: idPtr(l2),
				LineIDs:   []model.ID{model.NewID()},
			}); err != nil {
				t.Fatalf("C 旧包应开 A/C 争议: %v", err)
			}
			after := view(t, d)
			got := textsOf(after.Lines)
			// 收回 A 后：L1, BEFORE, L2
			if len(got) != 3 || got[1] != "BEFORE" || after.Lines[1].ID != beforeID {
				t.Fatalf("B 必须原位: %v lines=%+v", got, after.Lines)
			}
			bo := after.Lines[1].InsertOrigin
			if bo == nil || bo.Action != model.ActionInsertBefore || bo.Anchor != l2 {
				t.Fatalf("B 出处保留: %+v", bo)
			}
			if len(after.Disputes) != 2 {
				t.Fatalf("A/C 两候选: %+v", after.Disputes)
			}
			jia, _ := byPerson(after, "甲")
			bing, _ := byPerson(after, "丙")
			if jia.Action != model.ActionInsert || bing.Action != model.ActionInsert {
				t.Fatalf("争议方向后插: %+v", after.Disputes)
			}
			if len(jia.Content) != 1 || jia.Content[0] != "AFTER" || len(bing.Content) != 1 || bing.Content[0] != "C" {
				t.Fatalf("A/C 正文: jia=%+v bing=%+v", jia, bing)
			}
			if jia.RealLine != l1 || bing.RealLine != l1 {
				t.Fatalf("争议锚 L1: jia=%v bing=%v", jia.RealLine, bing.RealLine)
			}
			// afterID 已 unlink，不得再占链。
			for _, ln := range after.Lines {
				if ln.ID == afterID {
					t.Fatalf("A 应已收回: %+v", after.Lines)
				}
			}
			// OldPrev 不得再指已删 A；View→Load 后 B 仍在，可续写与追随收口。
			assertCoexistForeignSurvivesLoad(t, d, l1, l2, beforeID, "BEFORE", model.ActionInsertBefore, l2, "甲", "丙")
		})
	}
}

// 镜像：旧 BeforeSeen=L1，C 与 B 争议，A 原位保留。
func TestCoexistOppositeForeignKeepsAfterOnBeforeStale(t *testing.T) {
	for _, order := range []string{"afterThenBefore", "beforeThenAfter"} {
		t.Run(order, func(t *testing.T) {
			d, l1, l2, afterID, beforeID := coexistAB(t, order)
			if err := d.SubmitWith("丙", l2, model.ActionInsertBefore, []string{"C"}, SubmitOpts{
				BeforeSeen: idPtr(l1),
				LineIDs:    []model.ID{model.NewID()},
			}); err != nil {
				t.Fatalf("C 旧包应开 B/C 争议: %v", err)
			}
			after := view(t, d)
			got := textsOf(after.Lines)
			// 收回 B 后：L1, AFTER, L2
			if len(got) != 3 || got[1] != "AFTER" || after.Lines[1].ID != afterID {
				t.Fatalf("A 必须原位: %v lines=%+v", got, after.Lines)
			}
			ao := after.Lines[1].InsertOrigin
			if ao == nil || model.InsertAction(ao.Action) != model.ActionInsert || ao.Anchor != l1 {
				t.Fatalf("A 出处保留: %+v", ao)
			}
			if len(after.Disputes) != 2 {
				t.Fatalf("B/C 两候选: %+v", after.Disputes)
			}
			yi, _ := byPerson(after, "乙")
			bing, _ := byPerson(after, "丙")
			if yi.Action != model.ActionInsertBefore || bing.Action != model.ActionInsertBefore {
				t.Fatalf("争议方向前插: %+v", after.Disputes)
			}
			if len(yi.Content) != 1 || yi.Content[0] != "BEFORE" || len(bing.Content) != 1 || bing.Content[0] != "C" {
				t.Fatalf("B/C 正文: yi=%+v bing=%+v", yi, bing)
			}
			for _, ln := range after.Lines {
				if ln.ID == beforeID {
					t.Fatalf("B 应已收回: %+v", after.Lines)
				}
			}
			assertCoexistForeignSurvivesLoad(t, d, l1, l2, afterID, "AFTER", model.ActionInsert, l1, "乙", "丙")
		})
	}
}

// 本方向无段、仅对向 foreign：按真实邻接写入，不造单人伪争议。
func TestCoexistOppositeOnlySubmitApplyNoFakeDispute(t *testing.T) {
	t.Run("afterIntoBeforeOnly", func(t *testing.T) {
		d := New("t")
		if err := d.EnsureLines(2); err != nil {
			t.Fatal(err)
		}
		v := view(t, d)
		l1, l2 := v.Lines[0].ID, v.Lines[1].ID
		beforeID := model.NewID()
		if err := d.SubmitWith("乙", l2, model.ActionInsertBefore, []string{"BEFORE"}, SubmitOpts{
			BeforeSeen: idPtr(l1),
			LineIDs:    []model.ID{beforeID},
		}); err != nil {
			t.Fatal(err)
		}
		cID := model.NewID()
		if err := d.SubmitWith("丙", l1, model.ActionInsert, []string{"C"}, SubmitOpts{
			AfterSeen: idPtr(l2),
			LineIDs:   []model.ID{cID},
		}); err != nil {
			t.Fatalf("仅对向时应 submitApply: %v", err)
		}
		after := view(t, d)
		got := textsOf(after.Lines)
		if len(got) != 4 || got[1] != "C" || got[2] != "BEFORE" {
			t.Fatalf("C 贴锚、B 仍在: %v", got)
		}
		if after.Lines[1].ID != cID || after.Lines[2].ID != beforeID {
			t.Fatalf("ID: %+v", after.Lines)
		}
		if len(after.Disputes) != 0 {
			t.Fatalf("无伪争议: %+v", after.Disputes)
		}
	})
	t.Run("beforeIntoAfterOnly", func(t *testing.T) {
		d := New("t")
		if err := d.EnsureLines(2); err != nil {
			t.Fatal(err)
		}
		v := view(t, d)
		l1, l2 := v.Lines[0].ID, v.Lines[1].ID
		afterID := model.NewID()
		if err := d.SubmitWith("甲", l1, model.ActionInsert, []string{"AFTER"}, SubmitOpts{
			AfterSeen: idPtr(l2),
			LineIDs:   []model.ID{afterID},
		}); err != nil {
			t.Fatal(err)
		}
		cID := model.NewID()
		if err := d.SubmitWith("丙", l2, model.ActionInsertBefore, []string{"C"}, SubmitOpts{
			BeforeSeen: idPtr(l1),
			LineIDs:    []model.ID{cID},
		}); err != nil {
			t.Fatalf("仅对向时应 submitApply: %v", err)
		}
		after := view(t, d)
		got := textsOf(after.Lines)
		if len(got) != 4 || got[1] != "AFTER" || got[2] != "C" {
			t.Fatalf("A 仍在、C 贴锚: %v", got)
		}
		if after.Lines[1].ID != afterID || after.Lines[2].ID != cID {
			t.Fatalf("ID: %+v", after.Lines)
		}
		if len(after.Disputes) != 0 {
			t.Fatalf("无伪争议: %+v", after.Disputes)
		}
	})
}

// 空隙含正式第三锚或嵌套他锚插入：仍原子拒绝，正文不变。
func TestCoexistOppositeForeignRejectsUnrelatedOrNested(t *testing.T) {
	t.Run("formalThirdInSpan", func(t *testing.T) {
		d := New("t")
		if err := d.EnsureLines(3); err != nil {
			t.Fatal(err)
		}
		v := view(t, d)
		l1, l2, l3 := v.Lines[0].ID, v.Lines[1].ID, v.Lines[2].ID
		aID := model.NewID()
		if err := d.SubmitWith("甲", l1, model.ActionInsert, []string{"A"}, SubmitOpts{
			AfterSeen: idPtr(l2),
			LineIDs:   []model.ID{aID},
		}); err != nil {
			t.Fatal(err)
		}
		before := textsOf(view(t, d).Lines)
		err := d.SubmitWith("丙", l1, model.ActionInsert, []string{"C"}, SubmitOpts{
			AfterSeen: idPtr(l3),
			LineIDs:   []model.ID{model.NewID()},
		})
		if err != ErrStaleInsert {
			t.Fatalf("跨正式 L2 应拒: %v", err)
		}
		after := view(t, d)
		if got := textsOf(after.Lines); !slices.Equal(got, before) {
			t.Fatalf("正文不变: before=%v after=%v", before, got)
		}
		if len(after.Disputes) != 0 {
			t.Fatalf("无争议: %+v", after.Disputes)
		}
	})
	t.Run("nestedOtherAnchor", func(t *testing.T) {
		d := New("t")
		if err := d.EnsureLines(2); err != nil {
			t.Fatal(err)
		}
		v := view(t, d)
		l1, l2 := v.Lines[0].ID, v.Lines[1].ID
		aID := model.NewID()
		if err := d.SubmitWith("甲", l1, model.ActionInsert, []string{"A"}, SubmitOpts{
			AfterSeen: idPtr(l2),
			LineIDs:   []model.ID{aID},
		}); err != nil {
			t.Fatal(err)
		}
		nID := model.NewID()
		if err := d.SubmitWith("乙", aID, model.ActionInsert, []string{"NEST"}, SubmitOpts{
			AfterSeen: idPtr(l2),
			LineIDs:   []model.ID{nID},
		}); err != nil {
			t.Fatal(err)
		}
		before := textsOf(view(t, d).Lines)
		err := d.SubmitWith("丙", l1, model.ActionInsert, []string{"C"}, SubmitOpts{
			AfterSeen: idPtr(l2),
			LineIDs:   []model.ID{model.NewID()},
		})
		if err != ErrStaleInsert && err != ErrPromoteUnsafe {
			t.Fatalf("嵌套他锚应拒: %v", err)
		}
		after := view(t, d)
		if got := textsOf(after.Lines); !slices.Equal(got, before) {
			t.Fatalf("正文不变: before=%v after=%v", before, got)
		}
	})
}

func coexistAB(t *testing.T, order string) (d *Doc, l1, l2, afterID, beforeID model.ID) {
	t.Helper()
	d = New("t")
	if err := d.EnsureLines(2); err != nil {
		t.Fatal(err)
	}
	v := view(t, d)
	l1, l2 = v.Lines[0].ID, v.Lines[1].ID
	afterID, beforeID = model.NewID(), model.NewID()
	switch order {
	case "afterThenBefore":
		if err := d.SubmitWith("甲", l1, model.ActionInsert, []string{"AFTER"}, SubmitOpts{
			AfterSeen: idPtr(l2),
			LineIDs:   []model.ID{afterID},
		}); err != nil {
			t.Fatal(err)
		}
		if err := d.SubmitWith("乙", l2, model.ActionInsertBefore, []string{"BEFORE"}, SubmitOpts{
			BeforeSeen: idPtr(afterID),
			LineIDs:    []model.ID{beforeID},
		}); err != nil {
			t.Fatal(err)
		}
	case "beforeThenAfter":
		if err := d.SubmitWith("乙", l2, model.ActionInsertBefore, []string{"BEFORE"}, SubmitOpts{
			BeforeSeen: idPtr(l1),
			LineIDs:    []model.ID{beforeID},
		}); err != nil {
			t.Fatal(err)
		}
		if err := d.SubmitWith("甲", l1, model.ActionInsert, []string{"AFTER"}, SubmitOpts{
			AfterSeen: idPtr(beforeID),
			LineIDs:   []model.ID{afterID},
		}); err != nil {
			t.Fatal(err)
		}
	default:
		t.Fatalf("unknown order %q", order)
	}
	return d, l1, l2, afterID, beforeID
}

// 争议收口后 View→Load：保留段出处边界可达；续写与追随收口不丢该段。
// author/other 为争议双方（other 追随 author 收口）。
func assertCoexistForeignSurvivesLoad(t *testing.T, d *Doc, l1, l2, keepID model.ID, keepText, keepAction string, keepAnchor model.ID, author, other string) {
	t.Helper()
	pre := view(t, d)
	keep := lineByID(pre.Lines, keepID)
	if keep == nil || keep.InsertOrigin == nil {
		t.Fatalf("保留段出处: id=%v", keepID)
	}
	o := keep.InsertOrigin
	if model.InsertAction(o.Action) == model.ActionInsertBefore {
		if !o.OldPrev.IsZero() && lineByID(pre.Lines, o.OldPrev) == nil {
			t.Fatalf("OldPrev 悬空: %+v", o)
		}
	} else if !o.OldNext.IsZero() && lineByID(pre.Lines, o.OldNext) == nil {
		t.Fatalf("OldNext 悬空: %+v", o)
	}

	loaded := roundTripLoad(t, d)
	after := view(t, loaded)
	got := textsOf(after.Lines)
	if len(got) != 3 || after.Lines[1].ID != keepID || got[1] != keepText {
		t.Fatalf("Load 后保留段: %v lines=%+v", got, after.Lines)
	}
	lo := after.Lines[1].InsertOrigin
	if lo == nil || model.InsertAction(lo.Action) != model.InsertAction(keepAction) || lo.Anchor != keepAnchor {
		t.Fatalf("Load 后出处: %+v", lo)
	}
	if after.Lines[0].ID != l1 || after.Lines[2].ID != l2 {
		t.Fatalf("Load 锚点: %+v", after.Lines)
	}
	if len(after.Disputes) != 2 {
		t.Fatalf("Load 争议数: %+v", after.Disputes)
	}

	editor := keep.InsertOrigin.Person
	editText := keepText + "改"
	if err := loaded.Submit(editor, keepID, model.ActionEdit, []string{editText}); err != nil {
		t.Fatal(err)
	}
	mid := view(t, loaded)
	if mid.Lines[1].ID != keepID || textsOf(mid.Lines)[1] != editText {
		t.Fatalf("Load 后续写保留段: %v", textsOf(mid.Lines))
	}

	win, ok := byPerson(mid, author)
	if !ok {
		t.Fatalf("%s 候选: %+v", author, mid.Disputes)
	}
	if _, ok := byPerson(mid, other); !ok {
		t.Fatalf("%s 候选: %+v", other, mid.Disputes)
	}
	if _, err := loaded.RequestFollow(other, win.ID, 1); err != nil {
		t.Fatal(err)
	}
	if _, err := loaded.AnswerFollow(author, other, win.ID, true); err != nil {
		t.Fatal(err)
	}
	fin := view(t, loaded)
	if len(fin.Disputes) != 0 {
		t.Fatalf("追随收口无争议: %+v", fin.Disputes)
	}
	found := false
	for _, ln := range fin.Lines {
		if ln.ID == keepID && ln.Content == editText {
			found = true
			if ln.InsertOrigin == nil || model.InsertAction(ln.InsertOrigin.Action) != model.InsertAction(keepAction) {
				t.Fatalf("收口后出处: %+v", ln.InsertOrigin)
			}
		}
	}
	if !found {
		t.Fatalf("追随收口仍保留段: %v lines=%+v", textsOf(fin.Lines), fin.Lines)
	}
}

func lineByID(lines []model.Line, id model.ID) *model.Line {
	for i := range lines {
		if lines[i].ID == id {
			return &lines[i]
		}
	}
	return nil
}

func TestDeleteAnchorDefersBeforeInsert(t *testing.T) {
	d := New("t")
	if err := d.EnsureLines(2); err != nil {
		t.Fatal(err)
	}
	v := view(t, d)
	prev, anchor := v.Lines[0].ID, v.Lines[1].ID
	if err := d.SubmitWith("乙", anchor, model.ActionInsertBefore, []string{"X"}, SubmitOpts{
		BeforeSeen: idPtr(prev),
	}); err != nil {
		t.Fatal(err)
	}
	d.lines[anchor].Content = ""
	if err := d.DeleteIfIdle("丙", anchor, nil); err != nil {
		t.Fatal(err)
	}
	after := view(t, d)
	hasDel := false
	for _, item := range after.Disputes {
		if item.Action == model.ActionDelete && item.RealLine == anchor {
			hasDel = true
		}
	}
	if !hasDel {
		t.Fatalf("应有删这行主张: %+v", after.Disputes)
	}
	if !d.hasLiveInsert(anchor) && !d.hasInsertHistory(anchor) {
		t.Fatalf("前插主张仍在 live/history")
	}
	foundAnchor := false
	for _, ln := range after.Lines {
		if ln.ID == anchor {
			foundAnchor = true
		}
	}
	if !foundAnchor {
		t.Fatalf("有未决前插时锚点不得硬删: %+v", after.Lines)
	}
	got := textsOf(after.Lines)
	if len(got) < 3 || got[1] != "X" {
		t.Fatalf("前插段仍在链上: %v", got)
	}
}

func TestBeforeInsertNotDisguisedAsPaste(t *testing.T) {
	d := New("t")
	if err := d.EnsureLines(1); err != nil {
		t.Fatal(err)
	}
	head := view(t, d).Lines[0].ID
	zero := model.ID{}
	if err := d.SubmitWith("甲", head, model.ActionInsertBefore, []string{"N"}, SubmitOpts{
		BeforeSeen: &zero,
	}); err != nil {
		t.Fatal(err)
	}
	after := view(t, d)
	if after.Lines[len(after.Lines)-1].ID != head {
		t.Fatalf("前插不是改当前行多行 paste，原行仍在尾: %+v", after.Lines)
	}
	if after.Lines[len(after.Lines)-1].Content != "" {
		// New() 首行空内容
		t.Fatalf("原行正文未并入插入段")
	}
}
