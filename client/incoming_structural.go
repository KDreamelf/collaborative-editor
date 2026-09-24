package main

import (
	"fmt"
	"slices"

	"github.com/KDreamelf/collaborative-editor/internal/document"
	"github.com/KDreamelf/collaborative-editor/internal/model"
	"github.com/KDreamelf/collaborative-editor/internal/protocol"
)

// structuralOwnCCLocked 只在普通结构操作触及本机注意力且结果异文时声明本人的写法。
// 被物理删除的行以存活邻行作插入锚，避免向服务器申报不存在的 RealLine。
func (a *App) structuralOwnCCLocked(op protocol.Op) (*protocol.DisputeCC, error) {
	v, err := a.doc.View()
	if err != nil {
		return nil, err
	}
	index := func(raw string) int {
		for i, ln := range v.Lines {
			if ln.ID.Hex() == raw {
				return i
			}
		}
		return -1
	}
	var claim model.Dispute
	var target string
	switch op.Kind {
	case protocol.TypeDelete:
		if op.Delete == nil {
			return nil, fmt.Errorf("缺少删行内容")
		}
		target = op.Delete.PersonID
		if !a.lineAttendingLocked(op.Delete.LineID) {
			return nil, nil
		}
		i := index(op.Delete.LineID)
		if i < 0 {
			return nil, document.ErrLine
		}
		claim.Content = []string{v.Lines[i].Content}
		switch {
		case len(v.Lines) == 1:
			if v.Lines[i].Content == "" {
				return nil, nil
			}
			claim.RealLine, claim.Action = v.Lines[i].ID, model.ActionEdit
		case i > 0:
			claim.RealLine, claim.Action = v.Lines[i-1].ID, model.ActionInsert
		default:
			claim.RealLine, claim.Action = v.Lines[1].ID, model.ActionInsertBefore
		}
		if len(v.Lines) > 1 {
			claim.BaseIDs = []model.ID{v.Lines[i].ID}
		}
	case protocol.TypeMerge:
		if op.Merge == nil {
			return nil, fmt.Errorf("缺少合并内容")
		}
		target = op.Merge.PersonID
		i := index(op.Merge.LineID)
		if i < 0 {
			return nil, document.ErrLine
		}
		if i == 0 || (!a.lineAttendingLocked(op.Merge.LineID) && !a.lineAttendingLocked(v.Lines[i-1].ID.Hex())) {
			return nil, nil
		}
		claim.RealLine, claim.Action = v.Lines[i-1].ID, model.ActionEdit
		claim.Content = []string{v.Lines[i-1].Content, v.Lines[i].Content}
	case protocol.TypeSpanEdit:
		if op.SpanEdit == nil {
			return nil, fmt.Errorf("缺少整段修改内容")
		}
		s := op.SpanEdit
		target = s.PersonID
		hit := false
		for _, raw := range s.BaseIDs {
			if a.lineAttendingLocked(raw) {
				hit = true
				break
			}
		}
		if !hit {
			return nil, nil
		}
		if len(s.BaseIDs) < 2 {
			return nil, document.ErrSpanBase
		}
		first := index(s.BaseIDs[0])
		if first < 0 || first+len(s.BaseIDs) > len(v.Lines) {
			return nil, document.ErrSpanBase
		}
		claim.RealLine, claim.Action = v.Lines[first].ID, model.ActionEdit
		claim.Content = make([]string, len(s.BaseIDs))
		for j, raw := range s.BaseIDs {
			if v.Lines[first+j].ID.Hex() != raw {
				return nil, document.ErrSpanBase
			}
			claim.Content[j] = v.Lines[first+j].Content
		}
		if slices.Equal(claim.Content, s.Replacement) {
			return nil, nil
		}
	default:
		return nil, nil
	}
	if target == "" || target == a.personID {
		return nil, nil
	}
	claim.ID = a.stableClaimIDLocked(claim.RealLine.Hex(), claim.Action)
	claim.Person = a.personID
	return &protocol.DisputeCC{TargetPersonID: target, Claim: claim}, nil
}
