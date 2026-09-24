package server

import (
	"github.com/KDreamelf/collaborative-editor/internal/document"
	"github.com/KDreamelf/collaborative-editor/internal/model"
	"github.com/KDreamelf/collaborative-editor/internal/protocol"
)

// applyNeutralOp 中立服务器把一条客户端 Op 落到 Doc。
// 调用方持 r.mu。不发 WS、不决定转发序、不碰 Mongo。
// 普通内容只走 ApplyPlain*；争议只吃客户端抄送的 DisputeCC。
func (r *room) applyNeutralOp(senderPersonID string, op protocol.Op) error {
	if op.ID == "" {
		return errString("缺少 op.id")
	}
	person, err := neutralOpPersonID(op)
	if err != nil {
		return err
	}
	if person != senderPersonID {
		return errString("personId 与发送者不符")
	}

	switch op.Kind {
	case protocol.TypeSubmit:
		lineID, err := model.ParseID(op.Submit.LineID)
		if err != nil {
			return err
		}
		lineIDs, err := parseOpLineIDs(op.Submit.LineIDs)
		if err != nil {
			return err
		}
		return r.doc.ApplyPlainSubmit(op.Submit.PersonID, lineID, op.Submit.Action, op.Submit.Content, lineIDs)

	case protocol.TypeSpanEdit:
		opts, err := protocol.ParseSpanEditOpts(op.SpanEdit)
		if err != nil {
			return err
		}
		return r.doc.ApplyPlainSpan(opts)

	case protocol.TypeDelete:
		lineID, err := model.ParseID(op.Delete.LineID)
		if err != nil {
			return err
		}
		return r.doc.ApplyPlainDelete(lineID)

	case protocol.TypeMerge:
		lineID, err := model.ParseID(op.Merge.LineID)
		if err != nil {
			return err
		}
		return r.doc.ApplyPlainMerge(lineID)

	case protocol.TypeSuspend:
		lineID, err := model.ParseID(op.Suspend.LineID)
		if err != nil {
			return err
		}
		return r.doc.ApplyPlainSuspend(op.Suspend.PersonID, lineID, op.Suspend.Action, op.Suspend.Suspended)

	case protocol.TypeDisputeCC:
		return r.doc.StoreExplicitClaim(op.DisputeCC.Claim)

	case protocol.TypeFollow:
		disputeID, err := model.ParseID(op.Follow.DisputeID)
		if err != nil {
			return err
		}
		_, err = r.doc.RequestFollow(op.Follow.PersonID, disputeID, op.Follow.ClientTs)
		return err

	case protocol.TypeFollowAnswer:
		disputeID, err := model.ParseID(op.FollowAnswer.DisputeID)
		if err != nil {
			return err
		}
		_, err = r.doc.AnswerFollow(op.FollowAnswer.PersonID, op.FollowAnswer.FromID, disputeID, op.FollowAnswer.Accept)
		return err

	default:
		return errString("未知操作: " + op.Kind)
	}
}

func neutralOpPersonID(op protocol.Op) (string, error) {
	switch op.Kind {
	case protocol.TypeSubmit:
		if op.Submit == nil {
			return "", errString("缺少 submit")
		}
		return op.Submit.PersonID, nil
	case protocol.TypeSpanEdit:
		if op.SpanEdit == nil {
			return "", errString("缺少 spanEdit")
		}
		return op.SpanEdit.PersonID, nil
	case protocol.TypeDelete:
		if op.Delete == nil {
			return "", errString("缺少 delete")
		}
		return op.Delete.PersonID, nil
	case protocol.TypeMerge:
		if op.Merge == nil {
			return "", errString("缺少 merge")
		}
		return op.Merge.PersonID, nil
	case protocol.TypeFollow:
		if op.Follow == nil {
			return "", errString("缺少 follow")
		}
		return op.Follow.PersonID, nil
	case protocol.TypeFollowAnswer:
		if op.FollowAnswer == nil {
			return "", errString("缺少 followAnswer")
		}
		return op.FollowAnswer.PersonID, nil
	case protocol.TypeSuspend:
		if op.Suspend == nil {
			return "", errString("缺少 suspend")
		}
		return op.Suspend.PersonID, nil
	case protocol.TypeDisputeCC:
		if op.DisputeCC == nil {
			return "", errString("缺少 disputeCC")
		}
		return op.DisputeCC.Claim.Person, nil
	default:
		return "", errString("未知操作: " + op.Kind)
	}
}

func parseOpLineIDs(raw []string) ([]model.ID, error) {
	if len(raw) == 0 {
		return nil, nil
	}
	out := make([]model.ID, len(raw))
	for i, s := range raw {
		id, err := model.ParseID(s)
		if err != nil || id.IsZero() {
			return nil, document.ErrLineIDs
		}
		out[i] = id
	}
	return out, nil
}
