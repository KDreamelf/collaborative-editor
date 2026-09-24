package main

import (
	"testing"

	"github.com/KDreamelf/collaborative-editor/internal/document"
	"github.com/KDreamelf/collaborative-editor/internal/model"
	"github.com/KDreamelf/collaborative-editor/internal/protocol"
)

func TestDisputeRecordDoesNotCreateOwnClaim(t *testing.T) {
	line := model.NewID()
	doc, err := document.Load(model.Article{ID: model.NewID(), Title: "t"}, []model.Line{{ID: line, Content: "正文"}}, nil)
	if err != nil {
		t.Fatal(err)
	}
	app := NewAppWithStateDir(t.TempDir())
	app.personID = "丙"
	app.doc = doc
	app.connGen = 1
	claimID := model.NewID()
	app.onDisputeRecord(protocol.DisputeRecord{
		Type: protocol.TypeDisputeRecord,
		Disputes: []model.Dispute{{
			ID: claimID, RealLine: line, Action: model.ActionEdit, Person: "甲",
			Content: []string{"甲主张"}, Followers: []string{},
		}},
	}, app.connGen)
	v := appView(t, app)
	if len(v.Disputes) != 1 || v.Disputes[0].Person != "甲" || v.Disputes[0].ID != claimID {
		t.Fatalf("旁观只保留服务器记录: %+v", v.Disputes)
	}
	if len(editCCs(app)) != 0 {
		t.Fatalf("落盘记录不得让旁观者发包: %+v", editCCs(app))
	}

	app.onDisputeRecord(protocol.DisputeRecord{Type: protocol.TypeDisputeRecord, Disputes: nil}, app.connGen)
	if n := len(appView(t, app).Disputes); n != 0 {
		t.Fatalf("记录清空后旁观争议应消失: %d", n)
	}
}

func TestMisaddressedDisputeCCIgnored(t *testing.T) {
	line := model.NewID()
	doc, err := document.Load(model.Article{ID: model.NewID(), Title: "t"}, []model.Line{{ID: line, Content: "正文"}}, nil)
	if err != nil {
		t.Fatal(err)
	}
	app := NewAppWithStateDir(t.TempDir())
	app.personID = "丙"
	app.doc = doc
	app.connGen = 1
	app.relayReady = true
	app.onRelay(protocol.RelayEvent{Type: protocol.TypeRelay, Op: protocol.Op{
		ID:   model.NewID().Hex(),
		Kind: protocol.TypeDisputeCC,
		DisputeCC: &protocol.DisputeCC{
			TargetPersonID: "乙",
			Claim: model.Dispute{
				ID: model.NewID(), RealLine: line, Action: model.ActionEdit, Person: "甲",
				Content: []string{"甲主张"}, Followers: []string{},
			},
		},
	}}, app.connGen)
	if n := len(appView(t, app).Disputes); n != 0 {
		t.Fatalf("没写给丙的主张包不得入争议: %d", n)
	}
	if len(editCCs(app)) != 0 {
		t.Fatal("没写给丙的主张包不得让丙回包")
	}
}
