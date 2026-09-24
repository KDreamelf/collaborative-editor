package main

import (
	"fmt"
	"slices"

	"github.com/KDreamelf/collaborative-editor/internal/document"
	"github.com/KDreamelf/collaborative-editor/internal/model"
	"github.com/KDreamelf/collaborative-editor/internal/protocol"
)

// ownEditCC 在收到远端普通「改这行」时，若落在本人活跃注意力行且与本人主张不同，
// 构造要发给对方并抄送服务器的本人主张包。不改 Doc，不进本地争议。
// receiveOrdinaryEdit 先调它：有 CC 则直接返回，不再整合正文。
func ownEditCC(doc *document.Doc, self, attentionLine string, active bool, incoming protocol.Op, claimID model.ID) (*protocol.DisputeCC, error) {
	if incoming.Kind != protocol.TypeSubmit || incoming.Submit == nil || incoming.Submit.Action != model.ActionEdit {
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

	// 单行以正式正文为准：后来的普通编辑盖过旧主张。多行粘贴仍用整段信念。
	belief, err := doc.EditBelief(self, lineID)
	if err != nil {
		return nil, fmt.Errorf("本地缺少行 %s", sub.LineID)
	}
	own := belief
	if len(belief) == 1 {
		view, err := doc.View()
		if err != nil {
			return nil, err
		}
		foundLine := false
		for _, ln := range view.Lines {
			if ln.ID == lineID {
				own = []string{ln.Content}
				foundLine = true
				break
			}
		}
		if !foundLine {
			return nil, fmt.Errorf("本地缺少行 %s", sub.LineID)
		}
	}
	if slices.Equal(sub.Content, own) {
		return nil, nil
	}

	return &protocol.DisputeCC{
		TargetPersonID: sub.PersonID,
		Claim: model.Dispute{
			ID:       claimID,
			RealLine: lineID,
			Action:   model.ActionEdit,
			Person:   self,
			Content:  append([]string(nil), own...),
		},
	}, nil
}
