package document

import (
	"github.com/KDreamelf/collaborative-editor/internal/model"
)

// ApplyPlainDelete 中立服务器删空正式行。不造 Delete 候选。
// 显式 Dispute 以该行为 RealLine、或行上挂插入子段/跨度基准时拒绝，保全锚点。
func (d *Doc) ApplyPlainDelete(line model.ID) error {
	if _, err := d.line(line); err != nil {
		return err
	}
	working := d.cloneDoc()
	if err := working.applyPlainDelete(line); err != nil {
		return err
	}
	if _, err := working.ordered(); err != nil {
		return err
	}
	*d = *working
	return nil
}

func (d *Doc) applyPlainDelete(line model.ID) error {
	ln, err := d.line(line)
	if err != nil {
		return err
	}
	if ln.Content != "" {
		return ErrNotEmpty
	}
	if err := d.plainLineBusy(line); err != nil {
		return err
	}
	if len(d.lines) == 1 {
		ln.Content = ""
		d.clearLineClaims(line)
		return nil
	}
	d.dropPlainInsertOrigins(map[model.ID]bool{line: true})
	return d.unlinkSegment([]model.ID{line})
}

// ApplyPlainMerge 行首 Backspace：当前行正文接到前行尾并 unlink。首行 no-op。
// 不调用 MergeUp（不造候选、不 Submit）。
func (d *Doc) ApplyPlainMerge(line model.ID) error {
	if _, err := d.line(line); err != nil {
		return err
	}
	working := d.cloneDoc()
	if err := working.applyPlainMerge(line); err != nil {
		return err
	}
	if _, err := working.ordered(); err != nil {
		return err
	}
	*d = *working
	return nil
}

func (d *Doc) applyPlainMerge(line model.ID) error {
	ln, err := d.line(line)
	if err != nil {
		return err
	}
	if ln.Prev.IsZero() {
		return nil
	}
	if err := d.plainLineBusy(line); err != nil {
		return err
	}
	if err := d.plainLineBusy(ln.Prev); err != nil {
		return err
	}
	d.dropPlainInsertOrigins(map[model.ID]bool{line: true, ln.Prev: true})
	prev := d.lines[ln.Prev]
	prev.Content = prev.Content + ln.Content
	return d.unlinkSegment([]model.ID{line})
}

// ApplyPlainSpan 跨多条当前相邻正式行做一次整段机械替换。
// 首行 ID 保持；不按 BaseTexts 落后开争议；失败零突变。
func (d *Doc) ApplyPlainSpan(opts SpanEditOpts) error {
	if err := d.validateSpanOpts(opts); err != nil {
		return err
	}
	lineIDs := append([]model.ID(nil), opts.LineIDs...)
	// 预生 ID 已在链上：仅当旧 BaseIDs[1:] 已不在且首行后继正好是 LineIDs 时视为重发。
	if len(lineIDs) > 0 && d.plainSpanAlreadyApplied(opts.BaseIDs, lineIDs, opts.Replacement) {
		return nil
	}
	if err := d.checkNewLineIDs(lineIDs); err != nil {
		return err
	}
	working := d.cloneDoc()
	if err := working.applyPlainSpan(opts); err != nil {
		return err
	}
	if _, err := working.ordered(); err != nil {
		return err
	}
	*d = *working
	return nil
}

func (d *Doc) applyPlainSpan(opts SpanEditOpts) error {
	baseIDs := opts.BaseIDs
	replacement := opts.Replacement
	lineIDs := opts.LineIDs
	if err := d.checkSpanIDsContiguous(baseIDs, opts.AfterSeen); err != nil {
		return err
	}
	if err := d.plainSpanBusy(baseIDs); err != nil {
		return err
	}
	affected := make(map[model.ID]bool, len(baseIDs))
	for _, id := range baseIDs {
		affected[id] = true
	}
	d.dropPlainInsertOrigins(affected)
	firstID := baseIDs[0]
	first := d.lines[firstID]
	afterSeen := opts.AfterSeen
	if len(baseIDs) > 1 {
		if err := d.unlinkSegment(baseIDs[1:]); err != nil {
			return err
		}
	}
	first.Content = replacement[0]
	if len(replacement) == 1 {
		return nil
	}
	_, err := d.spliceBetween(firstID, afterSeen, replacement[1:], lineIDs)
	return err
}

// ApplyPlainSuspend 只改已有显式 Dispute 的 suspended 标记。无该主张则 no-op，不把 live 提升成 solo Dispute。
func (d *Doc) ApplyPlainSuspend(person string, line model.ID, action string, on bool) error {
	if person == "" {
		return ErrPerson
	}
	if action != model.ActionEdit && !model.IsInsertAction(action) && action != model.ActionDelete {
		return ErrAction
	}
	item := d.byPerson(line, action, person)
	if item == nil {
		return nil
	}
	working := d.cloneDoc()
	cp := working.disputes[item.ID]
	if cp == nil {
		return nil
	}
	if on {
		working.suspended[cp.ID] = true
	} else {
		delete(working.suspended, cp.ID)
	}
	*d = *working
	return nil
}

// plainLineBusy 只让真正收到的主张阻止结构编辑。普通插入出处可在改动时失效。
func (d *Doc) plainLineBusy(line model.ID) error {
	if len(d.group(line, model.ActionEdit)) > 0 || d.hasInsertDisputes(line) {
		return ErrHasClaims
	}
	if err := d.spanMemberStructuralBlock(line); err != nil {
		return err
	}
	for _, other := range d.lines {
		o := other.InsertOrigin
		if o == nil || !originTouches(o.Anchor, o.LineIDs, map[model.ID]bool{line: true}) {
			continue
		}
		if len(d.group(o.Anchor, model.ActionEdit)) > 0 || d.hasInsertDisputes(o.Anchor) {
			return ErrHasClaims
		}
	}
	return nil
}

func originTouches(anchor model.ID, ids []model.ID, affected map[model.ID]bool) bool {
	if affected[anchor] {
		return true
	}
	for _, id := range ids {
		if affected[id] {
			return true
		}
	}
	return false
}

// 结构改动改变了插入段形状；仅撤掉涉及这些行的出处，保留正文和其他段。
// ponytail: 不重算被改段的历史边界；若以后需要在此基础上并发分叉，再加入明确的候选用例。
func (d *Doc) dropPlainInsertOrigins(affected map[model.ID]bool) {
	for _, ln := range d.lines {
		if o := ln.InsertOrigin; o != nil && originTouches(o.Anchor, o.LineIDs, affected) {
			ln.InsertOrigin = nil
		}
	}
	for key, history := range d.insertHistory {
		keep := history[:0]
		for _, segment := range history {
			if segment != nil && originTouches(key.line, segment.ids, affected) {
				continue
			}
			keep = append(keep, segment)
		}
		if len(keep) == 0 {
			delete(d.insertHistory, key)
		} else {
			d.insertHistory[key] = keep
		}
	}
	for key, segment := range d.live {
		if model.IsInsertAction(key.action) && segment != nil && originTouches(key.line, segment.ids, affected) {
			delete(d.live, key)
		}
	}
}

func (d *Doc) plainSpanAlreadyApplied(baseIDs, lineIDs []model.ID, replacement []string) bool {
	if len(baseIDs) == 0 || len(replacement) == 0 {
		return false
	}
	first := d.lines[baseIDs[0]]
	if first == nil || first.Content != replacement[0] {
		return false
	}
	for _, id := range baseIDs[1:] {
		if d.lines[id] != nil {
			return false
		}
	}
	if len(replacement) == 1 {
		return len(lineIDs) == 0
	}
	if len(lineIDs) == 0 || first.Next != lineIDs[0] {
		return false
	}
	return d.alreadyHaveIDs(lineIDs)
}

// plainSpanBusy：跨度内任一行有显式主张则拒；普通插入出处由结构改动撤去。
func (d *Doc) plainSpanBusy(baseIDs []model.ID) error {
	inSpan := make(map[model.ID]bool, len(baseIDs))
	for _, id := range baseIDs {
		inSpan[id] = true
		if _, err := d.line(id); err != nil {
			return ErrSpanBase
		}
		if err := d.plainLineBusy(id); err != nil {
			return ErrSpanBlocked
		}
	}
	for _, item := range d.disputes {
		for _, id := range item.BaseIDs {
			if inSpan[id] {
				return ErrSpanBlocked
			}
		}
	}
	return nil
}
