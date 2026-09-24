package server

import (
	"testing"

	"github.com/KDreamelf/collaborative-editor/internal/document"
	"github.com/KDreamelf/collaborative-editor/internal/model"
	"github.com/KDreamelf/collaborative-editor/internal/protocol"
)

// 甲停笔后乙普通 Edit 覆盖正文；甲旧 DisputeCC 经 Follow→自动 Answer 应收束。
func TestAttentionExitFollowAnswerClearsOldClaim(t *testing.T) {
	r := &room{doc: document.New("t")}
	line := mustView(t, r).Lines[0].ID
	lineHex := line.Hex()

	if err := r.applyNeutralOp("甲", protocol.Op{
		ID: "seed", Kind: protocol.TypeSubmit,
		Submit: &protocol.Submit{PersonID: "甲", LineID: lineHex, Action: model.ActionEdit, Content: []string{"底稿"}},
	}); err != nil {
		t.Fatal(err)
	}

	jiaID, yiID := model.NewID(), model.NewID()
	if err := r.applyNeutralOp("甲", protocol.Op{
		ID: "cc甲", Kind: protocol.TypeDisputeCC,
		DisputeCC: &protocol.DisputeCC{
			TargetPersonID: "乙",
			Claim: model.Dispute{
				ID: jiaID, RealLine: line, Action: model.ActionEdit, Person: "甲",
				Content: []string{"甲文"}, Followers: []string{},
			},
		},
	}); err != nil {
		t.Fatal(err)
	}

	if err := r.applyNeutralOp("乙", protocol.Op{
		ID: "plain乙", Kind: protocol.TypeSubmit,
		Submit: &protocol.Submit{PersonID: "乙", LineID: lineHex, Action: model.ActionEdit, Content: []string{"乙2"}},
	}); err != nil {
		t.Fatal(err)
	}

	afterPlain := mustView(t, r)
	if afterPlain.Lines[0].Content != "乙2" {
		t.Fatalf("第3步后正式行应为乙2: got %q", afterPlain.Lines[0].Content)
	}
	jia, ok := disputeByPerson(afterPlain, "甲")
	if !ok || jia.Content[0] != "甲文" {
		t.Fatalf("第3步后甲旧主张应仍在: disputes=%+v", afterPlain.Disputes)
	}
	if len(afterPlain.Disputes) != 1 {
		t.Fatalf("第3步后应仅甲一份主张: %+v", afterPlain.Disputes)
	}

	if err := r.applyNeutralOp("乙", protocol.Op{
		ID: "cc乙", Kind: protocol.TypeDisputeCC,
		DisputeCC: &protocol.DisputeCC{
			TargetPersonID: "甲",
			Claim: model.Dispute{
				ID: yiID, RealLine: line, Action: model.ActionEdit, Person: "乙",
				Content: []string{"乙2"}, Followers: []string{},
			},
		},
	}); err != nil {
		t.Fatal(err)
	}

	if err := r.applyNeutralOp("甲", protocol.Op{
		ID: "f甲", Kind: protocol.TypeFollow,
		Follow: &protocol.Follow{PersonID: "甲", DisputeID: yiID.Hex(), ClientTs: 1},
	}); err != nil {
		t.Fatal(err)
	}
	mid := mustView(t, r)
	yi, ok := disputeByPerson(mid, "乙")
	if !ok || len(yi.Pending) != 1 || yi.Pending[0].From != "甲" {
		t.Fatalf("第5步后应对乙主张挂 pending: %+v", mid.Disputes)
	}

	if err := r.applyNeutralOp("乙", protocol.Op{
		ID: "fa乙", Kind: protocol.TypeFollowAnswer,
		FollowAnswer: &protocol.FollowAnswer{PersonID: "乙", FromID: "甲", DisputeID: yiID.Hex(), Accept: true},
	}); err != nil {
		t.Fatal(err)
	}

	final := mustView(t, r)
	if final.Lines[0].Content != "乙2" {
		t.Fatalf("第6步后正式行仍应是乙2: got %q", final.Lines[0].Content)
	}
	if len(final.Disputes) != 0 {
		t.Fatalf("第6步后主张应收干净: disputes=%+v", final.Disputes)
	}
	for _, d := range final.Disputes {
		if len(d.Pending) != 0 {
			t.Fatalf("第6步后不得留 pending: %+v", d)
		}
	}
}

func disputeByPerson(v document.View, person string) (model.Dispute, bool) {
	for _, d := range v.Disputes {
		if d.Person == person {
			return d, true
		}
	}
	return model.Dispute{}, false
}
