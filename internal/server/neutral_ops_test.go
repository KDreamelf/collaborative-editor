package server

import (
	"testing"

	"github.com/KDreamelf/collaborative-editor/internal/document"
	"github.com/KDreamelf/collaborative-editor/internal/model"
	"github.com/KDreamelf/collaborative-editor/internal/protocol"
)

func TestNeutralOrdinaryEditNoDispute(t *testing.T) {
	r := &room{doc: document.New("t")}
	line := mustView(t, r).Lines[0].ID.Hex()

	if err := r.applyNeutralOp("A", protocol.Op{
		ID: "e1", Kind: protocol.TypeSubmit,
		Submit: &protocol.Submit{PersonID: "A", LineID: line, Action: model.ActionEdit, Content: []string{"甲"}},
	}); err != nil {
		t.Fatal(err)
	}
	if err := r.applyNeutralOp("B", protocol.Op{
		ID: "e2", Kind: protocol.TypeSubmit,
		Submit: &protocol.Submit{PersonID: "B", LineID: line, Action: model.ActionEdit, Content: []string{"乙"}},
	}); err != nil {
		t.Fatal(err)
	}
	v := mustView(t, r)
	if len(v.Disputes) != 0 {
		t.Fatalf("普通 Edit 不得造争议: %+v", v.Disputes)
	}
	if v.Lines[0].Content != "乙" {
		t.Fatalf("后写覆盖: got %q", v.Lines[0].Content)
	}
}

func TestNeutralTwoDisputeCC(t *testing.T) {
	r := &room{doc: document.New("t")}
	line := mustView(t, r).Lines[0].ID

	aID, bID := model.NewID(), model.NewID()
	if err := r.applyNeutralOp("A", protocol.Op{
		ID: "ccA", Kind: protocol.TypeDisputeCC,
		DisputeCC: &protocol.DisputeCC{
			TargetPersonID: "B",
			Claim: model.Dispute{
				ID: aID, RealLine: line, Action: model.ActionEdit, Person: "A",
				Content: []string{"甲主张"}, Followers: []string{},
			},
		},
	}); err != nil {
		t.Fatal(err)
	}
	if err := r.applyNeutralOp("B", protocol.Op{
		ID: "ccB", Kind: protocol.TypeDisputeCC,
		DisputeCC: &protocol.DisputeCC{
			TargetPersonID: "A",
			Claim: model.Dispute{
				ID: bID, RealLine: line, Action: model.ActionEdit, Person: "B",
				Content: []string{"乙主张"}, Followers: []string{},
			},
		},
	}); err != nil {
		t.Fatal(err)
	}
	v := mustView(t, r)
	if len(v.Disputes) != 2 {
		t.Fatalf("两份 CC 后恰 2: %+v", v.Disputes)
	}
}

func TestNeutralImpersonationAndBadIDZeroMutation(t *testing.T) {
	r := &room{doc: document.New("t")}
	line := mustView(t, r).Lines[0]
	if err := r.applyNeutralOp("A", protocol.Op{
		ID: "seed", Kind: protocol.TypeSubmit,
		Submit: &protocol.Submit{PersonID: "A", LineID: line.ID.Hex(), Action: model.ActionEdit, Content: []string{"稳"}},
	}); err != nil {
		t.Fatal(err)
	}
	before := mustView(t, r)

	if err := r.applyNeutralOp("A", protocol.Op{
		ID: "", Kind: protocol.TypeSubmit,
		Submit: &protocol.Submit{PersonID: "A", LineID: line.ID.Hex(), Action: model.ActionEdit, Content: []string{"坏ID"}},
	}); err == nil {
		t.Fatal("空 Op.ID 应拒")
	}
	if err := r.applyNeutralOp("A", protocol.Op{
		ID: "fake", Kind: protocol.TypeSubmit,
		Submit: &protocol.Submit{PersonID: "B", LineID: line.ID.Hex(), Action: model.ActionEdit, Content: []string{"冒充"}},
	}); err == nil {
		t.Fatal("冒充 personId 应拒")
	}
	if err := r.applyNeutralOp("A", protocol.Op{
		ID: "fakeCC", Kind: protocol.TypeDisputeCC,
		DisputeCC: &protocol.DisputeCC{
			TargetPersonID: "A",
			Claim: model.Dispute{
				ID: model.NewID(), RealLine: line.ID, Action: model.ActionEdit, Person: "B",
				Content: []string{"冒充主张"},
			},
		},
	}); err == nil {
		t.Fatal("冒充 Claim.Person 应拒")
	}
	after := mustView(t, r)
	if after.Lines[0].Content != before.Lines[0].Content || len(after.Disputes) != len(before.Disputes) {
		t.Fatalf("坏请求零突变: before=%+v after=%+v", before, after)
	}
}

func TestNeutralSuspendNoClaimNoSolo(t *testing.T) {
	r := &room{doc: document.New("t")}
	line := mustView(t, r).Lines[0].ID.Hex()
	if err := r.applyNeutralOp("A", protocol.Op{
		ID: "e", Kind: protocol.TypeSubmit,
		Submit: &protocol.Submit{PersonID: "A", LineID: line, Action: model.ActionEdit, Content: []string{"正文"}},
	}); err != nil {
		t.Fatal(err)
	}
	if err := r.applyNeutralOp("A", protocol.Op{
		ID: "s", Kind: protocol.TypeSuspend,
		Suspend: &protocol.Suspend{PersonID: "A", LineID: line, Action: model.ActionEdit, Suspended: true},
	}); err != nil {
		t.Fatal(err)
	}
	v := mustView(t, r)
	if len(v.Disputes) != 0 || len(v.Suspended) != 0 {
		t.Fatalf("无主张 Suspend 不造候选: disputes=%+v suspended=%+v", v.Disputes, v.Suspended)
	}
	if v.Lines[0].Content != "正文" {
		t.Fatalf("正文被改: %q", v.Lines[0].Content)
	}
}

func TestNeutralFollowAnswerCloses(t *testing.T) {
	r := &room{doc: document.New("t")}
	line := mustView(t, r).Lines[0].ID
	aID, bID := model.NewID(), model.NewID()

	if err := r.applyNeutralOp("A", protocol.Op{
		ID: "ccA", Kind: protocol.TypeDisputeCC,
		DisputeCC: &protocol.DisputeCC{
			TargetPersonID: "B",
			Claim: model.Dispute{
				ID: aID, RealLine: line, Action: model.ActionEdit, Person: "A",
				Content: []string{"甲赢"}, Followers: []string{},
			},
		},
	}); err != nil {
		t.Fatal(err)
	}
	if err := r.applyNeutralOp("B", protocol.Op{
		ID: "ccB", Kind: protocol.TypeDisputeCC,
		DisputeCC: &protocol.DisputeCC{
			TargetPersonID: "A",
			Claim: model.Dispute{
				ID: bID, RealLine: line, Action: model.ActionEdit, Person: "B",
				Content: []string{"乙输"}, Followers: []string{},
			},
		},
	}); err != nil {
		t.Fatal(err)
	}
	if n := len(mustView(t, r).Disputes); n != 2 {
		t.Fatalf("收口前应 2: %d", n)
	}

	if err := r.applyNeutralOp("B", protocol.Op{
		ID: "f", Kind: protocol.TypeFollow,
		Follow: &protocol.Follow{PersonID: "B", DisputeID: aID.Hex(), ClientTs: 1},
	}); err != nil {
		t.Fatal(err)
	}
	if err := r.applyNeutralOp("A", protocol.Op{
		ID: "fa", Kind: protocol.TypeFollowAnswer,
		FollowAnswer: &protocol.FollowAnswer{PersonID: "A", FromID: "B", DisputeID: aID.Hex(), Accept: true},
	}); err != nil {
		t.Fatal(err)
	}
	v := mustView(t, r)
	if len(v.Disputes) != 0 || v.Lines[0].Content != "甲赢" {
		t.Fatalf("Follow+Answer 应收口到甲: %+v", v)
	}
}

func mustView(t *testing.T, r *room) document.View {
	t.Helper()
	v, err := r.doc.View()
	if err != nil {
		t.Fatal(err)
	}
	return v
}
