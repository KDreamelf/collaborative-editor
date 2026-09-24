package main

import (
	"slices"
	"testing"

	"github.com/KDreamelf/collaborative-editor/internal/model"
	"github.com/KDreamelf/collaborative-editor/internal/protocol"
)

func editOp(person, lineHex string, content []string, base *string, lineIDs []string) protocol.Op {
	return protocol.Op{
		Kind: protocol.TypeSubmit,
		Submit: &protocol.Submit{
			Type:        protocol.TypeSubmit,
			PersonID:    person,
			LineID:      lineHex,
			Action:      model.ActionEdit,
			Content:     content,
			BaseContent: base,
			LineIDs:     lineIDs,
		},
	}
}

func TestReceiveOrdinaryEditActiveDiffReturnsOwnCC(t *testing.T) {
	line := model.NewID()
	claimID := model.NewID()
	d := loadDoc(t, []model.Line{{ID: line, Content: "我的正文"}}, nil)
	before := docSnap(t, d)

	got, err := receiveOrdinaryEdit(d, "甲", line.Hex(), true, editOp("乙", line.Hex(), []string{"对方改"}, strPtr("旧"), nil), claimID)
	if err != nil {
		t.Fatal(err)
	}
	after := docSnap(t, d)
	if !slices.Equal(before, after) {
		t.Fatal("View 应不变")
	}
	v := mustView(t, d)
	if len(v.Disputes) != 0 {
		t.Fatalf("disputes=%d", len(v.Disputes))
	}
	if got == nil {
		t.Fatal("期望本人 CC")
	}
	if got.TargetPersonID != "乙" || got.Claim.Person != "甲" || !slices.Equal(got.Claim.Content, []string{"我的正文"}) {
		t.Fatalf("cc=%+v", got)
	}
}

func TestReceiveOrdinaryEditInactiveIntegrates(t *testing.T) {
	line := model.NewID()
	d := loadDoc(t, []model.Line{{ID: line, Content: "旧正文"}}, nil)

	got, err := receiveOrdinaryEdit(d, "甲", line.Hex(), false, editOp("乙", line.Hex(), []string{"新正文"}, strPtr("更旧"), nil), model.NewID())
	if err != nil {
		t.Fatal(err)
	}
	if got != nil {
		t.Fatalf("非活跃不应 CC: %+v", got)
	}
	v := mustView(t, d)
	if len(v.Disputes) != 0 {
		t.Fatalf("disputes=%d", len(v.Disputes))
	}
	if len(v.Lines) != 1 || v.Lines[0].Content != "新正文" {
		t.Fatalf("正文未整合: %+v", v.Lines)
	}
}

func TestReceiveOrdinaryEditOtherAttentionIntegrates(t *testing.T) {
	line := model.NewID()
	other := model.NewID()
	d := loadDoc(t, []model.Line{
		{ID: line, Content: "目标行", Next: other},
		{ID: other, Content: "别行", Prev: line},
	}, nil)

	got, err := receiveOrdinaryEdit(d, "甲", other.Hex(), true, editOp("乙", line.Hex(), []string{"已改"}, nil, nil), model.NewID())
	if err != nil {
		t.Fatal(err)
	}
	if got != nil {
		t.Fatalf("注意力别行不应 CC: %+v", got)
	}
	v := mustView(t, d)
	if len(v.Disputes) != 0 {
		t.Fatalf("disputes=%d", len(v.Disputes))
	}
	if v.Lines[0].Content != "已改" {
		t.Fatalf("未整合: %+v", v.Lines)
	}
}

func TestReceiveOrdinaryEditSameTextNoCC(t *testing.T) {
	line := model.NewID()
	d := loadDoc(t, []model.Line{{ID: line, Content: "同文"}}, nil)
	before := docSnap(t, d)

	got, err := receiveOrdinaryEdit(d, "甲", line.Hex(), true, editOp("乙", line.Hex(), []string{"同文"}, strPtr("更旧"), nil), model.NewID())
	if err != nil {
		t.Fatal(err)
	}
	if got != nil {
		t.Fatalf("同文不应 CC: %+v", got)
	}
	if !slices.Equal(before, docSnap(t, d)) {
		t.Fatal("同文应 no-op")
	}
	if len(mustView(t, d).Disputes) != 0 {
		t.Fatal("disputes 应仍为 0")
	}
}

func TestReceiveOrdinaryEditStaleBaseNoDispute(t *testing.T) {
	line := model.NewID()
	d := loadDoc(t, []model.Line{{ID: line, Content: "本端已新"}}, nil)

	got, err := receiveOrdinaryEdit(d, "甲", "", false, editOp("乙", line.Hex(), []string{"对方结果"}, strPtr("发送方旧基准"), nil), model.NewID())
	if err != nil {
		t.Fatal(err)
	}
	if got != nil {
		t.Fatalf("不应 CC: %+v", got)
	}
	v := mustView(t, d)
	if len(v.Disputes) != 0 {
		t.Fatalf("旧 BaseContent 不得制造争议: %+v", v.Disputes)
	}
	if v.Lines[0].Content != "对方结果" {
		t.Fatalf("应整合为对方结果: %q", v.Lines[0].Content)
	}
}

func TestReceiveOrdinaryEditExistingForeignCandidateKeepsDoc(t *testing.T) {
	line := model.NewID()
	foreignID := model.NewID()
	ownID := model.NewID()
	d := loadDoc(t, []model.Line{{ID: line, Content: "正式"}}, []model.Dispute{
		{ID: ownID, RealLine: line, Action: model.ActionEdit, Person: "甲", Content: []string{"我的候选"}},
		{ID: foreignID, RealLine: line, Action: model.ActionEdit, Person: "乙", Content: []string{"外来候选"}},
	})
	before := docSnap(t, d)

	got, err := receiveOrdinaryEdit(d, "甲", "", false, editOp("丙", line.Hex(), []string{"普通包新文"}, strPtr("正式"), nil), model.NewID())
	if err != nil {
		t.Fatal(err)
	}
	if got != nil {
		t.Fatalf("不应 CC: %+v", got)
	}
	if !slices.Equal(before, docSnap(t, d)) {
		t.Fatal("已有外来候选时普通包不得改 Doc")
	}
	v := mustView(t, d)
	if len(v.Disputes) != 2 {
		t.Fatalf("不得另开候选: disputes=%d", len(v.Disputes))
	}
}

func TestReceiveOrdinaryEditMultilineLineIDs(t *testing.T) {
	line := model.NewID()
	tail := model.NewID()
	d := loadDoc(t, []model.Line{{ID: line, Content: "头"}}, nil)

	got, err := receiveOrdinaryEdit(d, "甲", "", false, editOp("乙", line.Hex(), []string{"hello", "world"}, strPtr("无关旧"), []string{tail.Hex()}), model.NewID())
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
	if len(v.Lines) != 2 {
		t.Fatalf("应拆两行: %+v", v.Lines)
	}
	if v.Lines[0].Content != "hello" || v.Lines[1].Content != "world" {
		t.Fatalf("正文: %+v", v.Lines)
	}
	if v.Lines[0].ID != line || v.Lines[1].ID != tail {
		t.Fatalf("预生 LineIDs 未用上: %s %s", v.Lines[0].ID.Hex(), v.Lines[1].ID.Hex())
	}
}

func TestReceiveOrdinaryEditIgnoresOwnAndOtherKind(t *testing.T) {
	line := model.NewID()
	d := loadDoc(t, []model.Line{{ID: line, Content: "正文"}}, nil)
	before := docSnap(t, d)

	got, err := receiveOrdinaryEdit(d, "甲", line.Hex(), true, editOp("甲", line.Hex(), []string{"自己"}, nil, nil), model.NewID())
	if err != nil || got != nil {
		t.Fatalf("自己 Op: got=%v err=%v", got, err)
	}
	got, err = receiveOrdinaryEdit(d, "甲", line.Hex(), true, protocol.Op{Kind: protocol.TypeDelete}, model.NewID())
	if err != nil || got != nil {
		t.Fatalf("其他 Kind: got=%v err=%v", got, err)
	}
	if !slices.Equal(before, docSnap(t, d)) {
		t.Fatal("不得改 Doc")
	}
}

func TestReceiveOrdinaryEditParseFailNoMutate(t *testing.T) {
	line := model.NewID()
	d := loadDoc(t, []model.Line{{ID: line, Content: "正文"}}, nil)
	before := docSnap(t, d)

	_, err := receiveOrdinaryEdit(d, "甲", "", false, editOp("乙", "bad-id", []string{"x"}, nil, nil), model.NewID())
	if err == nil {
		t.Fatal("期望解析错误")
	}
	if !slices.Equal(before, docSnap(t, d)) {
		t.Fatal("解析失败不得改 Doc")
	}

	_, err = receiveOrdinaryEdit(d, "甲", "", false, editOp("乙", line.Hex(), []string{"a", "b"}, nil, []string{"bad-tail"}), model.NewID())
	if err == nil {
		t.Fatal("期望 LineIDs 解析错误")
	}
	if !slices.Equal(before, docSnap(t, d)) {
		t.Fatal("LineIDs 解析失败不得改 Doc")
	}
}
