package main

import (
	"slices"
	"testing"

	"github.com/KDreamelf/collaborative-editor/internal/document"
	"github.com/KDreamelf/collaborative-editor/internal/model"
	"github.com/KDreamelf/collaborative-editor/internal/protocol"
)

func insertOp(person, lineHex, action string, content []string, lineIDs []string, after, before *string) protocol.Op {
	return protocol.Op{
		Kind: protocol.TypeSubmit,
		Submit: &protocol.Submit{
			Type:       protocol.TypeSubmit,
			PersonID:   person,
			LineID:     lineHex,
			Action:     action,
			Content:    content,
			LineIDs:    lineIDs,
			AfterSeen:  after,
			BeforeSeen: before,
		},
	}
}

func TestReceiveOrdinaryInsertAfterEnterIntegrates(t *testing.T) {
	anchor := model.NewID()
	newID := model.NewID()
	d := loadDoc(t, []model.Line{{ID: anchor, Content: "锚"}}, nil)

	got, err := receiveOrdinaryInsert(d, "甲", "", false,
		insertOp("乙", anchor.Hex(), model.ActionInsert, []string{"新行"}, []string{newID.Hex()}, strPtr("旧后继"), nil),
		model.NewID())
	if err != nil {
		t.Fatal(err)
	}
	if got != nil {
		t.Fatalf("不应 CC: %+v", got)
	}
	v := mustView(t, d)
	if len(v.Disputes) != 0 {
		t.Fatalf("disputes=%d", len(v.Disputes))
	}
	if len(v.Lines) != 2 || v.Lines[0].ID != anchor || v.Lines[1].ID != newID || v.Lines[1].Content != "新行" {
		t.Fatalf("行尾 Enter 未入链: %+v", v.Lines)
	}
}

func TestReceiveOrdinaryInsertAfterStackTwiceNoDispute(t *testing.T) {
	anchor := model.NewID()
	id1, id2 := model.NewID(), model.NewID()
	d := loadDoc(t, []model.Line{{ID: anchor, Content: "锚"}}, nil)

	if _, err := receiveOrdinaryInsert(d, "甲", "", false,
		insertOp("乙", anchor.Hex(), model.ActionInsert, []string{"先"}, []string{id1.Hex()}, strPtr(""), nil),
		model.NewID()); err != nil {
		t.Fatal(err)
	}
	if _, err := receiveOrdinaryInsert(d, "甲", "", false,
		insertOp("丙", anchor.Hex(), model.ActionInsert, []string{"后"}, []string{id2.Hex()}, strPtr("远端旧"), nil),
		model.NewID()); err != nil {
		t.Fatal(err)
	}
	v := mustView(t, d)
	if len(v.Disputes) != 0 {
		t.Fatalf("堆叠须零争议: %+v", v.Disputes)
	}
	// 同步堆叠新段更靠近锚点：锚, 后, 先
	if len(v.Lines) != 3 || v.Lines[1].Content != "后" || v.Lines[2].Content != "先" {
		t.Fatalf("堆叠序: %+v", v.Lines)
	}
	if v.Lines[1].ID != id2 || v.Lines[2].ID != id1 {
		t.Fatalf("预生 ID: %+v", v.Lines)
	}
}

func TestReceiveOrdinaryInsertBeforeEnterIntegrates(t *testing.T) {
	below := model.NewID()
	newID := model.NewID()
	d := loadDoc(t, []model.Line{{ID: below, Content: "原下行"}}, nil)

	got, err := receiveOrdinaryInsert(d, "甲", "", false,
		insertOp("乙", below.Hex(), model.ActionInsertBefore, []string{"插入"}, []string{newID.Hex()}, nil, strPtr("旧前驱")),
		model.NewID())
	if err != nil {
		t.Fatal(err)
	}
	if got != nil {
		t.Fatalf("不应 CC: %+v", got)
	}
	v := mustView(t, d)
	if len(v.Disputes) != 0 {
		t.Fatalf("disputes=%d", len(v.Disputes))
	}
	if len(v.Lines) != 2 || v.Lines[0].ID != newID || v.Lines[0].Content != "插入" || v.Lines[1].ID != below {
		t.Fatalf("行首 Enter 应挂在下行之前: %+v", v.Lines)
	}
}

func TestReceiveOrdinaryInsertActiveDiffReturnsOwnCC(t *testing.T) {
	anchor := model.NewID()
	claimID := model.NewID()
	d := loadDoc(t, []model.Line{{ID: anchor, Content: "锚"}}, nil)

	got, err := receiveOrdinaryInsert(d, "甲", anchor.Hex(), true,
		insertOp("乙", anchor.Hex(), model.ActionInsert, []string{"对方段"}, []string{model.NewID().Hex()}, strPtr(""), nil),
		claimID)
	if err != nil {
		t.Fatal(err)
	}
	if got != nil {
		t.Fatalf("插入不发主张: %+v", got)
	}
	v := mustView(t, d)
	if len(v.Disputes) != 0 || len(v.Lines) != 2 || v.Lines[1].Content != "对方段" {
		t.Fatalf("应直接插入: %+v disputes=%+v", v.Lines, v.Disputes)
	}
}

func TestReceiveOrdinaryInsertActiveDiffBeforeRealLine(t *testing.T) {
	below := model.NewID()
	claimID := model.NewID()
	d := loadDoc(t, []model.Line{{ID: below, Content: "下行"}}, nil)

	got, err := receiveOrdinaryInsert(d, "甲", below.Hex(), true,
		insertOp("乙", below.Hex(), model.ActionInsertBefore, []string{"对方"}, []string{model.NewID().Hex()}, nil, strPtr("")),
		claimID)
	if err != nil {
		t.Fatal(err)
	}
	if got != nil {
		t.Fatalf("前插不发主张: %+v", got)
	}
	v := mustView(t, d)
	if len(v.Lines) != 2 || v.Lines[0].Content != "对方" || v.Lines[1].ID != below {
		t.Fatalf("应插在下行前: %+v", v.Lines)
	}
}

func TestReceiveOrdinaryInsertActiveMultilineOwnContent(t *testing.T) {
	anchor := model.NewID()
	id1, id2 := model.NewID(), model.NewID()
	d := loadDoc(t, []model.Line{{ID: anchor, Content: "锚"}}, nil)
	after := model.ID{}
	if err := d.SubmitWith("甲", anchor, model.ActionInsert, []string{"我1", "我2"}, document.SubmitOpts{
		AfterSeen: &after,
		LineIDs:   []model.ID{id1, id2},
	}); err != nil {
		t.Fatal(err)
	}
	claimID := model.NewID()

	got, err := receiveOrdinaryInsert(d, "甲", anchor.Hex(), true,
		insertOp("乙", anchor.Hex(), model.ActionInsert, []string{"对方"}, []string{model.NewID().Hex()}, strPtr(""), nil),
		claimID)
	if err != nil {
		t.Fatal(err)
	}
	if got != nil {
		t.Fatalf("插入不发主张: %+v", got)
	}
	found := false
	for _, ln := range mustView(t, d).Lines {
		if ln.Content == "对方" {
			found = true
		}
	}
	if !found {
		t.Fatal("应写入对方插入")
	}
}

func TestReceiveOrdinaryInsertActiveStackedCAContent(t *testing.T) {
	anchor, tail := model.NewID(), model.NewID()
	aID, cID := model.NewID(), model.NewID()
	d := loadDoc(t, []model.Line{
		{ID: anchor, Content: "锚", Next: tail},
		{ID: tail, Content: "尾", Prev: anchor},
	}, nil)
	if err := d.SubmitWith("甲", anchor, model.ActionInsert, []string{"A"}, document.SubmitOpts{
		AfterSeen: &tail,
		LineIDs:   []model.ID{aID},
	}); err != nil {
		t.Fatal(err)
	}
	if err := d.SubmitWith("丙", anchor, model.ActionInsert, []string{"C"}, document.SubmitOpts{
		AfterSeen: &aID,
		LineIDs:   []model.ID{cID},
	}); err != nil {
		t.Fatal(err)
	}
	claimID := model.NewID()

	got, err := receiveOrdinaryInsert(d, "丙", anchor.Hex(), true,
		insertOp("丁", anchor.Hex(), model.ActionInsert, []string{"D"}, []string{model.NewID().Hex()}, strPtr(tail.Hex()), nil),
		claimID)
	if err != nil {
		t.Fatal(err)
	}
	if got != nil {
		t.Fatalf("插入不发主张: %+v", got)
	}
	found := false
	for _, ln := range mustView(t, d).Lines {
		if ln.Content == "D" {
			found = true
		}
	}
	if !found {
		t.Fatal("应写入 D")
	}
}

func TestReceiveOrdinaryInsertActiveObserverSyncedA(t *testing.T) {
	anchor, tail := model.NewID(), model.NewID()
	aID := model.NewID()
	d := loadDoc(t, []model.Line{
		{ID: anchor, Content: "锚", Next: tail},
		{ID: tail, Content: "尾", Prev: anchor},
	}, nil)
	if err := d.SubmitWith("甲", anchor, model.ActionInsert, []string{"A"}, document.SubmitOpts{
		AfterSeen: &tail,
		LineIDs:   []model.ID{aID},
	}); err != nil {
		t.Fatal(err)
	}
	claimID := model.NewID()

	got, err := receiveOrdinaryInsert(d, "观察者", anchor.Hex(), true,
		insertOp("丁", anchor.Hex(), model.ActionInsert, []string{"D"}, []string{model.NewID().Hex()}, strPtr(tail.Hex()), nil),
		claimID)
	if err != nil {
		t.Fatal(err)
	}
	if got != nil {
		t.Fatalf("插入不发主张: %+v", got)
	}
	found := false
	for _, ln := range mustView(t, d).Lines {
		if ln.Content == "D" {
			found = true
		}
	}
	if !found {
		t.Fatal("应写入 D")
	}
}

func TestReceiveOrdinaryInsertActiveBeforeStackedAC(t *testing.T) {
	head := model.NewID()
	aID, cID := model.NewID(), model.NewID()
	d := loadDoc(t, []model.Line{{ID: head, Content: "下行"}}, nil)
	zero := model.ID{}
	if err := d.SubmitWith("甲", head, model.ActionInsertBefore, []string{"A"}, document.SubmitOpts{
		BeforeSeen: &zero,
		LineIDs:    []model.ID{aID},
	}); err != nil {
		t.Fatal(err)
	}
	if err := d.SubmitWith("丙", head, model.ActionInsertBefore, []string{"C"}, document.SubmitOpts{
		BeforeSeen: &aID,
		LineIDs:    []model.ID{cID},
	}); err != nil {
		t.Fatal(err)
	}
	claimID := model.NewID()

	got, err := receiveOrdinaryInsert(d, "丙", head.Hex(), true,
		insertOp("丁", head.Hex(), model.ActionInsertBefore, []string{"D"}, []string{model.NewID().Hex()}, nil, strPtr("")),
		claimID)
	if err != nil {
		t.Fatal(err)
	}
	if got != nil {
		t.Fatalf("前插不发主张: %+v", got)
	}
	found := false
	for _, ln := range mustView(t, d).Lines {
		if ln.Content == "D" {
			found = true
		}
	}
	if !found {
		t.Fatal("应写入 D")
	}
}

func TestReceiveOrdinaryInsertExistingForeignCandidateKeepsDoc(t *testing.T) {
	anchor := model.NewID()
	foreignID := model.NewID()
	ownID := model.NewID()
	d := loadDoc(t, []model.Line{{ID: anchor, Content: "锚"}}, []model.Dispute{
		{ID: ownID, RealLine: anchor, Action: model.ActionInsert, Person: "甲", Content: []string{"我的候选"}},
		{ID: foreignID, RealLine: anchor, Action: model.ActionInsert, Person: "乙", Content: []string{"外来候选"}},
	})
	got, err := receiveOrdinaryInsert(d, "甲", "", false,
		insertOp("丙", anchor.Hex(), model.ActionInsert, []string{"普通包"}, []string{model.NewID().Hex()}, strPtr(""), nil),
		model.NewID())
	if err != nil {
		t.Fatal(err)
	}
	if got != nil {
		t.Fatalf("不应 CC: %+v", got)
	}
	v := mustView(t, d)
	if len(v.Disputes) != 2 {
		t.Fatalf("主张应留着: %+v", v.Disputes)
	}
	found := false
	for _, ln := range v.Lines {
		if ln.Content == "普通包" {
			found = true
		}
	}
	if !found {
		t.Fatal("应写入普通插入")
	}
}

func TestReceiveOrdinaryInsertParseFailNoMutate(t *testing.T) {
	anchor := model.NewID()
	d := loadDoc(t, []model.Line{{ID: anchor, Content: "锚"}}, nil)
	before := docSnap(t, d)

	_, err := receiveOrdinaryInsert(d, "甲", "", false,
		insertOp("乙", "bad-id", model.ActionInsert, []string{"x"}, []string{model.NewID().Hex()}, nil, nil),
		model.NewID())
	if err == nil {
		t.Fatal("期望解析错误")
	}
	if !slices.Equal(before, docSnap(t, d)) {
		t.Fatal("解析失败不得改 Doc")
	}

	_, err = receiveOrdinaryInsert(d, "甲", "", false,
		insertOp("乙", anchor.Hex(), model.ActionInsert, []string{"a"}, []string{"bad-id"}, nil, nil),
		model.NewID())
	if err == nil {
		t.Fatal("期望 LineIDs 解析错误")
	}
	if !slices.Equal(before, docSnap(t, d)) {
		t.Fatal("LineIDs 解析失败不得改 Doc")
	}

	_, err = receiveOrdinaryInsert(d, "甲", "", false,
		insertOp("乙", model.NewID().Hex(), model.ActionInsert, []string{"x"}, []string{model.NewID().Hex()}, nil, nil),
		model.NewID())
	if err == nil {
		t.Fatal("期望缺行错误")
	}
	if !slices.Equal(before, docSnap(t, d)) {
		t.Fatal("缺行不得改 Doc")
	}
}

func TestReceiveOrdinaryInsertIgnoresOwnAndNonInsert(t *testing.T) {
	anchor := model.NewID()
	d := loadDoc(t, []model.Line{{ID: anchor, Content: "锚"}}, nil)
	before := docSnap(t, d)

	got, err := receiveOrdinaryInsert(d, "甲", anchor.Hex(), true,
		insertOp("甲", anchor.Hex(), model.ActionInsert, []string{"自己"}, []string{model.NewID().Hex()}, nil, nil),
		model.NewID())
	if err != nil || got != nil {
		t.Fatalf("自己 Op: got=%v err=%v", got, err)
	}
	got, err = receiveOrdinaryInsert(d, "甲", anchor.Hex(), true,
		editOp("乙", anchor.Hex(), []string{"改"}, nil, nil), model.NewID())
	if err != nil || got != nil {
		t.Fatalf("非插入: got=%v err=%v", got, err)
	}
	if !slices.Equal(before, docSnap(t, d)) {
		t.Fatal("不得改 Doc")
	}
}
