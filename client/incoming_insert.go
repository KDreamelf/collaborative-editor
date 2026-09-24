package main

import (
	"errors"
	"fmt"
	"slices"

	"github.com/KDreamelf/collaborative-editor/internal/document"
	"github.com/KDreamelf/collaborative-editor/internal/model"
	"github.com/KDreamelf/collaborative-editor/internal/protocol"
)

// ownInsertCC 在收到远端普通插入时，若落在本人活跃注意力锚点且段文不同，
// 构造本人 DisputeCC。不改 Doc，不进本地争议。
func ownInsertCC(doc *document.Doc, self, attentionLine string, active bool, incoming protocol.Op, claimID model.ID) (*protocol.DisputeCC, error) {
	if incoming.Kind != protocol.TypeSubmit || incoming.Submit == nil || !model.IsInsertAction(incoming.Submit.Action) {
		return nil, nil
	}
	sub := incoming.Submit
	if sub.PersonID == self || !active || sub.LineID != attentionLine {
		return nil, nil
	}

	lineID, err := model.ParseID(sub.LineID)
	if err != nil {
		return nil, fmt.Errorf("无效行ID %q: %w", sub.LineID, err)
	}
	if lineID.IsZero() {
		return nil, fmt.Errorf("无效行ID %q", sub.LineID)
	}

	own, err := doc.InsertBelief(self, lineID, sub.Action)
	if err != nil {
		if errors.Is(err, document.ErrLine) {
			return nil, fmt.Errorf("本地缺少行 %s", sub.LineID)
		}
		return nil, err
	}
	if slices.Equal(sub.Content, own) {
		return nil, nil
	}

	return &protocol.DisputeCC{
		TargetPersonID: sub.PersonID,
		Claim: model.Dispute{
			ID:       claimID,
			RealLine: lineID,
			Action:   sub.Action,
			Person:   self,
			Content:  append([]string(nil), own...),
		},
	}, nil
}

// receiveOrdinaryInsert 处理远端普通插入同步包的单步决策。
// 活跃注意力同锚且异段：只返回本人 DisputeCC，不改 Doc。
// 已有同锚同向外來候选：保持 Doc，等对方主张包。
// 其余：用接收前本端 View 的 Next/Prev 作 AfterSeen/BeforeSeen 走兼容插入整合，零争议。
func receiveOrdinaryInsert(doc *document.Doc, self, attentionLine string, active bool, op protocol.Op, claimID model.ID) (*protocol.DisputeCC, error) {
	if op.Kind != protocol.TypeSubmit || op.Submit == nil || !model.IsInsertAction(op.Submit.Action) {
		return nil, nil
	}
	sub := op.Submit
	if sub.PersonID == self {
		return nil, nil
	}

	cc, err := ownInsertCC(doc, self, attentionLine, active, op, claimID)
	if err != nil {
		return nil, err
	}
	if cc != nil {
		return cc, nil
	}

	lineID, err := model.ParseID(sub.LineID)
	if err != nil {
		return nil, fmt.Errorf("无效行ID %q: %w", sub.LineID, err)
	}
	if lineID.IsZero() {
		return nil, fmt.Errorf("无效行ID %q", sub.LineID)
	}

	var lineIDs []model.ID
	if len(sub.LineIDs) > 0 {
		lineIDs = make([]model.ID, len(sub.LineIDs))
		for i, raw := range sub.LineIDs {
			id, err := model.ParseID(raw)
			if err != nil {
				return nil, fmt.Errorf("无效行ID %q: %w", raw, err)
			}
			lineIDs[i] = id
		}
	}

	v, err := doc.View()
	if err != nil {
		return nil, err
	}
	var next, prev model.ID
	found := false
	for _, ln := range v.Lines {
		if ln.ID == lineID {
			next = ln.Next
			prev = ln.Prev
			found = true
			break
		}
	}
	if !found {
		return nil, fmt.Errorf("本地缺少行 %s", sub.LineID)
	}
	for _, d := range v.Disputes {
		if d.RealLine == lineID && d.Action == sub.Action && d.Person != self {
			return nil, nil
		}
	}

	opts := document.SubmitOpts{LineIDs: lineIDs}
	switch sub.Action {
	case model.ActionInsert:
		seen := next
		opts.AfterSeen = &seen
	case model.ActionInsertBefore:
		seen := prev
		opts.BeforeSeen = &seen
	}
	if err := doc.SubmitWith(sub.PersonID, lineID, sub.Action, append([]string(nil), sub.Content...), opts); err != nil {
		return nil, err
	}
	return nil, nil
}
