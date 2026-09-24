package document

import (
	"github.com/KDreamelf/collaborative-editor/internal/model"
)

// StoreExplicitClaim 服务器侧只存抄送者原始 Dispute：按 Claim.ID 幂等 upsert。
// 不造本人第二份、不改正式链/live/insertHistory，不走 SubmitWith/openDispute/tryResolve。
// 重复 CC 只更新 Content/BaseIDs；同槽允许 Edit↔Delete 改 Action。
// Followers 与 d.pending 仅首次入场可写入，之后由 Follow/FollowAnswer 改。
func (d *Doc) StoreExplicitClaim(claim model.Dispute) error {
	if claim.ID.IsZero() || claim.RealLine.IsZero() {
		return ErrBroken
	}
	if claim.Person == "" {
		return ErrPerson
	}
	switch claim.Action {
	case model.ActionEdit:
		if len(claim.Content) == 0 {
			return ErrContent
		}
	case model.ActionInsert, model.ActionInsertBefore, model.ActionDelete:
		// 空 Content：插入=本人未插入；删行合法。
	default:
		return ErrAction
	}
	if d.lines[claim.RealLine] == nil {
		return ErrLine
	}

	existing := d.disputes[claim.ID]
	if existing != nil {
		if existing.Person != claim.Person || existing.RealLine != claim.RealLine {
			return ErrBroken
		}
		if existing.Action != claim.Action && !sameDisputeSlot(existing.Action, claim.Action) {
			return ErrBroken
		}
	} else if other := d.byPerson(claim.RealLine, claim.Action, claim.Person); other != nil {
		// 同一人同真实行同槽位已有不同 ID → 拒绝双本人候选。
		return ErrBroken
	}

	content := append([]string(nil), claim.Content...)
	baseIDs := append([]model.ID{}, claim.BaseIDs...)

	item := existing
	if item == nil {
		item = &model.Dispute{
			ID:        claim.ID,
			RealLine:  claim.RealLine,
			Action:    claim.Action,
			Person:    claim.Person,
			Followers: d.seedFollowMeta(claim.ID, claim.Followers, claim.Pending),
		}
		d.disputes[claim.ID] = item
	}
	item.Action = claim.Action
	item.Content = content
	item.BaseIDs = baseIDs
	item.Pending = nil // 与 Load 一致：待确认落在 d.pending
	return nil
}

// seedFollowMeta 首次见到 Claim.ID：复制 Followers，Pending 写入 d.pending。
func (d *Doc) seedFollowMeta(disputeID model.ID, followers []string, pending []model.PendingConfirm) []string {
	for _, p := range pending {
		d.pending = append(d.pending, followPend{
			from: p.From, to: p.To, dispute: disputeID, ts: p.ClientTs,
		})
	}
	return append([]string{}, followers...)
}
