package protocol

import (
	"bytes"
	"encoding/json"
	"testing"

	"github.com/KDreamelf/collaborative-editor/internal/document"
	"github.com/KDreamelf/collaborative-editor/internal/model"
)

func TestDisputeCCAndBootstrapJSONRoundTrip(t *testing.T) {
	claimID := model.NewID()
	claim := model.Dispute{
		ID:      claimID,
		Person:  "甲",
		Content: []string{"改后正文"},
		Action:  model.ActionEdit,
	}
	op := Op{
		ID:   "op-1",
		Kind: TypeDisputeCC,
		DisputeCC: &DisputeCC{
			TargetPersonID: "乙",
			Claim:          claim,
		},
	}
	relayRaw, err := json.Marshal(RelayEvent{Type: TypeRelay, Op: op})
	if err != nil {
		t.Fatal(err)
	}
	var relay RelayEvent
	if err := json.Unmarshal(relayRaw, &relay); err != nil || relay.Op.DisputeCC == nil || relay.Op.DisputeCC.Claim.ID != claimID {
		t.Fatalf("主张转发: %+v %v", relay, err)
	}
	boot := Bootstrap{
		Type: TypeBootstrap,
		Base: document.View{
			Article: model.Article{ID: model.NewID(), Title: "t"},
			Lines:   []model.Line{{ID: model.NewID(), Content: "正式行"}},
		},
		Disputes: []model.Dispute{claim},
		YourLine: "line-a",
		People:   []Person{{ID: "甲", Name: "甲"}},
		Cursors:  []Cursor{{PersonID: "甲", Name: "甲", LineID: "line-a", Offset: 0}},
	}

	raw, err := json.Marshal(boot)
	if err != nil {
		t.Fatal(err)
	}
	var got Bootstrap
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatal(err)
	}
	if got.Type != TypeBootstrap {
		t.Fatalf("type=%q", got.Type)
	}
	if bytes.Contains(raw, []byte(`"events"`)) || bytes.Contains(raw, []byte(`"seq"`)) {
		t.Fatalf("入场包不得携带操作历史或全局序号: %s", raw)
	}
	cc := relay.Op
	if cc.DisputeCC.TargetPersonID != "乙" {
		t.Fatalf("target=%q", cc.DisputeCC.TargetPersonID)
	}
	if cc.DisputeCC.Claim.ID != claimID || cc.DisputeCC.Claim.Person != "甲" ||
		len(cc.DisputeCC.Claim.Content) != 1 || cc.DisputeCC.Claim.Content[0] != "改后正文" {
		t.Fatalf("claim=%+v", cc.DisputeCC.Claim)
	}
	if len(got.Disputes) != 1 || got.Disputes[0].ID != claimID {
		t.Fatalf("disputes=%+v", got.Disputes)
	}
}
