package main

import (
	"fmt"

	"github.com/KDreamelf/collaborative-editor/internal/document"
	"github.com/KDreamelf/collaborative-editor/internal/model"
	"github.com/KDreamelf/collaborative-editor/internal/protocol"
)

// rebuildFromChainAndClaims 用服务器当前共享链 + 显式主张重建本端 Doc。
// 纯函数：不改入参 local / claimIDs；入场只读当前行链与显式主张。
// 仅本人 CC 不进争议；同槽位有他人主张才回填本人候选（稳定 ID + 完整 Content）。
// 槽位有他人 CC、Disputes 尚无本人时，优先 local.EditBelief / InsertBelief；
// local 缺行或无有效内容才由服务器链生成。插入提升后可能覆盖本人 Content，故回填后再 Store 一次。
func rebuildFromChainAndClaims(self string, local *document.Doc, boot protocol.Bootstrap, claimIDs map[string]model.ID) (*document.Doc, error) {
	doc, err := document.Load(boot.Base.Article, boot.Base.Lines, nil)
	if err != nil {
		return nil, err
	}

	ownBySlot := map[string]model.Dispute{}
	if local != nil {
		lv, err := local.View()
		if err != nil {
			return nil, err
		}
		collectOwnClaims(self, lv.Disputes, ownBySlot)
	}
	collectOwnClaims(self, boot.Disputes, ownBySlot)

	foreignSlots := map[string]bool{}
	for _, c := range boot.Disputes {
		if c.Person == "" || c.Person == self {
			continue
		}
		foreignSlots[claimSlotKey(c.RealLine, c.Action)] = true
	}

	for _, foreign := range boot.Disputes {
		if foreign.Person == "" || foreign.Person == self {
			continue
		}
		slot := claimSlotKey(foreign.RealLine, foreign.Action)
		ownID := resolveOwnClaimID(claimIDs, ownBySlot, foreign.RealLine, foreign.Action)

		if _, ok := ownBySlot[slot]; !ok {
			if belief, ok := ownBeliefFromLocal(local, self, foreign, ownID); ok {
				ownBySlot[slot] = belief
				if ownID.IsZero() {
					ownID = belief.ID
				}
			}
		}

		restored := false
		if own, ok := ownBySlot[slot]; ok && foreignSlots[slot] {
			if !ownID.IsZero() {
				own.ID = ownID
			}
			ownBySlot[slot] = own
			if err := doc.StoreExplicitClaim(own); err != nil {
				return nil, err
			}
			restored = true
		}

		if ownID.IsZero() {
			ownID = model.NewID()
		}
		switch {
		case foreign.Action == model.ActionEdit:
			if err := doc.ReceiveForeignEditClaim(self, foreign, ownID); err != nil {
				return nil, err
			}
		case foreign.Action == model.ActionDelete:
			if err := doc.ReceiveForeignDeleteClaim(self, foreign, ownID); err != nil {
				return nil, err
			}
		case model.IsInsertAction(foreign.Action):
			if err := doc.ReceiveForeignInsertClaim(self, foreign, ownID); err != nil {
				return nil, err
			}
		default:
			return nil, fmt.Errorf("不支持的主张做法 %q", foreign.Action)
		}

		// 插入提升 upsert 可能改写本人 Content；以 ownBySlot 为准再落一次。
		if restored {
			own := ownBySlot[slot]
			own.ID = ownID
			if err := doc.StoreExplicitClaim(own); err != nil {
				return nil, err
			}
		}
	}

	return doc, nil
}

// ownBeliefFromLocal 从本机 Doc 取当前完整主张。缺行 / 无有效内容 / 出错 → false，交由服务器链生成。
func ownBeliefFromLocal(local *document.Doc, self string, foreign model.Dispute, ownID model.ID) (model.Dispute, bool) {
	if local == nil || self == "" {
		return model.Dispute{}, false
	}
	action := foreign.Action
	var content []string
	var err error
	switch {
	case action == model.ActionEdit || action == model.ActionDelete:
		content, err = local.EditBelief(self, foreign.RealLine)
		action = model.ActionEdit
	case model.IsInsertAction(action):
		action = model.InsertAction(action)
		content, err = local.InsertBelief(self, foreign.RealLine, action)
	default:
		return model.Dispute{}, false
	}
	if err != nil || len(content) == 0 {
		return model.Dispute{}, false
	}
	id := ownID
	if id.IsZero() {
		id = model.NewID()
	}
	return model.Dispute{
		ID:        id,
		RealLine:  foreign.RealLine,
		Action:    action,
		Person:    self,
		Content:   append([]string(nil), content...),
		Followers: []string{},
	}, true
}

func collectOwnClaims(self string, claims []model.Dispute, into map[string]model.Dispute) {
	for _, c := range claims {
		if c.Person != self {
			continue
		}
		key := claimSlotKey(c.RealLine, c.Action)
		if _, ok := into[key]; ok {
			continue
		}
		into[key] = model.Dispute{
			ID:        c.ID,
			RealLine:  c.RealLine,
			Action:    c.Action,
			Person:    c.Person,
			Content:   append([]string(nil), c.Content...),
			BaseIDs:   append([]model.ID(nil), c.BaseIDs...),
			Followers: append([]string(nil), c.Followers...),
			Pending:   append([]model.PendingConfirm(nil), c.Pending...),
		}
	}
}

func claimSlotKey(line model.ID, action string) string {
	if model.IsInsertAction(action) {
		action = model.InsertAction(action)
	} else if action == model.ActionDelete {
		action = model.ActionEdit
	}
	return line.Hex() + "\x00" + action
}

func resolveOwnClaimID(claimIDs map[string]model.ID, ownBySlot map[string]model.Dispute, line model.ID, action string) model.ID {
	if model.IsInsertAction(action) {
		action = model.InsertAction(action)
	} else if action == model.ActionDelete {
		action = model.ActionEdit
	}
	key := line.Hex() + "\x00" + action
	if claimIDs != nil {
		if id, ok := claimIDs[key]; ok && !id.IsZero() {
			return id
		}
	}
	if own, ok := ownBySlot[claimSlotKey(line, action)]; ok && !own.ID.IsZero() {
		return own.ID
	}
	return model.ID{}
}
