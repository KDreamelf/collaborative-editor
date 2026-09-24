package main

import (
	"slices"
	"testing"

	"github.com/KDreamelf/collaborative-editor/internal/document"
	"github.com/KDreamelf/collaborative-editor/internal/model"
	"github.com/KDreamelf/collaborative-editor/internal/protocol"
)

func TestRebuildFromChainAndClaimsEmpty(t *testing.T) {
	line := model.NewID()
	article := model.Article{ID: model.NewID(), Title: "t"}
	baseLines := []model.Line{{ID: line, Content: "共享正文"}}
	local := loadDoc(t, baseLines, nil)
	before := docSnap(t, local)

	boot := protocol.Bootstrap{
		Type: protocol.TypeBootstrap,
		Base: document.View{Article: article, Lines: baseLines, Disputes: []model.Dispute{}},
	}
	got, err := rebuildFromChainAndClaims("甲", local, boot, nil)
	if err != nil {
		t.Fatal(err)
	}
	v := mustView(t, got)
	if len(v.Lines) != 1 || v.Lines[0].Content != "共享正文" || v.Lines[0].ID != line {
		t.Fatalf("链应原样: %+v", v.Lines)
	}
	if len(v.Disputes) != 0 {
		t.Fatalf("Disputes 应 0: %+v", v.Disputes)
	}
	if !slices.Equal(before, docSnap(t, local)) {
		t.Fatal("不得改 local")
	}
}

func TestRebuildFromChainAndClaimsOwnOnlyNoDispute(t *testing.T) {
	line := model.NewID()
	article := model.Article{ID: model.NewID(), Title: "t"}
	baseLines := []model.Line{{ID: line, Content: "他"}}
	ownID := model.NewID()
	local := loadDoc(t, []model.Line{{ID: line, Content: "我"}}, nil)
	before := docSnap(t, local)

	boot := protocol.Bootstrap{
		Base: document.View{Article: article, Lines: baseLines, Disputes: []model.Dispute{}},
		Disputes: []model.Dispute{{
			ID: ownID, RealLine: line, Action: model.ActionEdit, Person: "甲",
			Content: []string{"我"}, Followers: []string{},
		}},
	}
	got, err := rebuildFromChainAndClaims("甲", local, boot, map[string]model.ID{
		line.Hex() + "\x00" + model.ActionEdit: ownID,
	})
	if err != nil {
		t.Fatal(err)
	}
	v := mustView(t, got)
	if len(v.Disputes) != 0 {
		t.Fatalf("仅本人 CC 不得进争议: %+v", v.Disputes)
	}
	if len(v.Lines) != 1 || v.Lines[0].Content != "他" {
		t.Fatalf("正文取服务器链: %+v", v.Lines)
	}
	if !slices.Equal(before, docSnap(t, local)) {
		t.Fatal("不得改 local")
	}
}

func TestRebuildFromChainAndClaimsOwnEditPlusForeign(t *testing.T) {
	line := model.NewID()
	article := model.Article{ID: model.NewID(), Title: "t"}
	ownID, foreignID := model.NewID(), model.NewID()
	local := loadDoc(t, []model.Line{{ID: line, Content: "旧链"}}, []model.Dispute{{
		ID: ownID, RealLine: line, Action: model.ActionEdit, Person: "甲",
		Content: []string{"我"}, Followers: []string{},
	}})
	before := docSnap(t, local)

	boot := protocol.Bootstrap{
		Base: document.View{
			Article:  article,
			Lines:    []model.Line{{ID: line, Content: "他"}},
			Disputes: []model.Dispute{},
		},
		Disputes: []model.Dispute{{
			ID: foreignID, RealLine: line, Action: model.ActionEdit, Person: "乙",
			Content: []string{"他"}, Followers: []string{},
		}},
	}
	got, err := rebuildFromChainAndClaims("甲", local, boot, map[string]model.ID{
		line.Hex() + "\x00" + model.ActionEdit: ownID,
	})
	if err != nil {
		t.Fatal(err)
	}
	v := mustView(t, got)
	if len(v.Lines) != 1 || v.Lines[0].Content != "他" {
		t.Fatalf("正式链取服务器: %+v", v.Lines)
	}
	if len(v.Disputes) != 2 {
		t.Fatalf("应两份候选: %+v", v.Disputes)
	}
	var own, foreign *model.Dispute
	for i := range v.Disputes {
		d := &v.Disputes[i]
		switch d.Person {
		case "甲":
			own = d
		case "乙":
			foreign = d
		}
	}
	if own == nil || foreign == nil {
		t.Fatalf("缺本人或他人: %+v", v.Disputes)
	}
	if own.ID != ownID || !slices.Equal(own.Content, []string{"我"}) {
		t.Fatalf("本人候选应仍「我」+稳定 ID: %+v", own)
	}
	if foreign.ID != foreignID || !slices.Equal(foreign.Content, []string{"他"}) {
		t.Fatalf("他人候选: %+v", foreign)
	}
	if !slices.Equal(before, docSnap(t, local)) {
		t.Fatal("不得改 local")
	}
}

// 活跃行已写「我」、CC 仍在 queue：local.Disputes 空，服务器链「他」+乙 CC。
// 须从 EditBelief 回填本人「我」，不得用服务器链合成「他」。
func TestRebuildFromChainAndClaimsEditBeliefWhenDisputesEmpty(t *testing.T) {
	line := model.NewID()
	article := model.Article{ID: model.NewID(), Title: "t"}
	ownID, foreignID := model.NewID(), model.NewID()
	local := loadDoc(t, []model.Line{{ID: line, Content: "我"}}, nil)
	before := docSnap(t, local)

	boot := protocol.Bootstrap{
		Base: document.View{
			Article:  article,
			Lines:    []model.Line{{ID: line, Content: "他"}},
			Disputes: []model.Dispute{},
		},
		Disputes: []model.Dispute{{
			ID: foreignID, RealLine: line, Action: model.ActionEdit, Person: "乙",
			Content: []string{"他"}, Followers: []string{},
		}},
	}
	got, err := rebuildFromChainAndClaims("甲", local, boot, map[string]model.ID{
		line.Hex() + "\x00" + model.ActionEdit: ownID,
	})
	if err != nil {
		t.Fatal(err)
	}
	v := mustView(t, got)
	if len(v.Lines) != 1 || v.Lines[0].Content != "他" {
		t.Fatalf("正式链取服务器: %+v", v.Lines)
	}
	var own, foreign *model.Dispute
	for i := range v.Disputes {
		d := &v.Disputes[i]
		switch d.Person {
		case "甲":
			own = d
		case "乙":
			foreign = d
		}
	}
	if own == nil || foreign == nil {
		t.Fatalf("缺本人或他人: %+v", v.Disputes)
	}
	if own.ID != ownID || !slices.Equal(own.Content, []string{"我"}) {
		t.Fatalf("本人应 EditBelief「我」+稳定 ID: %+v", own)
	}
	if foreign.ID != foreignID || !slices.Equal(foreign.Content, []string{"他"}) {
		t.Fatalf("他人候选: %+v", foreign)
	}
	if !slices.Equal(before, docSnap(t, local)) {
		t.Fatal("不得改 local")
	}
}

func TestRebuildFromChainAndClaimsInsertPromoteNoDouble(t *testing.T) {
	seed := document.New("t")
	if err := seed.EnsureLines(2); err != nil {
		t.Fatal(err)
	}
	before := mustView(t, seed)
	anchor, tail := before.Lines[0].ID, before.Lines[1].ID
	aID, cID := model.NewID(), model.NewID()
	if err := seed.SubmitWith("甲", anchor, model.ActionInsert, []string{"A"}, document.SubmitOpts{
		AfterSeen: idPtr(tail), LineIDs: []model.ID{aID},
	}); err != nil {
		t.Fatal(err)
	}
	if err := seed.SubmitWith("丙", anchor, model.ActionInsert, []string{"C"}, document.SubmitOpts{
		AfterSeen: idPtr(aID), LineIDs: []model.ID{cID},
	}); err != nil {
		t.Fatal(err)
	}
	chain := mustView(t, seed)
	if len(chain.Lines) != 4 {
		t.Fatalf("种子链应锚+C+A+尾: %+v", chain.Lines)
	}

	ownID, foreignID := model.NewID(), model.NewID()
	local := loadDoc(t, chain.Lines, nil)
	localBefore := docSnap(t, local)

	boot := protocol.Bootstrap{
		Base: document.View{
			Article:  chain.Article,
			Lines:    append([]model.Line(nil), chain.Lines...),
			Disputes: []model.Dispute{},
		},
		Disputes: []model.Dispute{{
			ID: foreignID, RealLine: anchor, Action: model.ActionInsert, Person: "丁",
			Content: []string{"D"}, Followers: []string{},
		}},
	}
	got, err := rebuildFromChainAndClaims("丙", local, boot, map[string]model.ID{
		anchor.Hex() + "\x00" + model.ActionInsert: ownID,
	})
	if err != nil {
		t.Fatal(err)
	}
	v := mustView(t, got)
	if len(v.Lines) != 2 || v.Lines[0].ID != anchor || v.Lines[1].ID != tail {
		t.Fatalf("应只留锚+原后继，不得双显插入段: %+v", v.Lines)
	}
	if len(v.Disputes) != 3 {
		t.Fatalf("候选 A / C+A / D 三份: %+v", v.Disputes)
	}
	by := map[string]model.Dispute{}
	for _, d := range v.Disputes {
		by[d.Person] = d
	}
	if !slices.Equal(by["甲"].Content, []string{"A"}) {
		t.Fatalf("甲=A: %+v", by["甲"])
	}
	if by["丙"].ID != ownID || !slices.Equal(by["丙"].Content, []string{"C", "A"}) {
		t.Fatalf("丙=C+A 稳定 ID: %+v", by["丙"])
	}
	if by["丁"].ID != foreignID || !slices.Equal(by["丁"].Content, []string{"D"}) {
		t.Fatalf("丁=D: %+v", by["丁"])
	}
	if !slices.Equal(localBefore, docSnap(t, local)) {
		t.Fatal("不得改 local")
	}
}

// 本地已整合 C+A 尚未入 Disputes；服务器链仅锚+尾 + 乙 Insert CC。
// 须 InsertBelief 保留本人 C+A，不得丢成空/服务器合成。
func TestRebuildFromChainAndClaimsInsertBeliefWhenDisputesEmpty(t *testing.T) {
	seed := document.New("t")
	if err := seed.EnsureLines(2); err != nil {
		t.Fatal(err)
	}
	before := mustView(t, seed)
	anchor, tail := before.Lines[0].ID, before.Lines[1].ID
	aID, cID := model.NewID(), model.NewID()
	if err := seed.SubmitWith("甲", anchor, model.ActionInsert, []string{"A"}, document.SubmitOpts{
		AfterSeen: idPtr(tail), LineIDs: []model.ID{aID},
	}); err != nil {
		t.Fatal(err)
	}
	if err := seed.SubmitWith("丙", anchor, model.ActionInsert, []string{"C"}, document.SubmitOpts{
		AfterSeen: idPtr(aID), LineIDs: []model.ID{cID},
	}); err != nil {
		t.Fatal(err)
	}
	localView := mustView(t, seed)
	if len(localView.Lines) != 4 {
		t.Fatalf("本地应锚+C+A+尾: %+v", localView.Lines)
	}
	localBefore := docSnap(t, seed)

	ownID, foreignID := model.NewID(), model.NewID()
	headLine := model.Line{ID: anchor, Content: localView.Lines[0].Content, Next: tail}
	tailLine := model.Line{ID: tail, Content: localView.Lines[3].Content, Prev: anchor}
	boot := protocol.Bootstrap{
		Base: document.View{
			Article:  localView.Article,
			Lines:    []model.Line{headLine, tailLine},
			Disputes: []model.Dispute{},
		},
		Disputes: []model.Dispute{{
			ID: foreignID, RealLine: anchor, Action: model.ActionInsert, Person: "乙",
			Content: []string{"B"}, Followers: []string{},
		}},
	}
	got, err := rebuildFromChainAndClaims("丙", seed, boot, map[string]model.ID{
		anchor.Hex() + "\x00" + model.ActionInsert: ownID,
	})
	if err != nil {
		t.Fatal(err)
	}
	v := mustView(t, got)
	if len(v.Lines) != 2 || v.Lines[0].ID != anchor || v.Lines[1].ID != tail {
		t.Fatalf("正式链取服务器锚+尾: %+v", v.Lines)
	}
	var own, foreign *model.Dispute
	for i := range v.Disputes {
		d := &v.Disputes[i]
		switch d.Person {
		case "丙":
			own = d
		case "乙":
			foreign = d
		}
	}
	if own == nil || foreign == nil {
		t.Fatalf("缺本人或他人: %+v", v.Disputes)
	}
	if own.ID != ownID || !slices.Equal(own.Content, []string{"C", "A"}) {
		t.Fatalf("本人应 InsertBelief C+A: %+v", own)
	}
	if foreign.ID != foreignID || !slices.Equal(foreign.Content, []string{"B"}) {
		t.Fatalf("他人候选: %+v", foreign)
	}
	if !slices.Equal(localBefore, docSnap(t, seed)) {
		t.Fatal("不得改 local")
	}
}

func TestRebuildFromChainAndClaimsNilLocalUsesServerChain(t *testing.T) {
	line := model.NewID()
	article := model.Article{ID: model.NewID(), Title: "t"}
	foreignID := model.NewID()
	boot := protocol.Bootstrap{
		Base: document.View{
			Article: article, Lines: []model.Line{{ID: line, Content: "他"}}, Disputes: []model.Dispute{},
		},
		Disputes: []model.Dispute{{
			ID: foreignID, RealLine: line, Action: model.ActionEdit, Person: "乙",
			Content: []string{"他"}, Followers: []string{},
		}},
	}
	got, err := rebuildFromChainAndClaims("甲", nil, boot, nil)
	if err != nil {
		t.Fatal(err)
	}
	v := mustView(t, got)
	if len(v.Lines) != 1 || v.Lines[0].Content != "他" {
		t.Fatalf("正式链: %+v", v.Lines)
	}
	if len(v.Disputes) != 1 || v.Disputes[0].ID != foreignID || v.Disputes[0].Person != "乙" {
		t.Fatalf("无本人主张时只保留服务器记录: %+v", v.Disputes)
	}
}

func TestRebuildFromChainAndClaimsForeignIDIdempotent(t *testing.T) {
	line := model.NewID()
	article := model.Article{ID: model.NewID(), Title: "t"}
	ownID, foreignID := model.NewID(), model.NewID()
	foreign := model.Dispute{
		ID: foreignID, RealLine: line, Action: model.ActionEdit, Person: "乙",
		Content: []string{"他"}, Followers: []string{},
	}
	local := loadDoc(t, []model.Line{{ID: line, Content: "我"}}, []model.Dispute{{
		ID: ownID, RealLine: line, Action: model.ActionEdit, Person: "甲",
		Content: []string{"我"}, Followers: []string{},
	}})

	boot := protocol.Bootstrap{
		Base: document.View{
			Article: article, Lines: []model.Line{{ID: line, Content: "他"}}, Disputes: []model.Dispute{},
		},
		Disputes: []model.Dispute{foreign, foreign},
	}
	got, err := rebuildFromChainAndClaims("甲", local, boot, map[string]model.ID{
		line.Hex() + "\x00" + model.ActionEdit: ownID,
	})
	if err != nil {
		t.Fatal(err)
	}
	v := mustView(t, got)
	if len(v.Disputes) != 2 {
		t.Fatalf("重复 foreign ID 不增份: %+v", v.Disputes)
	}
}

func TestRebuildFromChainAndClaimsBadClaimKeepsLocal(t *testing.T) {
	line := model.NewID()
	missing := model.NewID()
	article := model.Article{ID: model.NewID(), Title: "t"}
	local := loadDoc(t, []model.Line{{ID: line, Content: "本地"}}, []model.Dispute{{
		ID: model.NewID(), RealLine: line, Action: model.ActionEdit, Person: "甲",
		Content: []string{"本地候选"}, Followers: []string{},
	}})
	before := docSnap(t, local)

	boot := protocol.Bootstrap{
		Base: document.View{
			Article: article, Lines: []model.Line{{ID: line, Content: "他"}}, Disputes: []model.Dispute{},
		},
		Disputes: []model.Dispute{{
			ID: model.NewID(), RealLine: missing, Action: model.ActionEdit, Person: "乙",
			Content: []string{"幽灵"}, Followers: []string{},
		}},
	}
	got, err := rebuildFromChainAndClaims("甲", local, boot, nil)
	if err == nil || got != nil {
		t.Fatalf("缺行应 error 且无新 Doc: got=%v err=%v", got, err)
	}
	if !slices.Equal(before, docSnap(t, local)) {
		t.Fatal("失败不得改 local")
	}

	boot.Disputes = []model.Dispute{{
		ID: model.ID{}, RealLine: line, Action: model.ActionEdit, Person: "乙",
		Content: []string{"坏"}, Followers: []string{},
	}}
	got, err = rebuildFromChainAndClaims("甲", local, boot, nil)
	if err == nil || got != nil {
		t.Fatalf("坏 claim 应 error: got=%v err=%v", got, err)
	}
	if !slices.Equal(before, docSnap(t, local)) {
		t.Fatal("失败不得改 local")
	}
}

// 外来 Delete + 本地原行非空正文：本人保留行（EditBelief）+ 外来删行。
func TestRebuildFromChainAndClaimsForeignDeleteKeepsOwnEdit(t *testing.T) {
	line := model.NewID()
	article := model.Article{ID: model.NewID(), Title: "t"}
	ownID, foreignID := model.NewID(), model.NewID()
	local := loadDoc(t, []model.Line{{ID: line, Content: "我"}}, nil)
	before := docSnap(t, local)

	boot := protocol.Bootstrap{
		Base: document.View{
			Article:  article,
			Lines:    []model.Line{{ID: line, Content: "他"}},
			Disputes: []model.Dispute{},
		},
		Disputes: []model.Dispute{{
			ID: foreignID, RealLine: line, Action: model.ActionDelete, Person: "乙",
			Content: nil, Followers: []string{},
		}},
	}
	got, err := rebuildFromChainAndClaims("甲", local, boot, map[string]model.ID{
		line.Hex() + "\x00" + model.ActionEdit: ownID,
	})
	if err != nil {
		t.Fatal(err)
	}
	v := mustView(t, got)
	if len(v.Lines) != 1 || v.Lines[0].Content != "他" {
		t.Fatalf("正式链取服务器: %+v", v.Lines)
	}
	var own, foreign *model.Dispute
	for i := range v.Disputes {
		d := &v.Disputes[i]
		switch d.Person {
		case "甲":
			own = d
		case "乙":
			foreign = d
		}
	}
	if own == nil || foreign == nil {
		t.Fatalf("缺本人或他人: %+v", v.Disputes)
	}
	if own.ID != ownID || own.Action != model.ActionEdit || !slices.Equal(own.Content, []string{"我"}) {
		t.Fatalf("本人应保留行「我」+稳定 ID: %+v", own)
	}
	if foreign.ID != foreignID || foreign.Action != model.ActionDelete || len(foreign.Content) != 0 {
		t.Fatalf("外来删这行: %+v", foreign)
	}
	if !slices.Equal(before, docSnap(t, local)) {
		t.Fatal("不得改 local")
	}
}

// 仅本人 Delete CC：无外来争议，正文取服务器链。
func TestRebuildFromChainAndClaimsOwnDeleteOnlyNoDispute(t *testing.T) {
	line := model.NewID()
	article := model.Article{ID: model.NewID(), Title: "t"}
	ownID := model.NewID()
	local := loadDoc(t, []model.Line{{ID: line, Content: "本地"}}, []model.Dispute{{
		ID: ownID, RealLine: line, Action: model.ActionDelete, Person: "甲",
		Content: nil, Followers: []string{},
	}})
	before := docSnap(t, local)

	boot := protocol.Bootstrap{
		Base: document.View{
			Article: article, Lines: []model.Line{{ID: line, Content: "他"}}, Disputes: []model.Dispute{},
		},
		Disputes: []model.Dispute{{
			ID: ownID, RealLine: line, Action: model.ActionDelete, Person: "甲",
			Content: nil, Followers: []string{},
		}},
	}
	got, err := rebuildFromChainAndClaims("甲", local, boot, map[string]model.ID{
		line.Hex() + "\x00" + model.ActionEdit: ownID,
	})
	if err != nil {
		t.Fatal(err)
	}
	v := mustView(t, got)
	if len(v.Disputes) != 0 {
		t.Fatalf("仅本人 CC 不得进争议: %+v", v.Disputes)
	}
	if len(v.Lines) != 1 || v.Lines[0].Content != "他" {
		t.Fatalf("正文取服务器链: %+v", v.Lines)
	}
	if !slices.Equal(before, docSnap(t, local)) {
		t.Fatal("不得改 local")
	}
}

// 本地已有本人 Delete 候选 + 外来 Edit：本人 Delete ID/Content 不丢。
func TestRebuildFromChainAndClaimsOwnDeletePlusForeignEdit(t *testing.T) {
	line := model.NewID()
	article := model.Article{ID: model.NewID(), Title: "t"}
	ownID, foreignID := model.NewID(), model.NewID()
	local := loadDoc(t, []model.Line{{ID: line, Content: "链上"}}, []model.Dispute{{
		ID: ownID, RealLine: line, Action: model.ActionDelete, Person: "甲",
		Content: nil, Followers: []string{},
	}})
	before := docSnap(t, local)

	boot := protocol.Bootstrap{
		Base: document.View{
			Article:  article,
			Lines:    []model.Line{{ID: line, Content: "他"}},
			Disputes: []model.Dispute{},
		},
		Disputes: []model.Dispute{{
			ID: foreignID, RealLine: line, Action: model.ActionEdit, Person: "乙",
			Content: []string{"他"}, Followers: []string{},
		}},
	}
	got, err := rebuildFromChainAndClaims("甲", local, boot, map[string]model.ID{
		line.Hex() + "\x00" + model.ActionEdit: ownID,
	})
	if err != nil {
		t.Fatal(err)
	}
	v := mustView(t, got)
	if len(v.Lines) != 1 || v.Lines[0].Content != "他" {
		t.Fatalf("正式链取服务器: %+v", v.Lines)
	}
	var own, foreign *model.Dispute
	for i := range v.Disputes {
		d := &v.Disputes[i]
		switch d.Person {
		case "甲":
			own = d
		case "乙":
			foreign = d
		}
	}
	if own == nil || foreign == nil {
		t.Fatalf("缺本人或他人: %+v", v.Disputes)
	}
	if own.ID != ownID || own.Action != model.ActionDelete || len(own.Content) != 0 {
		t.Fatalf("本人 Delete ID/Content 应保留: %+v", own)
	}
	if foreign.ID != foreignID || foreign.Action != model.ActionEdit || !slices.Equal(foreign.Content, []string{"他"}) {
		t.Fatalf("他人 Edit: %+v", foreign)
	}
	if !slices.Equal(before, docSnap(t, local)) {
		t.Fatal("不得改 local")
	}
}

func idPtr(id model.ID) *model.ID { return &id }
