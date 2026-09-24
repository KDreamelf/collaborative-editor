package main

import (
	"fmt"

	"github.com/KDreamelf/collaborative-editor/internal/document"
	"github.com/KDreamelf/collaborative-editor/internal/model"
	"github.com/KDreamelf/collaborative-editor/internal/protocol"
)

// receiveOrdinaryEdit 处理远端普通「改这行」同步包的单步决策。
// 活跃注意力同行且异文：只返回本人 DisputeCC（主张抄送包），不改 Doc。
// 已有外来 ActionEdit 候选：保持 Doc，等对方主张包。
// 其余：用接收前本端正式正文作 BaseContent 整合进 Doc，避免旧基准触发自动争议。
func receiveOrdinaryEdit(doc *document.Doc, self, attentionLine string, active bool, op protocol.Op, claimID model.ID) (*protocol.DisputeCC, error) {
	if op.Kind != protocol.TypeSubmit || op.Submit == nil || op.Submit.Action != model.ActionEdit {
		return nil, nil
	}
	sub := op.Submit
	if sub.PersonID == self {
		return nil, nil
	}

	cc, err := ownEditCC(doc, self, attentionLine, active, op, claimID)
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
	var formal string
	found := false
	for _, ln := range v.Lines {
		if ln.ID == lineID {
			formal = ln.Content
			found = true
			break
		}
	}
	if !found {
		return nil, fmt.Errorf("本地缺少行 %s", sub.LineID)
	}
	for _, d := range v.Disputes {
		if d.RealLine == lineID && d.Action == model.ActionEdit && d.Person != self {
			return nil, nil
		}
	}
	if len(sub.Content) == 1 && sub.Content[0] == formal {
		return nil, nil
	}

	base := formal
	opts := document.SubmitOpts{
		BaseContent: &base,
		LineIDs:     lineIDs,
		WholeClaim:  sub.WholeClaim,
	}
	if err := doc.SubmitWith(sub.PersonID, lineID, model.ActionEdit, append([]string(nil), sub.Content...), opts); err != nil {
		return nil, err
	}
	return nil, nil
}
