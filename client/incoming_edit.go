package main

import (
	"fmt"

	"github.com/KDreamelf/collaborative-editor/internal/document"
	"github.com/KDreamelf/collaborative-editor/internal/model"
	"github.com/KDreamelf/collaborative-editor/internal/protocol"
)

// receiveOrdinaryEdit 处理远端普通「改这行」。
// 这一行还在写，且正文不同：只返回本人主张包，不改正式行。
// 没在写：直接改正式行。已有主张留着。
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
	if len(sub.Content) == 1 && sub.Content[0] == formal {
		return nil, nil
	}
	// 没在写这一行：普通编辑直接改正式行，已有主张留着。
	if err := doc.ApplyPlainSubmit(sub.PersonID, lineID, model.ActionEdit, append([]string(nil), sub.Content...), lineIDs); err != nil {
		return nil, err
	}
	return nil, nil
}
