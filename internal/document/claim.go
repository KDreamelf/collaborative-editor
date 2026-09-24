package document

import (
	"errors"
	"slices"

	"github.com/KDreamelf/collaborative-editor/internal/model"
)

var (
	ErrFollowSelf = errors.New("不能追随自己")
	ErrNoDispute  = errors.New("没有这场争议")
	ErrNotYours   = errors.New("这不是你的主张，不能替你点头")
)

const (
	FollowPending = "pending"
	FollowApplied = "applied"
	FollowLost    = "lost"
	FollowDenied  = "denied"
)

// FollowOutcome 是追随这一步的结果。Peer 是另一个需要知道结果的人。
type FollowOutcome struct {
	Status     string
	PeerID     string
	PeerStatus string
	DisputeID  model.ID
}

// Submit 收下一个人的一份主张。零扩展，兼容旧调用。
func (d *Doc) Submit(person string, line model.ID, action string, content []string) error {
	return d.SubmitWith(person, line, action, content, SubmitOpts{})
}

// SubmitWith 与 Submit 相同，另带 AfterSeen / BeforeSeen / BaseContent / LineIDs / WholeClaim。
// AfterSeen 仅「插在后面」：与锚点当前后继相同则同步堆叠；不同则未同步转争议。
// BeforeSeen 仅「插在前面」：与锚点当前前驱相同则同步堆叠；不同则未同步转争议。
// BaseContent 仅「改这行」：与当前正式内容相同则已同步续写；落后且结果不同则开争议。
// WholeClaim：false=普通编辑保留尾；true=整份候选替换。
// 失败路径（无效基准、无法提升）不得 detachFollow。
func (d *Doc) SubmitWith(person string, line model.ID, action string, content []string, opts SubmitOpts) error {
	if action != model.ActionEdit && !model.IsInsertAction(action) && action != model.ActionDelete {
		return ErrAction
	}
	if _, err := d.line(line); err != nil {
		return err
	}
	// 删除主张一律走 DeleteIfIdle；detachFollow 在其成功路径内做，失败无副作用。
	if action == model.ActionDelete {
		return d.DeleteIfIdle(person, line, nil)
	}
	if len(content) == 0 {
		return ErrContent
	}
	content = append([]string(nil), content...)
	if err := validateLineIDs(action, content, opts.LineIDs); err != nil {
		return err
	}
	// 重发：行 ID 已在链上则无副作用确认，不覆盖后来的 live/正文。
	if model.IsInsertAction(action) && len(opts.LineIDs) > 0 && d.alreadyHaveIDs(opts.LineIDs) {
		return nil
	}
	if action == model.ActionEdit && len(content) > 1 && len(opts.LineIDs) > 0 && d.alreadyHaveIDs(opts.LineIDs) {
		return nil
	}
	// 冲突 ID 在任何追随/归档/写链之前拒绝。
	if err := d.checkNewLineIDs(opts.LineIDs); err != nil {
		return err
	}

	// 未决跨度成员：改提整段候选；成员锚定插入一律拒（含末行，胜出后末行会删）。
	if baseIDs, baseTexts, index, ok := d.openSpanCovering(line); ok {
		if action == model.ActionEdit {
			return d.submitEditIntoOpenSpan(person, line, content, baseIDs, baseTexts, index)
		}
		if model.IsInsertAction(action) {
			return ErrSpanBlocked
		}
	}

	key := claimKey{line, action}
	if action == model.ActionInsert && opts.AfterSeen != nil {
		currentNext := d.lines[line].Next
		if *opts.AfterSeen == currentNext {
			d.detachFollow(person, line, action)
			if live := d.live[key]; live != nil && live.person != person {
				d.archiveLiveInsert(key, live)
				delete(d.live, key)
			}
			return d.submitApply(person, key, content, opts)
		}
		return d.submitStaleInsert(person, key, content, opts)
	}
	if action == model.ActionInsertBefore && opts.BeforeSeen != nil {
		currentPrev := d.lines[line].Prev
		if *opts.BeforeSeen == currentPrev {
			d.detachFollow(person, line, action)
			if live := d.live[key]; live != nil && live.person != person {
				d.archiveLiveInsert(key, live)
				delete(d.live, key)
			}
			return d.submitApply(person, key, content, opts)
		}
		return d.submitStaleInsert(person, key, content, opts)
	}

	if action == model.ActionEdit && opts.BaseContent != nil {
		// detachFollow 仅在 submitEditWithBase 成功变更路径内调用；ErrStaleEdit 等失败不得先拆追随。
		return d.submitEditWithBase(person, line, content, opts)
	}

	// 无 Seen 的并发插入：提升校验失败不得先拆追随。
	if model.IsInsertAction(action) {
		if live := d.live[key]; live != nil && live.person != person {
			if _, err := d.planPromote(key, live); err != nil {
				return err
			}
		}
	}

	d.detachFollow(person, line, action)
	return d.submitApply(person, key, content, opts)
}

// submitEditWithBase：按客户端快照正文判定已同步续写或落后争议。
// 挂起主张仍不挡 otherActive；带基准且落后时是否豁免挂起，待产品确认（ponytail: 易改点）。
// 失败返回前不得 detachFollow / 改正文 / 改争议 / 改 live。
func (d *Doc) submitEditWithBase(person string, line model.ID, content []string, opts SubmitOpts) error {
	key := claimKey{line, model.ActionEdit}
	base := *opts.BaseContent
	ln := d.lines[line]
	current := ln.Content

	if otherActive(d.group(line, model.ActionEdit), person, d.suspended) {
		d.detachFollow(person, line, model.ActionEdit)
		d.upsert(person, line, model.ActionEdit, content, false)
		return nil
	}
	if item := d.byPerson(line, model.ActionEdit, person); item != nil && d.live[key] == nil {
		d.detachFollow(person, line, model.ActionEdit)
		delete(d.suspended, item.ID)
		item.Action = model.ActionEdit
		item.Content = content
		docs := d.group(line, model.ActionEdit)
		if len(d.standers(docs)) == 1 && len(docs) == 1 {
			saved := append([]string(nil), item.Content...)
			delete(d.disputes, item.ID)
			return d.writeThrough(person, key, saved, opts)
		}
		return nil
	}
	if live := d.live[key]; live != nil && live.person == person {
		d.detachFollow(person, line, model.ActionEdit)
		return d.updateLive(key, live, content, opts)
	}

	if base == current {
		d.detachFollow(person, line, model.ActionEdit)
		// 已同步续写：丢掉他人 live，不 revert（正文已是共同可见态）。
		if live := d.live[key]; live != nil && live.person != person {
			delete(d.live, key)
			d.clearRecentEdit(line)
		}
		return d.writeThrough(person, key, content, opts)
	}

	// 基准落后：提交结果与当前正式相同则不新开争议。
	if len(content) == 1 && content[0] == current {
		return nil
	}
	return d.openStaleEditDispute(person, line, content, base)
}

// openStaleEditDispute：保留最新作者与提交者各一份，正式行回到共同基准。
// live.before == base：直接共同基准，走 revert。
// live.before != base：仅普通可恢复单行允许多代基准（正式行回提交者 base）；多行 splice 等仍 ErrStaleEdit。
func (d *Doc) openStaleEditDispute(person string, line model.ID, content []string, base string) error {
	key := claimKey{line, model.ActionEdit}
	live := d.live[key]
	if live != nil {
		if live.before != base {
			if err := d.checkMultiGenStaleEdit(live, line); err != nil {
				return err
			}
			d.detachFollow(person, line, model.ActionEdit)
			ln := d.lines[line]
			ln.Content = base
			d.clearRecentEdit(line)
			d.clearEditOrigin([]model.ID{line})
			delete(d.live, key)
			d.upsert(live.person, line, model.ActionEdit, append([]string(nil), live.content...), false)
			d.upsert(person, line, model.ActionEdit, content, false)
			return nil
		}
		if err := d.revert(key, live); err != nil {
			return err
		}
		d.detachFollow(person, line, model.ActionEdit)
		delete(d.live, key)
		d.upsert(live.person, line, model.ActionEdit, append([]string(nil), live.content...), false)
		d.upsert(person, line, model.ActionEdit, content, false)
		return nil
	}

	d.detachFollow(person, line, model.ActionEdit)
	ln := d.lines[line]
	current := ln.Content
	ln.Content = base
	d.clearRecentEdit(line)
	d.upsert(CurrentBodyPerson, line, model.ActionEdit, []string{current}, false)
	d.upsert(person, line, model.ActionEdit, content, false)
	return nil
}

// checkMultiGenStaleEdit：只读。多代落后仅普通可恢复单行（无 splice 附属行/子主张）；失败零变化。
func (d *Doc) checkMultiGenStaleEdit(live *liveClaim, line model.ID) error {
	if !reversibleLineLive(live) || len(live.ids) > 0 {
		return ErrStaleEdit
	}
	ln := d.lines[line]
	if ln == nil || ln.Content != live.content[0] {
		return ErrStaleEdit
	}
	return nil
}

func (d *Doc) submitApply(person string, key claimKey, content []string, opts SubmitOpts) error {
	line, action := key.line, key.action
	if otherActive(d.group(line, action), person, d.suspended) {
		d.upsert(person, line, action, content, false)
		return nil
	}
	if live := d.live[key]; live != nil && live.person != person {
		return d.openDispute(key, live, person, content)
	}
	if item := d.byPerson(line, action, person); item != nil && d.live[key] == nil {
		delete(d.suspended, item.ID)
		item.Action = action
		item.Content = content
		docs := d.group(line, action)
		if len(d.standers(docs)) == 1 && len(docs) == 1 {
			saved := append([]string(nil), item.Content...)
			delete(d.disputes, item.ID)
			return d.writeThrough(person, key, saved, opts)
		}
		return nil
	}
	if live := d.live[key]; live != nil && live.person == person {
		return d.updateLive(key, live, content, opts)
	}
	// Load 后已入链插入只在 history：同人再改仍收回该段再写。
	if model.IsInsertAction(action) {
		if claim := d.takeHistoryInsert(key, person); claim != nil {
			d.live[key] = claim
			return d.updateLive(key, claim, content, opts)
		}
	}
	return d.writeThrough(person, key, content, opts)
}

// takeHistoryInsert：history 最新段仍贴锚点（后插看 Next，前插看 Prev）且属此人，弹出供 updateLive。
func (d *Doc) takeHistoryInsert(key claimKey, person string) *liveClaim {
	hist := d.insertHistory[key]
	if len(hist) == 0 {
		return nil
	}
	last := hist[len(hist)-1]
	if last == nil || last.person != person || !last.spliced || len(last.ids) == 0 {
		return nil
	}
	anchor := d.lines[key.line]
	if anchor == nil {
		return nil
	}
	if key.action == model.ActionInsertBefore {
		tail := last.ids[len(last.ids)-1]
		if d.lines[tail] == nil || anchor.Prev != tail {
			return nil
		}
	} else {
		if d.lines[last.ids[0]] == nil || anchor.Next != last.ids[0] {
			return nil
		}
	}
	d.insertHistory[key] = hist[:len(hist)-1]
	if len(d.insertHistory[key]) == 0 {
		delete(d.insertHistory, key)
	}
	return last
}

// submitStaleInsert：Seen 与当前边界不符。
// 后插：锚点 Next→AfterSeen；前插：锚点 Prev→BeforeSeen。收成每人一份候选再开争议。
// 基准不在链上、跨入非本锚点本方向段、或未支持的子操作：拒绝且不动正文。
func (d *Doc) submitStaleInsert(person string, key claimKey, content []string, opts SubmitOpts) error {
	live := d.live[key]
	if live != nil && live.person == person {
		d.detachFollow(person, key.line, key.action)
		return d.updateLive(key, live, content, opts)
	}

	var seen model.ID
	switch key.action {
	case model.ActionInsertBefore:
		if opts.BeforeSeen == nil {
			return ErrStaleInsert
		}
		seen = *opts.BeforeSeen
	default:
		if opts.AfterSeen == nil {
			return ErrStaleInsert
		}
		seen = *opts.AfterSeen
	}

	segs := d.collectAnchorInserts(key)
	var scoped []*liveClaim
	var blockTexts []string
	var err error
	if key.action == model.ActionInsertBefore {
		scoped, blockTexts, err = d.scopeInsertsByBeforeSeen(key.line, seen, segs)
	} else {
		scoped, blockTexts, err = d.scopeInsertsByAfterSeen(key.line, seen, segs)
	}
	if err != nil {
		return err
	}
	// 空隙仅对向 foreign、本方向无段：按当前真实邻接写入，不造单人伪争议。
	if len(scoped) == 0 {
		d.detachFollow(person, key.line, key.action)
		return d.submitApply(person, key, content, opts)
	}
	if slices.Equal(content, blockTexts) {
		return nil
	}

	for _, seg := range scoped {
		plans, err := d.planPromote(key, seg)
		if err != nil {
			return err
		}
		// 多段子编辑提升暂不支持；单段仍走 openInsertDispute。
		if len(scoped) > 1 && len(plans) > 0 {
			return ErrPromoteUnsafe
		}
	}

	d.detachFollow(person, key.line, key.action)
	if len(scoped) == 1 {
		return d.openInsertDispute(key, scoped[0], person, content)
	}
	return d.openStackedInsertDispute(key, scoped, person, content)
}

// collectAnchorInserts：该锚点+方向已入链插入，旧→新（history 后接 live）。
func (d *Doc) collectAnchorInserts(key claimKey) []*liveClaim {
	var segs []*liveClaim
	for _, s := range d.insertHistory[key] {
		if s != nil && s.spliced && len(s.ids) > 0 {
			segs = append(segs, s)
		}
	}
	if live := d.live[key]; live != nil && live.spliced && len(live.ids) > 0 {
		segs = append(segs, live)
	}
	return segs
}

// scopeInsertsByAfterSeen：锚点 Next→AfterSeen 必须恰好等于本锚点后插若干连续段。
// 返回旧→新 scoped，以及链上正文（新段在前）。
func (d *Doc) scopeInsertsByAfterSeen(anchor model.ID, afterSeen model.ID, segs []*liveClaim) ([]*liveClaim, []string, error) {
	return d.scopeInsertsBySeen(claimKey{anchor, model.ActionInsert}, afterSeen, segs)
}

// scopeInsertsByBeforeSeen：锚点 Prev→BeforeSeen 必须恰好等于本锚点前插若干连续段。
// 返回旧→新 scoped，以及链上正文（旧段在前，阅读序）。
func (d *Doc) scopeInsertsByBeforeSeen(anchor model.ID, beforeSeen model.ID, segs []*liveClaim) ([]*liveClaim, []string, error) {
	return d.scopeInsertsBySeen(claimKey{anchor, model.ActionInsertBefore}, beforeSeen, segs)
}

// scopeInsertsBySeen：沿远离锚点方向 walk 到 seen；covered 须等于本方向连续段。
// seenBound 上反方向相邻插入可夹在历史跨度内：从 walk 滤掉后只拿本方向段比较/开争议，foreign 不 unlink。
func (d *Doc) scopeInsertsBySeen(key claimKey, seenBound model.ID, segs []*liveClaim) ([]*liveClaim, []string, error) {
	base := d.lines[key.line]
	if base == nil {
		return nil, nil, ErrLine
	}
	before := key.action == model.ActionInsertBefore
	var walk []model.ID
	var texts []string
	seen := map[model.ID]bool{}
	id := base.Next
	if before {
		id = base.Prev
	}
	for id != seenBound {
		if id.IsZero() || seen[id] {
			return nil, nil, ErrStaleInsert
		}
		ln := d.lines[id]
		if ln == nil {
			return nil, nil, ErrStaleInsert
		}
		seen[id] = true
		walk = append(walk, id)
		texts = append(texts, ln.Content)
		if before {
			id = ln.Prev
		} else {
			id = ln.Next
		}
	}
	if len(walk) == 0 {
		return nil, nil, ErrStaleInsert
	}

	foreignAction := model.ActionInsertBefore
	if before {
		foreignAction = model.ActionInsert
	}
	foreignIDs, err := d.oppositeInsertIDsAt(claimKey{seenBound, foreignAction})
	if err != nil {
		return nil, nil, err
	}
	var ownWalk []model.ID
	var ownTexts []string
	for i, wid := range walk {
		if foreignIDs[wid] {
			continue
		}
		ownWalk = append(ownWalk, wid)
		ownTexts = append(ownTexts, texts[i])
	}
	if len(ownWalk) == 0 {
		return nil, nil, nil
	}

	var covered []model.ID
	var newestFirst []*liveClaim
	for i := len(segs) - 1; i >= 0; i-- {
		seg := segs[i]
		if len(seg.ids) == 0 {
			return nil, nil, ErrStaleInsert
		}
		for j, sid := range seg.ids {
			ln := d.lines[sid]
			if ln == nil {
				return nil, nil, ErrBroken
			}
			if j > 0 && ln.Prev != seg.ids[j-1] {
				return nil, nil, ErrBroken
			}
		}
		head := d.lines[seg.ids[0]]
		if head.InsertOrigin == nil || head.InsertOrigin.Person != seg.person || head.InsertOrigin.Anchor != key.line {
			return nil, nil, ErrStaleInsert
		}
		if model.InsertAction(head.InsertOrigin.Action) != key.action {
			return nil, nil, ErrStaleInsert
		}
		if before {
			// walk 沿 Prev 是新→旧；covered 段内 ID 也要新→旧（整段反转）。
			for j := len(seg.ids) - 1; j >= 0; j-- {
				covered = append(covered, seg.ids[j])
			}
		} else {
			covered = append(covered, seg.ids...)
		}
		newestFirst = append(newestFirst, seg)
		if len(covered) >= len(ownWalk) {
			break
		}
	}
	if !slices.Equal(covered, ownWalk) {
		for _, seg := range segs {
			if _, err := d.planPromote(key, seg); err != nil {
				return nil, nil, err
			}
		}
		return nil, nil, ErrStaleInsert
	}
	oldest := newestFirst[len(newestFirst)-1]
	if before {
		if oldest.oldPrev != seenBound && !foreignIDs[oldest.oldPrev] {
			return nil, nil, ErrStaleInsert
		}
	} else if oldest.oldNext != seenBound && !foreignIDs[oldest.oldNext] {
		return nil, nil, ErrStaleInsert
	}
	scoped := make([]*liveClaim, len(newestFirst))
	for i, s := range newestFirst {
		scoped[len(newestFirst)-1-i] = s
	}
	for _, seg := range scoped {
		if err := d.checkSegmentUnlink(key.line, seg, key.action); err != nil {
			if _, perr := d.planPromote(key, seg); perr != nil {
				return nil, nil, perr
			}
			return nil, nil, err
		}
	}
	// 前插 walk/texts 是新→旧；争议正文阅读序要旧→新。
	if before {
		slices.Reverse(ownTexts)
	}
	return scoped, ownTexts, nil
}

// oppositeInsertIDsAt：seenBound 上反方向已入链插入的 ID 集合；段不完整则拒。
func (d *Doc) oppositeInsertIDsAt(foreignKey claimKey) (map[model.ID]bool, error) {
	ids := map[model.ID]bool{}
	for _, seg := range d.collectAnchorInserts(foreignKey) {
		if len(seg.ids) == 0 {
			return nil, ErrStaleInsert
		}
		for j, sid := range seg.ids {
			ln := d.lines[sid]
			if ln == nil {
				return nil, ErrBroken
			}
			if j > 0 && ln.Prev != seg.ids[j-1] {
				return nil, ErrBroken
			}
		}
		head := d.lines[seg.ids[0]]
		if head.InsertOrigin == nil || head.InsertOrigin.Person != seg.person || head.InsertOrigin.Anchor != foreignKey.line {
			return nil, ErrStaleInsert
		}
		if model.InsertAction(head.InsertOrigin.Action) != foreignKey.action {
			return nil, ErrStaleInsert
		}
		if err := d.checkSegmentUnlink(foreignKey.line, seg, foreignKey.action); err != nil {
			return nil, err
		}
		for _, sid := range seg.ids {
			ids[sid] = true
		}
	}
	return ids, nil
}

// layerInsertCandidates：按旧→新逐层叠，每人保留最后一次候选。
// before=false：新段在前（后插阅读序）；before=true：旧段在前（前插阅读序）。
func layerInsertCandidates(segs []*liveClaim, before bool) []promotePlan {
	acc := []string{}
	last := map[string][]string{}
	var order []string
	for _, seg := range segs {
		var layered []string
		if before {
			layered = append(append([]string{}, acc...), seg.content...)
		} else {
			layered = append(append([]string{}, seg.content...), acc...)
		}
		if _, ok := last[seg.person]; !ok {
			order = append(order, seg.person)
		}
		last[seg.person] = layered
		acc = layered
	}
	out := make([]promotePlan, 0, len(order))
	for _, p := range order {
		out = append(out, promotePlan{person: p, content: last[p]})
	}
	return out
}

func (d *Doc) forgetInsertSegs(key claimKey, segs []*liveClaim) {
	heads := map[model.ID]bool{}
	for _, s := range segs {
		if s != nil && len(s.ids) > 0 {
			heads[s.ids[0]] = true
		}
	}
	if live := d.live[key]; live != nil && len(live.ids) > 0 && heads[live.ids[0]] {
		delete(d.live, key)
	}
	hist := d.insertHistory[key]
	n := 0
	for _, h := range hist {
		if h != nil && len(h.ids) > 0 && heads[h.ids[0]] {
			continue
		}
		hist[n] = h
		n++
	}
	if n == 0 {
		delete(d.insertHistory, key)
	} else {
		d.insertHistory[key] = hist[:n]
	}
}

// openStackedInsertDispute：多段旧基准 → 层叠候选 + 新来者，逆序拆链。
func (d *Doc) openStackedInsertDispute(key claimKey, segs []*liveClaim, person string, content []string) error {
	cands := layerInsertCandidates(segs, key.action == model.ActionInsertBefore)
	for i := len(segs) - 1; i >= 0; i-- {
		seg := segs[i]
		for _, id := range seg.ids {
			delete(d.live, claimKey{id, model.ActionEdit})
			delete(d.live, claimKey{id, model.ActionInsert})
			delete(d.live, claimKey{id, model.ActionInsertBefore})
		}
		d.clearInsertOrigin(seg.ids)
		if err := d.unlinkSegment(seg.ids); err != nil {
			return err
		}
	}
	d.forgetInsertSegs(key, segs)
	for _, c := range cands {
		d.upsert(c.person, key.line, key.action, c.content, false)
	}
	d.upsert(person, key.line, key.action, content, false)
	d.sweepPending()
	return nil
}

func validateLineIDs(action string, content []string, ids []model.ID) error {
	if len(ids) == 0 {
		return nil
	}
	want := 0
	switch {
	case model.IsInsertAction(action):
		want = len(content)
	case action == model.ActionEdit:
		if len(content) > 1 {
			want = len(content) - 1
		}
	}
	if len(ids) != want {
		return ErrLineIDs
	}
	seen := map[model.ID]bool{}
	for _, id := range ids {
		if id.IsZero() || seen[id] {
			return ErrLineIDs
		}
		seen[id] = true
	}
	return nil
}

// SetSuspended 挂起或恢复。挂起后别人来写不拉争议。主张还在，别人仍可以接受。
// 已经接进正文的插入不再收回，否则挂起后再接受会把同一段再接一次。
func (d *Doc) SetSuspended(person string, line model.ID, action string, suspended bool) error {
	if action != model.ActionEdit && !model.IsInsertAction(action) && action != model.ActionDelete {
		return ErrAction
	}
	key := claimKey{line, action}
	if item := d.byPerson(line, action, person); item != nil {
		if suspended {
			d.suspended[item.ID] = true
			return nil
		}
		delete(d.suspended, item.ID)
		if live := d.live[key]; live != nil && live.person != person {
			return d.openDispute(key, live, person, append([]string(nil), item.Content...))
		}
		d.tryResolve(line, action)
		return nil
	}
	live := d.live[key]
	if live == nil || live.person != person || !suspended {
		return nil
	}
	if live.spliced {
		// 已入链的插入来源要留着，未同步碰撞还靠它识别。
		if model.IsInsertAction(action) {
			d.archiveLiveInsert(key, live)
		}
		delete(d.live, key)
		return nil
	}
	item := d.upsert(person, line, action, append([]string(nil), live.content...), true)
	d.suspended[item.ID] = true
	delete(d.live, key)
	if action == model.ActionEdit {
		d.clearRecentEdit(line)
	}
	return nil
}

// RequestFollow 想接受 target 这份主张。不是互追时先等对方点头。
// 互追看发出的时间戳，更早的生效。时间戳相同则按人的 ID 排序，两边结果一致。
func (d *Doc) RequestFollow(from string, target model.ID, ts int64) (FollowOutcome, error) {
	item := d.disputes[target]
	if item == nil {
		return FollowOutcome{}, ErrNoDispute
	}
	if item.Person == from {
		return FollowOutcome{}, ErrFollowSelf
	}
	out := FollowOutcome{DisputeID: target, PeerID: item.Person}
	if slices.Contains(item.Followers, from) {
		out.Status = FollowApplied
		return out, nil
	}
	if rev, i := d.findPending(item.Person, from, item.RealLine, item.Action); rev != nil {
		revFrom, revTo, revTS := rev.from, rev.to, rev.ts
		d.pending = slices.Delete(d.pending, i, i+1)
		winnerFrom, winnerTo := from, item.Person
		callerWins := true
		if earlier(revTS, revFrom, ts, from) {
			winnerFrom, winnerTo = revFrom, revTo
			callerWins = false
		}
		if err := d.applyFollow(winnerFrom, winnerTo, item.RealLine, item.Action); err != nil {
			return FollowOutcome{}, err
		}
		out.PeerID = revFrom
		if callerWins {
			out.Status = FollowApplied
			out.PeerStatus = FollowLost
		} else {
			out.Status = FollowLost
			out.PeerStatus = FollowApplied
		}
		return out, nil
	}
	if d.pendingIndex(from, item.Person, item.ID) >= 0 {
		out.Status = FollowPending
		return out, nil
	}
	d.pending = append(d.pending, followPend{from: from, to: item.Person, dispute: item.ID, ts: ts})
	out.Status = FollowPending
	return out, nil
}

// AnswerFollow 是被追随的人点头或拒绝。点头前两边都能继续改自己的。
func (d *Doc) AnswerFollow(by, from string, target model.ID, accept bool) (FollowOutcome, error) {
	item := d.disputes[target]
	if item == nil {
		return FollowOutcome{}, ErrNoDispute
	}
	if item.Person != by {
		return FollowOutcome{}, ErrNotYours
	}
	i := d.pendingIndex(from, by, target)
	if i < 0 {
		return FollowOutcome{}, ErrNoDispute
	}
	d.pending = slices.Delete(d.pending, i, i+1)
	out := FollowOutcome{DisputeID: target, PeerID: from}
	if !accept {
		out.Status = FollowDenied
		out.PeerStatus = FollowDenied
		return out, nil
	}
	if err := d.applyFollow(from, by, item.RealLine, item.Action); err != nil {
		return FollowOutcome{}, err
	}
	out.Status = FollowApplied
	out.PeerStatus = FollowApplied
	return out, nil
}

// DeleteIfIdle 删掉空行。非空行拒绝。已有编辑/删除争议、插入主张，或有人未挂起停留时，
// 追加或更新删除主张，不清空已有争议。只剩一行时清空内容，不把链拆没。
func (d *Doc) DeleteIfIdle(person string, line model.ID, holders []Presence) error {
	ln, err := d.line(line)
	if err != nil {
		return err
	}
	if ln.Content != "" {
		return ErrNotEmpty
	}
	// 未决跨度成员不能删行清主张；暂无删行候选表达 → 原子拒绝。
	if err := d.spanMemberStructuralBlock(line); err != nil {
		return err
	}
	hasBody := len(d.group(line, model.ActionEdit)) > 0
	hasInsert := d.hasInsertClaims(line)
	if held(holders, person) || hasBody || hasInsert {
		if err := d.claimDelete(person, ln, holders); err != nil {
			return err
		}
		d.detachFollow(person, line, model.ActionDelete)
		d.tryResolve(line, model.ActionDelete)
		return nil
	}
	if len(d.lines) == 1 {
		ln.Content = ""
		d.clearLineClaims(line)
		return nil
	}
	d.unlink(ln)
	d.clearLineClaims(line)
	return nil
}

// MergeUp 是行首退格：编辑前一行，把这一行接上去，再销毁这一行。
// 这一行还有未决主张时拒绝，保全内容。还有人未挂起停留时，改走删除主张。
func (d *Doc) MergeUp(person string, line model.ID, holders []Presence) error {
	ln, err := d.line(line)
	if err != nil {
		return err
	}
	if ln.Prev.IsZero() {
		return nil
	}
	// 未决跨度成员（本行或合并目标前行）结构变更会清基准 → 拒绝保全。
	if err := d.spanMemberStructuralBlock(line); err != nil {
		return err
	}
	if err := d.spanMemberStructuralBlock(ln.Prev); err != nil {
		return err
	}
	if len(d.group(line, model.ActionEdit)) > 0 || d.hasInsertDisputes(line) || d.blocksMergeLive(line, person) {
		return ErrHasClaims
	}
	if held(holders, person) {
		if err := d.claimDelete(person, ln, holders); err != nil {
			return err
		}
		d.detachFollow(person, line, model.ActionDelete)
		d.tryResolve(line, model.ActionDelete)
		return nil
	}
	prev := d.lines[ln.Prev]
	joined := prev.Content + ln.Content
	d.unlink(ln)
	d.clearLineClaims(line)
	return d.Submit(person, prev.ID, model.ActionEdit, []string{joined})
}

func (d *Doc) updateLive(key claimKey, live *liveClaim, content []string, opts SubmitOpts) error {
	if model.IsInsertAction(key.action) {
		// 同一份插入被重发时，段已经在链上，不能再接一次。
		if live.spliced {
			if len(live.ids) > 0 && d.alreadySpliced(live.ids, content) {
				live.content = content
				d.setInsertOrigin(live.ids, live.person, key.line, key.action, live.oldPrev, live.oldNext, content)
				return nil
			}
			if key.action != model.ActionInsertBefore {
				if seg, err := d.between(key.line, live.oldNext); err == nil && sameText(seg, content) {
					live.content = content
					if len(live.ids) > 0 {
						d.setInsertOrigin(live.ids, live.person, key.line, key.action, live.oldPrev, live.oldNext, content)
					}
					return nil
				}
			}
			if len(live.ids) > 0 {
				d.clearInsertOrigin(live.ids)
				if err := d.unlinkSegment(live.ids); err != nil {
					return err
				}
			} else if live.spliced {
				if err := d.revert(key, live); err != nil {
					return err
				}
			}
		}
		delete(d.live, key)
		return d.writeThrough(live.person, key, content, opts)
	}
	// WholeClaim：明确整份候选替换（跨度 / 多行缩扩）。
	if opts.WholeClaim {
		if len(live.baseIDs) >= 2 {
			return d.updateSpanLive(key, live, content, opts.LineIDs)
		}
		if live.spliced || len(content) > 1 || len(live.content) > 1 {
			return d.replaceEditLive(key, live, content, opts)
		}
		live.content = append([]string(nil), content...)
		d.lines[key.line].Content = content[0]
		return nil
	}
	// 普通正式行：只改头；多行粘贴把新增行插在头与原后继之间，保留已有尾。
	return d.extendEditLive(key, live, content, opts)
}

// extendEditLive：普通续写。不删已有尾 ID/正文；Span 保留 baseIDs，刷新 EditOrigin 全结果。
func (d *Doc) extendEditLive(key claimKey, live *liveClaim, content []string, opts SubmitOpts) error {
	ln := d.lines[key.line]
	if ln == nil {
		return ErrLine
	}
	tailIDs := append([]model.ID(nil), live.ids...)
	tailTexts := make([]string, len(tailIDs))
	for i, id := range tailIDs {
		row := d.lines[id]
		if row == nil {
			return ErrBroken
		}
		tailTexts[i] = row.Content
	}
	if !live.spliced {
		live.oldNext = ln.Next
	}
	ln.Content = content[0]
	var newIDs []model.ID
	if len(content) > 1 {
		ids, err := d.spliceIDs(key.line, content[1:], opts.LineIDs)
		if err != nil {
			return err
		}
		newIDs = ids
	}
	live.ids = append(newIDs, tailIDs...)
	live.content = append(append([]string(nil), content...), tailTexts...)
	if len(live.ids) > 0 || len(live.baseIDs) >= 2 {
		live.spliced = true
	}
	if len(live.baseIDs) >= 2 {
		resultIDs := append([]model.ID{key.line}, live.ids...)
		d.setEditOrigin(resultIDs, live.person, live.baseIDs, live.baseTexts, live.oldNext, live.content)
		return nil
	}
	if len(live.ids) > 0 {
		d.clearRecentEdit(key.line)
		return nil
	}
	d.setRecentEdit(key.line, live.person, live.before)
	return nil
}

// replaceEditLive：非跨度「改这行」多行主张整段换新内容；保 live.before。
// 段内他人改动/未决插入 → ErrPromoteUnsafe 零变化；本人单行子改可随 unlink 清掉。
func (d *Doc) replaceEditLive(key claimKey, live *liveClaim, content []string, opts SubmitOpts) error {
	if err := d.checkEditSegReplaceable(key.line, live); err != nil {
		return err
	}
	ln := d.lines[key.line]
	if ln == nil {
		return ErrLine
	}
	before := live.before
	oldNext := live.oldNext
	if live.spliced {
		if len(live.ids) > 0 {
			if err := d.unlinkSegment(live.ids); err != nil {
				return err
			}
		}
	} else {
		oldNext = ln.Next
	}

	ln.Content = content[0]
	live.content = append([]string(nil), content...)
	live.before = before
	if len(content) > 1 {
		ids, err := d.spliceIDs(key.line, content[1:], opts.LineIDs)
		if err != nil {
			return err
		}
		live.spliced = true
		live.oldNext = oldNext
		live.ids = ids
		d.clearRecentEdit(key.line)
		return nil
	}
	live.spliced = false
	live.oldNext = model.ID{}
	live.ids = nil
	d.setRecentEdit(key.line, live.person, before)
	return nil
}

// checkEditSegReplaceable：只读。多行编辑段内有他人主张或未决插入则拒，避免 unlink 吞掉。
func (d *Doc) checkEditSegReplaceable(head model.ID, live *liveClaim) error {
	if d.hasInsertClaims(head) {
		return ErrPromoteUnsafe
	}
	for _, id := range live.ids {
		if d.hasInsertClaims(id) || d.hasInsertHistory(id) {
			return ErrPromoteUnsafe
		}
		if child := d.live[claimKey{id, model.ActionEdit}]; child != nil {
			if child.person != live.person || child.spliced || len(child.content) != 1 || len(child.baseIDs) >= 2 {
				return ErrPromoteUnsafe
			}
		}
		for _, item := range d.group(id, model.ActionEdit) {
			if item.Person != live.person || item.Action != model.ActionEdit || len(item.Content) != 1 || len(item.BaseIDs) >= 2 {
				return ErrPromoteUnsafe
			}
		}
	}
	return nil
}

func (d *Doc) writeThrough(person string, key claimKey, content []string, opts SubmitOpts) error {
	ln := d.lines[key.line]
	fresh := &liveClaim{person: person, content: content}
	if key.action == model.ActionEdit {
		fresh.before = ln.Content
		ln.Content = content[0]
		if len(content) > 1 {
			oldNext := ln.Next
			ids, err := d.spliceIDs(key.line, content[1:], opts.LineIDs)
			if err != nil {
				ln.Content = fresh.before
				return err
			}
			fresh.spliced = true
			fresh.oldNext = oldNext
			fresh.ids = ids
			d.clearRecentEdit(key.line)
		} else {
			d.setRecentEdit(key.line, person, fresh.before)
		}
	} else if key.action == model.ActionInsertBefore {
		oldPrev := ln.Prev
		ids, err := d.spliceBetween(oldPrev, key.line, content, opts.LineIDs)
		if err != nil {
			return err
		}
		fresh.spliced = true
		fresh.oldPrev = oldPrev
		fresh.ids = ids
		d.setInsertOrigin(ids, person, key.line, model.ActionInsertBefore, oldPrev, model.ID{}, content)
	} else {
		oldNext := ln.Next
		ids, err := d.spliceIDs(key.line, content, opts.LineIDs)
		if err != nil {
			return err
		}
		fresh.spliced = true
		fresh.oldNext = oldNext
		fresh.ids = ids
		d.setInsertOrigin(ids, person, key.line, model.ActionInsert, model.ID{}, oldNext, content)
	}
	d.live[key] = fresh
	return nil
}

func (d *Doc) openDispute(key claimKey, live *liveClaim, person string, content []string) error {
	if model.IsInsertAction(key.action) {
		if _, err := d.planPromote(key, live); err != nil {
			return err
		}
		return d.openInsertDispute(key, live, person, content)
	}
	if err := d.revert(key, live); err != nil {
		return err
	}
	delete(d.live, key)
	d.upsert(live.person, key.line, key.action, live.content, false)
	d.upsert(person, key.line, key.action, content, false)
	return nil
}

type promotePlan struct {
	person  string
	content []string
	source  *model.Dispute // 非空则原地改成锚点同方向插入，保留 ID/追随/挂起
}

// planPromote：只读检查。先扫不可提升依赖，再确认段可安全 unlink，最后收集提升方案。
// 段内他人单行改这行可提升为同锚点同方向整段插入；多行粘贴/子插入/删行等返回 ErrPromoteUnsafe。
func (d *Doc) planPromote(key claimKey, seg *liveClaim) ([]promotePlan, error) {
	if seg == nil || !seg.spliced {
		return nil, nil
	}
	anchor := key.line
	ids := seg.ids
	if len(ids) == 0 {
		if err := d.checkSegmentUnlink(anchor, seg, key.action); err != nil {
			return nil, err
		}
		between, err := d.between(anchor, seg.oldNext)
		if err != nil {
			return nil, err
		}
		for _, ln := range between {
			if d.hasUnsafeChildClaims(ln.ID) {
				return nil, ErrPromoteUnsafe
			}
		}
		return nil, nil
	}
	if len(ids) != len(seg.content) {
		return nil, ErrPromoteUnsafe
	}
	for _, id := range ids {
		if d.hasInsertClaims(id) {
			return nil, ErrPromoteUnsafe
		}
		if live := d.live[claimKey{id, model.ActionEdit}]; live != nil {
			if live.spliced || len(live.content) != 1 {
				return nil, ErrPromoteUnsafe
			}
		}
		for _, item := range d.group(id, model.ActionEdit) {
			if item.Action == model.ActionDelete || len(item.Content) != 1 {
				return nil, ErrPromoteUnsafe
			}
		}
	}
	if err := d.checkSegmentUnlink(anchor, seg, key.action); err != nil {
		return nil, err
	}

	type hit struct {
		index   int
		text    string
		dispute *model.Dispute
	}
	byPerson := map[string][]hit{}

	for i, id := range ids {
		for _, item := range d.group(id, model.ActionEdit) {
			byPerson[item.Person] = append(byPerson[item.Person], hit{i, item.Content[0], item})
		}
		if live := d.live[claimKey{id, model.ActionEdit}]; live != nil {
			byPerson[live.person] = append(byPerson[live.person], hit{i, live.content[0], nil})
		}
	}

	var plans []promotePlan
	for person, hits := range byPerson {
		base := append([]string(nil), seg.content...)
		var source *model.Dispute
		seenIdx := map[int]bool{}
		for _, h := range hits {
			if h.index < 0 || h.index >= len(base) || seenIdx[h.index] {
				return nil, ErrPromoteUnsafe
			}
			seenIdx[h.index] = true
			base[h.index] = h.text
			if h.dispute != nil {
				if source != nil && source.ID != h.dispute.ID {
					return nil, ErrPromoteUnsafe
				}
				source = h.dispute
			}
		}
		plans = append(plans, promotePlan{person: person, content: base, source: source})
	}
	return plans, nil
}

func (d *Doc) hasUnsafeChildClaims(line model.ID) bool {
	if d.hasLiveInsert(line) {
		return true
	}
	if live := d.live[claimKey{line, model.ActionEdit}]; live != nil {
		if live.spliced || len(live.content) != 1 {
			return true
		}
		return true // 无 ids 时无法安全对位提升
	}
	if d.hasInsertDisputes(line) {
		return true
	}
	for _, item := range d.group(line, model.ActionEdit) {
		if item.Action == model.ActionDelete || len(item.Content) != 1 {
			return true
		}
		return true
	}
	return false
}

func (d *Doc) openInsertDispute(key claimKey, seg *liveClaim, person string, content []string) error {
	plans, err := d.planPromote(key, seg)
	if err != nil {
		return err
	}

	authorContent := append([]string(nil), seg.content...)
	promoted := map[string]bool{}

	for _, p := range plans {
		if p.person == seg.person {
			authorContent = append([]string(nil), p.content...)
			if p.source != nil {
				delete(d.disputes, p.source.ID)
				delete(d.suspended, p.source.ID)
			}
			continue
		}
		promoted[p.person] = true
		if p.source != nil {
			p.source.RealLine = key.line
			p.source.Action = key.action
			p.source.Content = append([]string(nil), p.content...)
		} else {
			d.upsert(p.person, key.line, key.action, p.content, false)
		}
	}

	// 清段内子行 live；已提升的 dispute 已改 RealLine，unlink 时不会误删。
	for _, id := range seg.ids {
		delete(d.live, claimKey{id, model.ActionEdit})
		delete(d.live, claimKey{id, model.ActionInsert})
		delete(d.live, claimKey{id, model.ActionInsertBefore})
		for _, item := range append([]*model.Dispute(nil), d.group(id, model.ActionEdit)...) {
			if item.RealLine != id {
				continue
			}
			if item.Person == seg.person || promoted[item.Person] {
				delete(d.disputes, item.ID)
				delete(d.suspended, item.ID)
			}
		}
	}

	if err := d.revert(key, seg); err != nil {
		return err
	}
	d.forgetInsertSegs(key, []*liveClaim{seg})

	d.upsert(seg.person, key.line, key.action, authorContent, false)
	d.upsert(person, key.line, key.action, content, false)
	d.sweepPending()
	return nil
}

func (d *Doc) revert(key claimKey, live *liveClaim) error {
	if key.action == model.ActionEdit && len(live.baseIDs) >= 2 {
		return d.revertSpan(live)
	}
	ln := d.lines[key.line]
	if key.action == model.ActionEdit {
		ln.Content = live.before
		d.clearRecentEdit(key.line)
		d.clearEditOrigin([]model.ID{key.line})
	}
	if !live.spliced {
		return nil
	}
	// 只拆本段 ids，避免误伤同步堆叠在上面的别人的段。
	if len(live.ids) > 0 {
		d.clearInsertOrigin(live.ids)
		return d.unlinkSegment(live.ids)
	}
	seg, err := d.between(key.line, live.oldNext)
	if err != nil {
		return err
	}
	ln.Next = live.oldNext
	if !live.oldNext.IsZero() {
		d.lines[live.oldNext].Prev = ln.ID
	}
	for _, item := range seg {
		delete(d.lines, item.ID)
		d.clearLineClaims(item.ID)
	}
	return nil
}

func (d *Doc) between(anchor, oldNext model.ID) ([]*model.Line, error) {
	var out []*model.Line
	seen := map[model.ID]bool{}
	id := d.lines[anchor].Next
	for id != oldNext {
		if id.IsZero() || seen[id] || len(seen) > len(d.lines) {
			return nil, ErrBroken
		}
		seen[id] = true
		ln := d.lines[id]
		if ln == nil {
			return nil, ErrBroken
		}
		out = append(out, ln)
		id = ln.Next
	}
	if len(out) == 0 {
		return nil, ErrBroken
	}
	return out, nil
}

func (d *Doc) upsert(person string, line model.ID, action string, content []string, keepSuspend bool) *model.Dispute {
	if item := d.byPerson(line, action, person); item != nil {
		item.Action = action
		item.Content = content
		if !keepSuspend {
			delete(d.suspended, item.ID)
		}
		return item
	}
	item := &model.Dispute{
		ID:        model.NewID(),
		RealLine:  line,
		Action:    action,
		Person:    person,
		Content:   content,
		Followers: []string{},
	}
	d.disputes[item.ID] = item
	return item
}

// EditBelief 只读：该人对该行的 ActionEdit 主张正文。
// 优先已登记候选，其次同人 live.content，最后正式行 Content。返回切片副本。
func (d *Doc) EditBelief(person string, line model.ID) ([]string, error) {
	ln := d.lines[line]
	if ln == nil {
		return nil, ErrLine
	}
	if item := d.byPerson(line, model.ActionEdit, person); item != nil {
		return append([]string(nil), item.Content...), nil
	}
	if live := d.live[claimKey{line, model.ActionEdit}]; live != nil && live.person == person {
		return append([]string(nil), live.content...), nil
	}
	return []string{ln.Content}, nil
}

// InsertBelief 只读：本端已整合的该锚点同方向完整插入段。
// 优先本人 Dispute 候选；否则 collectAnchorInserts 按阅读序组合仍在链上的段
//（后插新→旧，前插旧→新）。完全无插入返回空。不含锚点正文、不含反方向/他锚点段。
// 嵌套布局无法无损表达时返回明确错误，不改 Doc，不假造只含最新段的主张。
func (d *Doc) InsertBelief(person string, line model.ID, action string) ([]string, error) {
	if !model.IsInsertAction(action) {
		return nil, ErrAction
	}
	if d.lines[line] == nil {
		return nil, ErrLine
	}
	if item := d.byPerson(line, action, person); item != nil {
		return append([]string(nil), item.Content...), nil
	}
	key := claimKey{line, action}
	segs := d.collectAnchorInserts(key)
	if len(segs) == 0 {
		return []string{}, nil
	}
	before := key.action == model.ActionInsertBefore
	var out []string
	for i := 0; i < len(segs); i++ {
		idx := i
		if !before {
			idx = len(segs) - 1 - i
		}
		seg := segs[idx]
		if len(seg.ids) == 0 {
			return nil, ErrPromoteUnsafe
		}
		head := d.lines[seg.ids[0]]
		if head == nil || head.InsertOrigin == nil ||
			head.InsertOrigin.Person != seg.person || head.InsertOrigin.Anchor != key.line ||
			model.InsertAction(head.InsertOrigin.Action) != key.action {
			return nil, ErrPromoteUnsafe
		}
		if err := d.checkSegmentUnlink(key.line, seg, key.action); err != nil {
			return nil, ErrPromoteUnsafe
		}
		if _, err := d.planPromote(key, seg); err != nil {
			return nil, err
		}
		for _, id := range seg.ids {
			ln := d.lines[id]
			if ln == nil {
				return nil, ErrBroken
			}
			out = append(out, ln.Content)
		}
	}
	return out, nil
}

// ReceiveForeignEditClaim 登记远端 ActionEdit 主张：按 foreign.ID 幂等 upsert，
// 若本端该 body 槽尚无本人候选则以 ownID 建 ActionEdit。不改正式链 / live / insertHistory，不 tryResolve。
func (d *Doc) ReceiveForeignEditClaim(self string, foreign model.Dispute, ownID model.ID) error {
	return d.receiveForeignBodyClaim(self, foreign, ownID, model.ActionEdit)
}

// ReceiveForeignDeleteClaim 仅在收到他人 ActionDelete 显式 CC 时登记：按 foreign.ID 幂等 upsert，
// 同槽无本人候选则以 ownID 建 ActionEdit 保留行（EditBelief）。正式链 / live 不变，不 tryResolve。
func (d *Doc) ReceiveForeignDeleteClaim(self string, foreign model.Dispute, ownID model.ID) error {
	return d.receiveForeignBodyClaim(self, foreign, ownID, model.ActionDelete)
}

// receiveForeignBodyClaim：Edit/Delete 同 body 槽共用。同 foreign.ID 允许 Edit↔Delete 切换并保留 Followers；
// 同人同槽不同 ID 拒双候选。失败零突变。
func (d *Doc) receiveForeignBodyClaim(self string, foreign model.Dispute, ownID model.ID, action string) error {
	if foreign.Action != action {
		return ErrAction
	}
	if foreign.Person == self {
		return nil
	}
	if foreign.ID.IsZero() || foreign.RealLine.IsZero() {
		return ErrBroken
	}
	if d.lines[foreign.RealLine] == nil {
		return ErrLine
	}
	if action == model.ActionDelete && len(foreign.Content) != 0 {
		return ErrBroken
	}
	if action != model.ActionEdit && action != model.ActionDelete {
		return ErrAction
	}

	existing := d.disputes[foreign.ID]
	if existing != nil {
		if existing.Person != foreign.Person || existing.RealLine != foreign.RealLine {
			return ErrBroken
		}
		if !lineBodyAction(existing.Action) {
			return ErrBroken
		}
	} else if other := d.byPerson(foreign.RealLine, action, foreign.Person); other != nil {
		return ErrBroken
	}

	own := d.byPerson(foreign.RealLine, action, self)
	needOwn := own == nil
	var ownContent []string
	if needOwn {
		if ownID.IsZero() || ownID == foreign.ID {
			return ErrBroken
		}
		if d.disputes[ownID] != nil {
			return ErrBroken
		}
		var err error
		ownContent, err = d.EditBelief(self, foreign.RealLine)
		if err != nil {
			return err
		}
	}

	content := append([]string(nil), foreign.Content...)
	baseIDs := append([]model.ID{}, foreign.BaseIDs...)
	if existing != nil {
		// 同 ID 内容 CC：保留已有 Followers 与 d.pending。
		existing.Content = content
		existing.BaseIDs = baseIDs
		existing.Action = action
		existing.Pending = nil
	} else {
		d.disputes[foreign.ID] = &model.Dispute{
			ID:        foreign.ID,
			RealLine:  foreign.RealLine,
			Action:    action,
			Person:    foreign.Person,
			Content:   content,
			BaseIDs:   baseIDs,
			Followers: d.seedFollowMeta(foreign.ID, foreign.Followers, foreign.Pending),
		}
	}
	if !needOwn {
		return nil
	}
	d.disputes[ownID] = &model.Dispute{
		ID:        ownID,
		RealLine:  foreign.RealLine,
		Action:    model.ActionEdit,
		Person:    self,
		Content:   ownContent,
		Followers: []string{},
	}
	return nil
}

// ReceiveForeignInsertClaim 收到外来插入主张包才进争议：有同锚同方向正式段则提升为层叠候选并登记外来；
// 无段只登记 foreign（不凭空加空白正式行）。失败在克隆上发生，原 Doc 零突变。
func (d *Doc) ReceiveForeignInsertClaim(self string, foreign model.Dispute, ownID model.ID) error {
	if !model.IsInsertAction(foreign.Action) {
		return ErrAction
	}
	action := model.InsertAction(foreign.Action)
	if foreign.Person == self {
		return nil
	}
	if foreign.ID.IsZero() || foreign.RealLine.IsZero() {
		return ErrBroken
	}
	line := foreign.RealLine
	if d.lines[line] == nil {
		return ErrLine
	}
	if existing := d.disputes[foreign.ID]; existing != nil {
		if existing.Person != foreign.Person || existing.RealLine != line {
			return ErrBroken
		}
	}
	if ownID == foreign.ID {
		return ErrBroken
	}
	if hit := d.disputes[ownID]; hit != nil {
		if hit.Person != self || hit.RealLine != line || model.InsertAction(hit.Action) != action {
			return ErrBroken
		}
	}

	key := claimKey{line, action}
	segs := d.collectAnchorInserts(key)
	var belief []string
	if len(segs) > 0 {
		if ownID.IsZero() {
			return ErrBroken
		}
		var err error
		belief, err = d.InsertBelief(self, line, action)
		if err != nil {
			return err
		}
		for _, seg := range segs {
			plans, err := d.planPromote(key, seg)
			if err != nil {
				return err
			}
			if len(segs) > 1 && len(plans) > 0 {
				return ErrPromoteUnsafe
			}
		}
	}

	working := d.cloneDoc()
	if err := working.applyForeignInsertClaim(self, foreign, ownID, key, segs, belief); err != nil {
		return err
	}
	if _, err := working.View(); err != nil {
		return err
	}
	*d = *working
	return nil
}

func (d *Doc) applyForeignInsertClaim(self string, foreign model.Dispute, ownID model.ID, key claimKey, segs []*liveClaim, belief []string) error {
	action := key.action
	line := key.line
	content := append([]string(nil), foreign.Content...)
	baseIDs := append([]model.ID{}, foreign.BaseIDs...)
	preexisting := d.disputes[foreign.ID]

	if len(segs) > 0 {
		// 克隆上的段指针来自 cloneLive，按原 ids 重新收集。
		segs = d.collectAnchorInserts(key)
		var err error
		if len(segs) == 1 {
			err = d.openInsertDispute(key, segs[0], foreign.Person, content)
		} else {
			err = d.openStackedInsertDispute(key, segs, foreign.Person, content)
		}
		if err != nil {
			return err
		}
		foreignItem := d.byPerson(line, action, foreign.Person)
		if foreignItem == nil {
			return ErrBroken
		}
		if err := d.rebindDisputeID(foreignItem, foreign.ID); err != nil {
			return err
		}
		foreignItem.Content = content
		foreignItem.BaseIDs = baseIDs
		foreignItem.Pending = nil
		if preexisting == nil {
			foreignItem.Followers = d.seedFollowMeta(foreign.ID, foreign.Followers, foreign.Pending)
		}

		own := d.byPerson(line, action, self)
		if own == nil {
			d.disputes[ownID] = &model.Dispute{
				ID:        ownID,
				RealLine:  line,
				Action:    action,
				Person:    self,
				Content:   append([]string(nil), belief...),
				Followers: []string{},
			}
		} else if err := d.rebindDisputeID(own, ownID); err != nil {
			return err
		}
		return nil
	}

	// 无正式段：只登记/重绑外来，不造本人空白行。
	if existing := d.disputes[foreign.ID]; existing != nil {
		existing.Content = content
		existing.BaseIDs = baseIDs
		existing.Action = action
		existing.Pending = nil
		return nil
	}
	if other := d.byPerson(line, action, foreign.Person); other != nil {
		if err := d.rebindDisputeID(other, foreign.ID); err != nil {
			return err
		}
		other.Content = content
		other.BaseIDs = baseIDs
		other.Action = action
		other.Followers = d.seedFollowMeta(foreign.ID, foreign.Followers, foreign.Pending)
		other.Pending = nil
		return nil
	}
	d.disputes[foreign.ID] = &model.Dispute{
		ID:        foreign.ID,
		RealLine:  line,
		Action:    action,
		Person:    foreign.Person,
		Content:   content,
		BaseIDs:   baseIDs,
		Followers: d.seedFollowMeta(foreign.ID, foreign.Followers, foreign.Pending),
	}
	return nil
}

func (d *Doc) rebindDisputeID(item *model.Dispute, newID model.ID) error {
	if item == nil {
		return ErrBroken
	}
	if item.ID == newID {
		return nil
	}
	if d.disputes[newID] != nil {
		return ErrBroken
	}
	old := item.ID
	delete(d.disputes, old)
	if d.suspended[old] {
		delete(d.suspended, old)
		d.suspended[newID] = true
	}
	for i := range d.pending {
		if d.pending[i].dispute == old {
			d.pending[i].dispute = newID
		}
	}
	item.ID = newID
	d.disputes[newID] = item
	return nil
}

func (d *Doc) detachFollow(person string, line model.ID, action string) {
	group := d.group(line, action)
	leader := followedLeader(group, person)
	if leader == nil {
		return
	}
	leader.Followers = removeAll(leader.Followers, person)
}

func (d *Doc) applyFollow(from, leaderPerson string, line model.ID, action string) error {
	if live := d.live[claimKey{line, action}]; live != nil && live.person == from {
		if err := d.revert(claimKey{line, action}, live); err != nil {
			return err
		}
		delete(d.live, claimKey{line, action})
	}
	group := d.group(line, action)
	var leader *model.Dispute
	for _, item := range group {
		if item.Person == leaderPerson {
			leader = item
			break
		}
	}
	if leader == nil {
		return ErrNoDispute
	}
	movers := []string{from}
	if mine := d.byPerson(line, action, from); mine != nil {
		movers = append(movers, mine.Followers...)
		mine.Followers = []string{}
	}
	for _, item := range group {
		item.Followers = removeAny(item.Followers, movers)
	}
	for _, m := range movers {
		if m != leaderPerson && !slices.Contains(leader.Followers, m) {
			leader.Followers = append(leader.Followers, m)
		}
	}
	d.tryResolve(line, action)
	return nil
}

func otherActive(docs []*model.Dispute, person string, suspended map[model.ID]bool) bool {
	for _, item := range docs {
		if item.Person == person || suspended[item.ID] {
			continue
		}
		if followedLeader(docs, item.Person) != nil {
			continue
		}
		return true
	}
	return false
}

func held(holders []Presence, self string) bool {
	for _, h := range holders {
		if h.Active && h.Person != self {
			return true
		}
	}
	return false
}

// claimDelete：本人删除主张；未挂起的停留者保留行（改这行）。不伪造两份同样空文本。
func (d *Doc) claimDelete(person string, ln *model.Line, holders []Presence) error {
	key := claimKey{ln.ID, model.ActionEdit}
	keepText := ln.Content
	if live := d.live[key]; live != nil {
		if len(live.content) > 0 {
			keepText = live.content[0]
		}
		if err := d.revert(key, live); err != nil {
			return err
		}
		delete(d.live, key)
		if live.person != person {
			d.upsert(live.person, ln.ID, model.ActionEdit, append([]string(nil), live.content...), false)
		}
	}
	d.upsert(person, ln.ID, model.ActionDelete, []string{}, false)
	for _, h := range holders {
		if !h.Active || h.Person == person {
			continue
		}
		if d.byPerson(ln.ID, model.ActionEdit, h.Person) != nil {
			continue
		}
		d.upsert(h.Person, ln.ID, model.ActionEdit, []string{keepText}, false)
	}
	return nil
}

func (d *Doc) clearLineClaims(line model.ID) {
	for id, item := range d.disputes {
		if item.RealLine == line {
			delete(d.disputes, id)
			delete(d.suspended, id)
		}
	}
	for key := range d.live {
		if key.line == line {
			delete(d.live, key)
		}
	}
	delete(d.insertHistory, claimKey{line, model.ActionInsert})
	delete(d.insertHistory, claimKey{line, model.ActionInsertBefore})
	if ln := d.lines[line]; ln != nil {
		ln.InsertOrigin = nil
		ln.RecentEdit = nil
		ln.EditOrigin = nil
	}
	d.sweepPending()
}

func (d *Doc) unlink(ln *model.Line) {
	if !ln.Prev.IsZero() {
		d.lines[ln.Prev].Next = ln.Next
	}
	if !ln.Next.IsZero() {
		d.lines[ln.Next].Prev = ln.Prev
	}
	delete(d.lines, ln.ID)
}

func removeAll(in []string, victim string) []string {
	return slices.DeleteFunc(in, func(s string) bool { return s == victim })
}

func removeAny(in []string, victims []string) []string {
	return slices.DeleteFunc(in, func(s string) bool { return slices.Contains(victims, s) })
}

func sameText(seg []*model.Line, content []string) bool {
	if len(seg) != len(content) {
		return false
	}
	for i := range seg {
		if seg[i].Content != content[i] {
			return false
		}
	}
	return true
}

func earlier(tsA int64, personA string, tsB int64, personB string) bool {
	if tsA != tsB {
		return tsA < tsB
	}
	return personA < personB
}

func (d *Doc) findPending(from, to string, line model.ID, action string) (*followPend, int) {
	for i := range d.pending {
		p := &d.pending[i]
		item := d.disputes[p.dispute]
		if p.from == from && p.to == to && item != nil && item.RealLine == line && sameDisputeSlot(item.Action, action) {
			return p, i
		}
	}
	return nil, -1
}

func (d *Doc) pendingIndex(from, to string, dispute model.ID) int {
	for i := range d.pending {
		if d.pending[i].from == from && d.pending[i].to == to && d.pending[i].dispute == dispute {
			return i
		}
	}
	return -1
}

func (d *Doc) tryResolve(line model.ID, action string) {
	docs := d.group(line, action)
	if len(docs) == 0 {
		return
	}
	stand := d.standers(docs)
	if len(stand) != 1 {
		return
	}
	winner := stand[0]
	for _, item := range docs {
		if item == winner {
			continue
		}
		if !slices.Contains(winner.Followers, item.Person) {
			return
		}
	}
	if len(docs) == 1 && d.suspended[winner.ID] && len(winner.Followers) == 0 {
		return
	}
	// 删除胜出时若还有未决插入主张或同锚点 live 插入：保留全部，暂缓决议。
	if winner.Action == model.ActionDelete && d.hasInsertClaims(line) {
		return
	}
	// 只回滚同争议槽位的 live；编辑/删除组不动独立插入 live，反之亦然。
	var liveKeys []claimKey
	for key := range d.live {
		if key.line == line && sameDisputeSlot(key.action, action) {
			liveKeys = append(liveKeys, key)
		}
	}
	for _, key := range liveKeys {
		live := d.live[key]
		if live == nil {
			continue
		}
		if err := d.revert(key, live); err != nil {
			return
		}
		delete(d.live, key)
	}
	content := append([]string(nil), winner.Content...)
	if err := d.applyWinner(winner.Person, line, winner.Action, content, winner.BaseIDs); err != nil {
		return
	}
	for _, item := range docs {
		delete(d.disputes, item.ID)
		delete(d.suspended, item.ID)
	}
	d.sweepPending()
	if model.IsInsertAction(action) {
		d.tryResolve(line, model.ActionDelete)
	}
}

func (d *Doc) applyWinner(person string, line model.ID, action string, content []string, baseIDs []model.ID) error {
	ln, err := d.line(line)
	if err != nil {
		return err
	}
	switch action {
	case model.ActionEdit:
		if len(content) == 0 {
			return ErrContent
		}
		if len(baseIDs) >= 2 {
			return d.applySpanWinner(person, baseIDs, content)
		}
		before := ln.Content
		ln.Content = content[0]
		if len(content) > 1 {
			d.clearRecentEdit(line)
			d.clearEditOrigin([]model.ID{line})
			_, err = d.splice(line, content[1:])
			return err
		}
		d.setRecentEdit(line, person, before)
		d.live[claimKey{line, model.ActionEdit}] = &liveClaim{
			person:  person,
			content: append([]string(nil), content...),
			before:  before,
		}
		return nil
	case model.ActionInsert:
		oldNext := ln.Next
		ids, err := d.splice(line, content)
		if err != nil {
			return err
		}
		d.setInsertOrigin(ids, person, line, model.ActionInsert, model.ID{}, oldNext, content)
		// 胜出段进 history，不占 live，以免挡住同锚点删除收口。
		d.archiveLiveInsert(claimKey{line, model.ActionInsert}, &liveClaim{
			person:  person,
			content: append([]string(nil), content...),
			oldNext: oldNext,
			spliced: true,
			ids:     append([]model.ID(nil), ids...),
		})
		return nil
	case model.ActionInsertBefore:
		oldPrev := ln.Prev
		ids, err := d.spliceBetween(oldPrev, line, content, nil)
		if err != nil {
			return err
		}
		d.setInsertOrigin(ids, person, line, model.ActionInsertBefore, oldPrev, model.ID{}, content)
		d.archiveLiveInsert(claimKey{line, model.ActionInsertBefore}, &liveClaim{
			person:  person,
			content: append([]string(nil), content...),
			oldPrev: oldPrev,
			spliced: true,
			ids:     append([]model.ID(nil), ids...),
		})
		return nil
	case model.ActionDelete:
		if d.hasInsertClaims(line) {
			return ErrHasClaims
		}
		if len(d.lines) == 1 {
			ln.Content = ""
			ln.RecentEdit = nil
			ln.InsertOrigin = nil
			return nil
		}
		d.unlink(ln)
		return nil
	default:
		return ErrAction
	}
}

func (d *Doc) sweepPending() {
	d.pending = slices.DeleteFunc(d.pending, func(p followPend) bool {
		return d.disputes[p.dispute] == nil
	})
}
