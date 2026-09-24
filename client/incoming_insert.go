package main

import (
	"fmt"

	"github.com/KDreamelf/collaborative-editor/internal/document"
	"github.com/KDreamelf/collaborative-editor/internal/model"
	"github.com/KDreamelf/collaborative-editor/internal/protocol"
)

// receiveOrdinaryInsert 处理远端普通插入。插入直接改正式链，不因注意力发主张包。
func receiveOrdinaryInsert(doc *document.Doc, self, attentionLine string, active bool, op protocol.Op, claimID model.ID) (*protocol.DisputeCC, error) {
	if op.Kind != protocol.TypeSubmit || op.Submit == nil || !model.IsInsertAction(op.Submit.Action) {
		return nil, nil
	}
	sub := op.Submit
	if sub.PersonID == self {
		return nil, nil
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
	found := false
	for _, ln := range v.Lines {
		if ln.ID == lineID {
			found = true
			break
		}
	}
	if !found {
		return nil, fmt.Errorf("本地缺少行 %s", sub.LineID)
	}
	if err := doc.ApplyPlainSubmit(sub.PersonID, lineID, sub.Action, append([]string(nil), sub.Content...), lineIDs); err != nil {
		return nil, err
	}
	return nil, nil
}
