package document

import (
	"slices"

	"github.com/KDreamelf/collaborative-editor/internal/model"
)

// SubmitSpanEdit 跨多条正式行的一次替换，一份整段「改这行」主张。
// 已同步：直接改正式链；落后且结果不同：收回先前整段、恢复共同基准、每人一份候选。
// 失败路径不得 detachFollow / 改正文 / 改争议 / 改 live。
func (d *Doc) SubmitSpanEdit(person string, opts SpanEditOpts) error {
	if err := d.validateSpanOpts(opts); err != nil {
		return err
	}
	baseIDs := append([]model.ID(nil), opts.BaseIDs...)
	baseTexts := append([]string(nil), opts.BaseTexts...)
	replacement := append([]string(nil), opts.Replacement...)
	lineIDs := append([]model.ID(nil), opts.LineIDs...)
	afterSeen := opts.AfterSeen
	first := baseIDs[0]
	key := claimKey{first, model.ActionEdit}
	ownLive := d.live[key]
	ownSpan := ownLive != nil && ownLive.person == person && len(ownLive.baseIDs) >= 2

	// 本人跨度 live 续写：旧新行可能仍在链上，不能走重发短路或冲突 ID 拒绝。
	if !ownSpan {
		if len(lineIDs) > 0 && d.alreadyHaveIDs(lineIDs) {
			return nil
		}
		if err := d.checkNewLineIDs(lineIDs); err != nil {
			return err
		}
	} else if len(lineIDs) > 0 && !sameIDs(ownLive.ids, lineIDs) {
		if err := d.checkNewLineIDs(lineIDs); err != nil {
			return err
		}
	}

	// 已有跨度 live 且基准对齐：走 stale/promote，不在此用 base 行集合误拦结果段。
	if live := d.live[key]; live != nil && len(live.baseIDs) >= 2 &&
		sameIDs(live.baseIDs, baseIDs) && sameStrings(live.baseTexts, baseTexts) {
		if afterSeen != live.oldNext {
			return ErrSpanBase
		}
		// 当前结果段（含可能的子行改动）已等于提交：保持正式链，零争议。
		if cur, err := d.textsUntil(first, afterSeen); err == nil && sameStrings(cur, replacement) {
			return nil
		}
		if live.person == person {
			d.detachFollow(person, first, model.ActionEdit)
			return d.updateSpanLive(key, live, replacement, lineIDs)
		}
		return d.openStaleSpanDispute(person, baseIDs, baseTexts, replacement)
	}

	// 插入/未决争议/不可折 live 早拒；可对位单行 live（含他人）放行，交 fold/提升争议。
	if err := d.spanBlocked(baseIDs); err != nil {
		return err
	}

	// 同锚点已有争议：必须同一跨度组；单行可垫基准提升，否则零副作用拒绝。
	promoteDocs, err := d.planEnsureSpanGroup(first, baseIDs, baseTexts)
	if err != nil {
		return err
	}

	if otherActive(d.group(first, model.ActionEdit), person, d.suspended) {
		d.applyEnsureSpanGroup(promoteDocs, baseIDs, baseTexts)
		d.detachFollow(person, first, model.ActionEdit)
		d.upsertSpan(person, first, replacement, baseIDs)
		return nil
	}
	if item := d.byPerson(first, model.ActionEdit, person); item != nil && d.live[key] == nil {
		d.applyEnsureSpanGroup(promoteDocs, baseIDs, baseTexts)
		d.detachFollow(person, first, model.ActionEdit)
		delete(d.suspended, item.ID)
		item.Action = model.ActionEdit
		item.Content = replacement
		item.BaseIDs = append([]model.ID(nil), baseIDs...)
		docs := d.group(first, model.ActionEdit)
		if len(d.standers(docs)) == 1 && len(docs) == 1 {
			saved := append([]string(nil), item.Content...)
			bases := append([]model.ID(nil), item.BaseIDs...)
			delete(d.disputes, item.ID)
			return d.writeSpan(person, bases, baseTexts, afterSeen, saved, lineIDs)
		}
		return nil
	}
	// 本人跨度 live：基准对齐已在上方早退；此处只剩基准不符，只读拒绝。
	if live := d.live[key]; live != nil && live.person == person && len(live.baseIDs) >= 2 {
		return ErrStaleEdit
	}

	if d.spanBaseMatches(baseIDs, baseTexts, afterSeen) {
		d.applyEnsureSpanGroup(promoteDocs, baseIDs, baseTexts)
		d.detachFollow(person, first, model.ActionEdit)
		if live := d.live[key]; live != nil && live.person != person {
			delete(d.live, key)
			d.clearRecentEdit(first)
			d.clearEditOrigin([]model.ID{first})
		}
		return d.writeSpan(person, baseIDs, baseTexts, afterSeen, replacement, lineIDs)
	}

	// 当前结果已等于提交：不新开争议。
	if cur, err := d.textsUntil(first, afterSeen); err == nil && sameStrings(cur, replacement) {
		return nil
	}

	// 跨度内可对位单行 live：仅本人 → 垫回再写整段；含他人 → 每人一份整段候选。
	hits, hitErr := d.planLineLivesInSpan(baseIDs, baseTexts)
	if hitErr == ErrSpanBlocked {
		return hitErr
	}
	if hitErr == nil && len(hits) > 0 {
		if err := d.checkSpanIDsContiguous(baseIDs, afterSeen); err != nil {
			return err
		}
		onlySelf := true
		for _, h := range hits {
			if h.person != person {
				onlySelf = false
				break
			}
		}
		if onlySelf {
			d.applyEnsureSpanGroup(promoteDocs, baseIDs, baseTexts)
			d.detachFollow(person, first, model.ActionEdit)
			if err := d.revertLineLiveHits(hits); err != nil {
				return err
			}
			return d.writeSpan(person, baseIDs, baseTexts, afterSeen, replacement, lineIDs)
		}
		return d.openSpanDisputeFromLineLives(person, baseIDs, baseTexts, replacement, hits)
	}

	return d.openStaleSpanDispute(person, baseIDs, baseTexts, replacement)
}

func (d *Doc) validateSpanOpts(opts SpanEditOpts) error {
	if len(opts.BaseIDs) < 2 || len(opts.BaseIDs) != len(opts.BaseTexts) {
		return ErrSpanBase
	}
	if len(opts.Replacement) == 0 {
		return ErrContent
	}
	want := 0
	if len(opts.Replacement) > 1 {
		want = len(opts.Replacement) - 1
	}
	if len(opts.LineIDs) != want {
		return ErrLineIDs
	}
	seen := map[model.ID]bool{}
	for _, id := range opts.BaseIDs {
		if id.IsZero() || seen[id] {
			return ErrSpanBase
		}
		seen[id] = true
	}
	for _, id := range opts.LineIDs {
		if id.IsZero() || seen[id] {
			return ErrLineIDs
		}
		seen[id] = true
	}
	return nil
}

// planEnsureSpanGroup：只读。同锚点争议须同一 BaseIDs；空 BaseIDs 单行「改这行」可垫 baseTexts[1:]；
// 删行、多行、不同跨度 → ErrSpanBlocked。
func (d *Doc) planEnsureSpanGroup(first model.ID, baseIDs []model.ID, baseTexts []string) ([]*model.Dispute, error) {
	docs := d.group(first, model.ActionEdit)
	if len(docs) == 0 {
		return nil, nil
	}
	var promote []*model.Dispute
	for _, item := range docs {
		if sameIDs(item.BaseIDs, baseIDs) {
			continue
		}
		if item.Action == model.ActionDelete {
			return nil, ErrSpanBlocked
		}
		if len(item.BaseIDs) > 0 {
			return nil, ErrSpanBlocked
		}
		if len(item.Content) != 1 || len(baseTexts) < 2 {
			return nil, ErrSpanBlocked
		}
		promote = append(promote, item)
	}
	return promote, nil
}

func (d *Doc) applyEnsureSpanGroup(promote []*model.Dispute, baseIDs []model.ID, baseTexts []string) {
	for _, item := range promote {
		item.Action = model.ActionEdit
		item.Content = append([]string{item.Content[0]}, baseTexts[1:]...)
		item.BaseIDs = append([]model.ID(nil), baseIDs...)
	}
}

// spanBlocked：跨度内未决插入、子争议、跨越锚点、不可折 live → 拒绝且零变化。
// 首行争议留给 ensureSpanGroup / otherActive。可对位单行 live 不拦（fold / 提升争议）。
func (d *Doc) spanBlocked(baseIDs []model.ID) error {
	inSpan := map[model.ID]bool{}
	for i, id := range baseIDs {
		inSpan[id] = true
		if _, err := d.line(id); err != nil {
			if i == 0 {
				return ErrSpanBase
			}
			continue
		}
		if d.hasLiveInsert(id) || len(d.group(id, model.ActionInsert)) > 0 {
			return ErrSpanBlocked
		}
		if d.insertHistory[id] != nil {
			return ErrSpanBlocked
		}
		if i > 0 {
			if live := d.live[claimKey{id, model.ActionEdit}]; live != nil && !reversibleLineLive(live) {
				return ErrSpanBlocked
			}
			if len(d.group(id, model.ActionEdit)) > 0 {
				return ErrSpanBlocked
			}
		}
		ln := d.lines[id]
		if ln != nil && ln.InsertOrigin != nil {
			return ErrSpanBlocked
		}
	}
	for _, ln := range d.lines {
		o := ln.InsertOrigin
		if o == nil {
			continue
		}
		for _, id := range o.LineIDs {
			if inSpan[id] {
				return ErrSpanBlocked
			}
		}
	}
	return nil
}

func reversibleLineLive(live *liveClaim) bool {
	return live != nil && !live.spliced && len(live.baseIDs) < 2 && len(live.content) == 1
}

// lineLiveHit 是跨度内一行上可对位的单行 live。
type lineLiveHit struct {
	key    claimKey
	person string
	index  int
	text   string
}

// planLineLivesInSpan：只读。每行要么已是 BaseTexts，要么是 before 对齐的可回退单行 live。
// 多人各行 hit 留给调用方按人合并；子插入/争议/多行 splice → ErrSpanBlocked。
func (d *Doc) planLineLivesInSpan(baseIDs []model.ID, baseTexts []string) ([]lineLiveHit, error) {
	if len(baseIDs) < 2 || len(baseIDs) != len(baseTexts) {
		return nil, ErrSpanBase
	}
	var hits []lineLiveHit
	for i, id := range baseIDs {
		ln, err := d.line(id)
		if err != nil {
			return nil, ErrSpanBase
		}
		if d.hasLiveInsert(id) || len(d.group(id, model.ActionInsert)) > 0 || d.insertHistory[id] != nil {
			return nil, ErrSpanBlocked
		}
		if ln.InsertOrigin != nil {
			return nil, ErrSpanBlocked
		}
		if len(d.group(id, model.ActionEdit)) > 0 {
			return nil, ErrSpanBlocked
		}
		live := d.live[claimKey{id, model.ActionEdit}]
		if live == nil {
			if ln.Content != baseTexts[i] {
				return nil, ErrStaleEdit
			}
			continue
		}
		if !reversibleLineLive(live) {
			return nil, ErrSpanBlocked
		}
		if live.before != baseTexts[i] {
			return nil, ErrStaleEdit
		}
		hits = append(hits, lineLiveHit{
			key:    claimKey{id, model.ActionEdit},
			person: live.person,
			index:  i,
			text:   live.content[0],
		})
	}
	return hits, nil
}

func lineLiveHitsToPlans(hits []lineLiveHit, baseTexts []string) []promotePlan {
	type agg struct {
		indexes map[int]string
	}
	byPerson := map[string]*agg{}
	order := []string{}
	for _, h := range hits {
		a := byPerson[h.person]
		if a == nil {
			a = &agg{indexes: map[int]string{}}
			byPerson[h.person] = a
			order = append(order, h.person)
		}
		a.indexes[h.index] = h.text
	}
	var plans []promotePlan
	for _, person := range order {
		content := append([]string(nil), baseTexts...)
		for i, text := range byPerson[person].indexes {
			if i >= 0 && i < len(content) {
				content[i] = text
			}
		}
		plans = append(plans, promotePlan{person: person, content: content})
	}
	return plans
}

func (d *Doc) revertLineLiveHits(hits []lineLiveHit) error {
	seen := map[claimKey]bool{}
	for _, h := range hits {
		if seen[h.key] {
			continue
		}
		seen[h.key] = true
		live := d.live[h.key]
		if live == nil {
			return ErrBroken
		}
		if err := d.revert(h.key, live); err != nil {
			return err
		}
		delete(d.live, h.key)
	}
	return nil
}

// openSpanDisputeFromLineLives：收回跨度内单行 live，正式链回 BaseTexts，每人一份整段候选 + 提交者。
func (d *Doc) openSpanDisputeFromLineLives(person string, baseIDs []model.ID, baseTexts []string, replacement []string, hits []lineLiveHit) error {
	first := baseIDs[0]
	plans := lineLiveHitsToPlans(hits, baseTexts)
	if err := d.revertLineLiveHits(hits); err != nil {
		return err
	}
	d.detachFollow(person, first, model.ActionEdit)
	for _, p := range plans {
		if p.person == person {
			// 提交者自己的行内 live 被垫进候选后，以本次 Replacement 为准。
			continue
		}
		d.upsertSpan(p.person, first, p.content, baseIDs)
	}
	d.upsertSpan(person, first, replacement, baseIDs)
	d.sweepPending()
	return nil
}

func (d *Doc) checkSpanIDsContiguous(baseIDs []model.ID, afterSeen model.ID) error {
	for i, id := range baseIDs {
		ln := d.lines[id]
		if ln == nil {
			return ErrSpanBase
		}
		if i > 0 && ln.Prev != baseIDs[i-1] {
			return ErrSpanBase
		}
		if i+1 < len(baseIDs) && ln.Next != baseIDs[i+1] {
			return ErrSpanBase
		}
	}
	last := d.lines[baseIDs[len(baseIDs)-1]]
	if last == nil || last.Next != afterSeen {
		return ErrSpanBase
	}
	return nil
}

func (d *Doc) spanBaseMatches(baseIDs []model.ID, baseTexts []string, afterSeen model.ID) bool {
	for i, id := range baseIDs {
		ln := d.lines[id]
		if ln == nil || ln.Content != baseTexts[i] {
			return false
		}
		if i > 0 && ln.Prev != baseIDs[i-1] {
			return false
		}
		if i+1 < len(baseIDs) && ln.Next != baseIDs[i+1] {
			return false
		}
	}
	last := d.lines[baseIDs[len(baseIDs)-1]]
	return last != nil && last.Next == afterSeen
}

func (d *Doc) textsUntil(start, until model.ID) ([]string, error) {
	var out []string
	seen := map[model.ID]bool{}
	id := start
	for id != until {
		if id.IsZero() || seen[id] || len(seen) > len(d.lines) {
			return nil, ErrBroken
		}
		seen[id] = true
		ln := d.lines[id]
		if ln == nil {
			return nil, ErrBroken
		}
		out = append(out, ln.Content)
		id = ln.Next
	}
	if len(out) == 0 {
		return nil, ErrBroken
	}
	return out, nil
}

func (d *Doc) writeSpan(person string, baseIDs []model.ID, baseTexts []string, afterSeen model.ID, replacement []string, lineIDs []model.ID) error {
	if !d.spanBaseMatches(baseIDs, baseTexts, afterSeen) {
		return ErrSpanBase
	}
	firstID := baseIDs[0]
	first := d.lines[firstID]
	if len(baseIDs) > 1 {
		if err := d.unlinkSegment(baseIDs[1:]); err != nil {
			return err
		}
	}
	first.Content = replacement[0]
	if first.Next != afterSeen {
		first.Content = baseTexts[0]
		_ = d.restoreBaseTail(firstID, baseIDs, baseTexts, afterSeen)
		return ErrBroken
	}
	var newIDs []model.ID
	if len(replacement) > 1 {
		ids, err := d.spliceIDs(firstID, replacement[1:], lineIDs)
		if err != nil {
			first.Content = baseTexts[0]
			_ = d.restoreBaseTail(firstID, baseIDs, baseTexts, afterSeen)
			return err
		}
		newIDs = ids
	}
	resultIDs := append([]model.ID{firstID}, newIDs...)
	d.setEditOrigin(resultIDs, person, baseIDs, baseTexts, afterSeen, replacement)
	d.live[claimKey{firstID, model.ActionEdit}] = &liveClaim{
		person:    person,
		content:   append([]string(nil), replacement...),
		before:    baseTexts[0],
		oldNext:   afterSeen,
		spliced:   true,
		ids:       append([]model.ID(nil), newIDs...),
		baseIDs:   append([]model.ID(nil), baseIDs...),
		baseTexts: append([]string(nil), baseTexts...),
	}
	return nil
}

func (d *Doc) updateSpanLive(key claimKey, live *liveClaim, replacement []string, lineIDs []model.ID) error {
	// 结果段有他人单行编辑时不能静默丢掉，先走提升争议。
	if plans, err := d.planSpanPromote(live); err != nil {
		return err
	} else if len(plans) > 0 {
		hasOther := false
		for _, p := range plans {
			if p.person != live.person {
				hasOther = true
				break
			}
		}
		if hasOther {
			return ErrSpanBlocked
		}
	}
	if err := d.checkRevertSpan(live); err != nil {
		return err
	}
	baseIDs := append([]model.ID(nil), live.baseIDs...)
	baseTexts := append([]string(nil), live.baseTexts...)
	afterSeen := live.oldNext
	if err := d.revertSpan(live); err != nil {
		return err
	}
	delete(d.live, key)
	return d.writeSpan(live.person, baseIDs, baseTexts, afterSeen, replacement, lineIDs)
}

func (d *Doc) openStaleSpanDispute(person string, baseIDs []model.ID, baseTexts []string, replacement []string) error {
	first := baseIDs[0]
	key := claimKey{first, model.ActionEdit}
	live := d.live[key]
	if live == nil || len(live.baseIDs) < 2 {
		return ErrStaleEdit
	}
	if !sameIDs(live.baseIDs, baseIDs) || !sameStrings(live.baseTexts, baseTexts) {
		return ErrStaleEdit
	}

	// 只读：提升方案 + 收回预检。失败则零变化。
	plans, err := d.planSpanPromote(live)
	if err != nil {
		return err
	}
	if err := d.checkRevertSpan(live); err != nil {
		return err
	}

	authorContent := append([]string(nil), live.content...)
	type pending struct {
		person  string
		content []string
	}
	var extras []pending
	promoted := map[string]bool{}

	for _, p := range plans {
		if p.person == live.person {
			authorContent = append([]string(nil), p.content...)
			if p.source != nil {
				delete(d.disputes, p.source.ID)
				delete(d.suspended, p.source.ID)
			}
			continue
		}
		promoted[p.person] = true
		if p.source != nil {
			p.source.RealLine = first
			p.source.Action = model.ActionEdit
			p.source.Content = append([]string(nil), p.content...)
			p.source.BaseIDs = append([]model.ID(nil), baseIDs...)
		} else {
			extras = append(extras, pending{p.person, append([]string(nil), p.content...)})
		}
	}

	resultIDs := append([]model.ID{first}, live.ids...)
	for _, id := range resultIDs {
		if id != first {
			delete(d.live, claimKey{id, model.ActionEdit})
			delete(d.live, claimKey{id, model.ActionInsert})
		}
		for _, item := range append([]*model.Dispute(nil), d.group(id, model.ActionEdit)...) {
			if item.RealLine != id {
				continue
			}
			if id == first {
				continue
			}
			if item.Person == live.person || promoted[item.Person] {
				delete(d.disputes, item.ID)
				delete(d.suspended, item.ID)
			}
		}
		d.clearRecentEdit(id)
	}

	if err := d.revertSpan(live); err != nil {
		return err
	}
	delete(d.live, key)
	d.detachFollow(person, first, model.ActionEdit)

	d.upsertSpan(live.person, first, authorContent, baseIDs)
	for _, e := range extras {
		d.upsertSpan(e.person, first, e.content, baseIDs)
	}
	d.upsertSpan(person, first, replacement, baseIDs)
	d.sweepPending()
	return nil
}

// planSpanPromote：只读。结果段内单行「改这行」提升为同跨度整段候选；
// 子插入/多行/删除/嵌套跨度 → ErrSpanBlocked。
func (d *Doc) planSpanPromote(seg *liveClaim) ([]promotePlan, error) {
	if seg == nil || len(seg.baseIDs) < 2 {
		return nil, ErrSpanBlocked
	}
	resultIDs := append([]model.ID{seg.baseIDs[0]}, seg.ids...)
	if len(resultIDs) != len(seg.content) {
		return nil, ErrSpanBlocked
	}
	first := seg.baseIDs[0]

	for i, id := range resultIDs {
		if d.hasLiveInsert(id) || len(d.group(id, model.ActionInsert)) > 0 {
			return nil, ErrSpanBlocked
		}
		if d.insertHistory[id] != nil {
			return nil, ErrSpanBlocked
		}
		ln := d.lines[id]
		if ln == nil {
			return nil, ErrSpanBlocked
		}
		if ln.InsertOrigin != nil {
			return nil, ErrSpanBlocked
		}
		if live := d.live[claimKey{id, model.ActionEdit}]; live != nil {
			if id == first && len(live.baseIDs) >= 2 {
				// 本跨度 live 自身
				continue
			}
			if live.spliced || len(live.content) != 1 || len(live.baseIDs) >= 2 {
				return nil, ErrSpanBlocked
			}
		}
		for _, item := range d.group(id, model.ActionEdit) {
			if item.Action == model.ActionDelete || len(item.Content) != 1 {
				return nil, ErrSpanBlocked
			}
			if len(item.BaseIDs) > 0 && !sameIDs(item.BaseIDs, seg.baseIDs) {
				return nil, ErrSpanBlocked
			}
			// 结果段子行上带 BaseIDs 的整段争议无法对位
			if id != first && len(item.BaseIDs) > 0 {
				return nil, ErrSpanBlocked
			}
			_ = i
		}
	}

	type hit struct {
		index   int
		text    string
		dispute *model.Dispute
	}
	byPerson := map[string][]hit{}
	for i, id := range resultIDs {
		if id == first {
			// 首行争议不在结果段子编辑提升里扫（跨度 live 占 live 槽）
			for _, item := range d.group(id, model.ActionEdit) {
				if item.Action == model.ActionDelete {
					return nil, ErrSpanBlocked
				}
			}
			continue
		}
		for _, item := range d.group(id, model.ActionEdit) {
			if item.Action != model.ActionEdit || len(item.Content) != 1 {
				return nil, ErrSpanBlocked
			}
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
				return nil, ErrSpanBlocked
			}
			seenIdx[h.index] = true
			base[h.index] = h.text
			if h.dispute != nil {
				if source != nil && source.ID != h.dispute.ID {
					return nil, ErrSpanBlocked
				}
				source = h.dispute
			}
		}
		plans = append(plans, promotePlan{person: person, content: base, source: source})
	}
	return plans, nil
}

// checkRevertSpan：收回前只读预检——结果段指针、后继、恢复 ID 不冲突。
func (d *Doc) checkRevertSpan(live *liveClaim) error {
	if live == nil || len(live.baseIDs) < 2 || len(live.baseIDs) != len(live.baseTexts) {
		return ErrBroken
	}
	firstID := live.baseIDs[0]
	first := d.lines[firstID]
	if first == nil {
		return ErrBroken
	}
	resultIDs := append([]model.ID{firstID}, live.ids...)
	if len(resultIDs) != len(live.content) {
		return ErrBroken
	}
	for i, id := range resultIDs {
		ln := d.lines[id]
		if ln == nil {
			return ErrBroken
		}
		if i > 0 && ln.Prev != resultIDs[i-1] {
			return ErrBroken
		}
		if i+1 < len(resultIDs) && ln.Next != resultIDs[i+1] {
			return ErrBroken
		}
	}
	last := d.lines[resultIDs[len(resultIDs)-1]]
	if last.Next != live.oldNext {
		return ErrBroken
	}
	if !live.oldNext.IsZero() {
		n := d.lines[live.oldNext]
		if n == nil || n.Prev != last.ID {
			return ErrBroken
		}
	}
	for _, id := range live.baseIDs[1:] {
		if d.lines[id] != nil {
			return ErrBroken
		}
	}
	return nil
}

func (d *Doc) revertSpan(live *liveClaim) error {
	if err := d.checkRevertSpan(live); err != nil {
		return err
	}
	firstID := live.baseIDs[0]
	first := d.lines[firstID]
	resultIDs := append([]model.ID{firstID}, live.ids...)
	d.clearEditOrigin(resultIDs)
	d.clearRecentEdit(firstID)
	if len(live.ids) > 0 {
		if err := d.unlinkSegment(live.ids); err != nil {
			return err
		}
	}
	first.Content = live.baseTexts[0]
	return d.restoreBaseTail(firstID, live.baseIDs, live.baseTexts, live.oldNext)
}

// restoreBaseTail：把 baseIDs[1:] 按原 ID/正文接回 first 与 oldNext 之间。
func (d *Doc) restoreBaseTail(firstID model.ID, baseIDs []model.ID, baseTexts []string, oldNext model.ID) error {
	first := d.lines[firstID]
	if first == nil || len(baseIDs) < 2 || len(baseIDs) != len(baseTexts) {
		return ErrBroken
	}
	if first.Next != oldNext {
		return ErrBroken
	}
	for i := 1; i < len(baseIDs); i++ {
		if d.lines[baseIDs[i]] != nil {
			return ErrBroken
		}
	}
	prev := firstID
	for i := 1; i < len(baseIDs); i++ {
		id := baseIDs[i]
		ln := &model.Line{ID: id, Prev: prev, Content: baseTexts[i]}
		d.lines[id] = ln
		d.lines[prev].Next = id
		prev = id
	}
	d.lines[prev].Next = oldNext
	if !oldNext.IsZero() {
		n := d.lines[oldNext]
		if n == nil {
			return ErrBroken
		}
		n.Prev = prev
	}
	return nil
}

func (d *Doc) upsertSpan(person string, line model.ID, content []string, baseIDs []model.ID) *model.Dispute {
	item := d.upsert(person, line, model.ActionEdit, content, false)
	item.BaseIDs = append([]model.ID(nil), baseIDs...)
	return item
}

func (d *Doc) applySpanWinner(person string, baseIDs []model.ID, content []string) error {
	if len(baseIDs) < 2 || len(content) == 0 {
		return ErrContent
	}
	firstID := baseIDs[0]
	if d.lines[firstID] == nil {
		return ErrLine
	}
	last := d.lines[baseIDs[len(baseIDs)-1]]
	if last == nil {
		return ErrBroken
	}
	for i, id := range baseIDs {
		ln := d.lines[id]
		if ln == nil {
			return ErrBroken
		}
		if i > 0 && ln.Prev != baseIDs[i-1] {
			return ErrBroken
		}
		if i+1 < len(baseIDs) && ln.Next != baseIDs[i+1] {
			return ErrBroken
		}
	}
	afterSeen := last.Next
	baseTexts := make([]string, len(baseIDs))
	for i, id := range baseIDs {
		baseTexts[i] = d.lines[id].Content
	}
	return d.writeSpan(person, baseIDs, baseTexts, afterSeen, content, nil)
}

func sameIDs(a, b []model.ID) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func sameStrings(a, b []string) bool {
	return slices.Equal(a, b)
}
