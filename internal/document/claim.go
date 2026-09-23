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

// Submit 收下一个人的一份主张。
// 这处没有别人还在写的主张时，直接写进正式行。
// 有别人还在写，两份都留下，正式行回到撞上之前。后到的不盖先到的。
// 对方已经挂起时，不拉争议，新内容直接进来。
func (d *Doc) Submit(person string, line model.ID, action string, content []string) error {
	if action != model.ActionEdit && action != model.ActionInsert {
		return ErrAction
	}
	if len(content) == 0 {
		return ErrContent
	}
	if _, err := d.line(line); err != nil {
		return err
	}
	content = append([]string(nil), content...)
	d.detachFollow(person, line, action)

	key := claimKey{line, action}
	if otherActive(d.group(line, action), person, d.suspended) {
		d.upsert(person, line, action, content, false)
		return nil
	}
	if live := d.live[key]; live != nil && live.person != person {
		return d.openDispute(key, live, person, content)
	}
	if item := d.byPerson(line, action, person); item != nil && d.live[key] == nil {
		delete(d.suspended, item.ID)
		item.Content = content
		docs := d.group(line, action)
		if len(d.standers(docs)) == 1 && len(docs) == 1 {
			saved := append([]string(nil), item.Content...)
			delete(d.disputes, item.ID)
			return d.writeThrough(person, key, saved)
		}
		return nil
	}
	if live := d.live[key]; live != nil && live.person == person {
		return d.updateLive(key, live, content)
	}
	return d.writeThrough(person, key, content)
}

// SetSuspended 挂起或恢复。挂起后别人来写不拉争议。主张还在，别人仍可以接受。
// 已经接进正文的插入不再收回，否则挂起后再接受会把同一段再接一次。
func (d *Doc) SetSuspended(person string, line model.ID, action string, suspended bool) error {
	if action != model.ActionEdit && action != model.ActionInsert {
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
		delete(d.live, key)
		return nil
	}
	item := d.upsert(person, line, action, append([]string(nil), live.content...), true)
	d.suspended[item.ID] = true
	delete(d.live, key)
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

// DeleteIfIdle 删掉空行。这行上还有别人没挂起的输入光标，就不删，拉争议。
// 只剩一行时清空内容，不把链拆没。
func (d *Doc) DeleteIfIdle(person string, line model.ID, holders []Presence) error {
	ln, err := d.line(line)
	if err != nil {
		return err
	}
	if held(holders, person) {
		d.holdDelete(person, ln, holders)
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
// 这一行上还有人，整步不做。
func (d *Doc) MergeUp(person string, line model.ID, holders []Presence) error {
	ln, err := d.line(line)
	if err != nil {
		return err
	}
	if ln.Prev.IsZero() {
		return nil
	}
	if held(holders, person) {
		d.holdDelete(person, ln, holders)
		return nil
	}
	prev := d.lines[ln.Prev]
	joined := prev.Content + ln.Content
	d.unlink(ln)
	d.clearLineClaims(line)
	return d.Submit(person, prev.ID, model.ActionEdit, []string{joined})
}

func (d *Doc) updateLive(key claimKey, live *liveClaim, content []string) error {
	if key.action == model.ActionInsert {
		delete(d.live, key)
		return d.writeThrough(live.person, key, content)
	}
	ln := d.lines[key.line]
	live.content = content
	ln.Content = content[0]
	if len(content) > 1 && !live.spliced {
		oldNext := ln.Next
		if _, err := d.splice(key.line, content[1:]); err != nil {
			return err
		}
		live.spliced = true
		live.oldNext = oldNext
	}
	return nil
}

func (d *Doc) writeThrough(person string, key claimKey, content []string) error {
	ln := d.lines[key.line]
	fresh := &liveClaim{person: person, content: content}
	if key.action == model.ActionEdit {
		fresh.before = ln.Content
		ln.Content = content[0]
		if len(content) > 1 {
			oldNext := ln.Next
			if _, err := d.splice(key.line, content[1:]); err != nil {
				ln.Content = fresh.before
				return err
			}
			fresh.spliced = true
			fresh.oldNext = oldNext
		}
	} else {
		oldNext := ln.Next
		if _, err := d.splice(key.line, content); err != nil {
			return err
		}
		fresh.spliced = true
		fresh.oldNext = oldNext
	}
	d.live[key] = fresh
	return nil
}

func (d *Doc) openDispute(key claimKey, live *liveClaim, person string, content []string) error {
	if err := d.revert(key, live); err != nil {
		return err
	}
	delete(d.live, key)
	d.upsert(live.person, key.line, key.action, live.content, false)
	d.upsert(person, key.line, key.action, content, false)
	return nil
}

func (d *Doc) revert(key claimKey, live *liveClaim) error {
	ln := d.lines[key.line]
	if key.action == model.ActionEdit {
		ln.Content = live.before
	}
	if !live.spliced {
		return nil
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

func (d *Doc) holdDelete(person string, ln *model.Line, holders []Presence) {
	content := []string{ln.Content}
	d.upsert(person, ln.ID, model.ActionEdit, content, false)
	for _, h := range holders {
		if h.Active && h.Person != person {
			d.upsert(h.Person, ln.ID, model.ActionEdit, content, false)
		}
	}
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
		if p.from == from && p.to == to && item != nil && item.RealLine == line && item.Action == action {
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
	key := claimKey{line, action}
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
	if live := d.live[key]; live != nil {
		if err := d.revert(key, live); err != nil {
			return
		}
		delete(d.live, key)
	}
	content := append([]string(nil), winner.Content...)
	if err := d.applyWinner(line, action, content); err != nil {
		return
	}
	for _, item := range docs {
		delete(d.disputes, item.ID)
		delete(d.suspended, item.ID)
	}
	d.sweepPending()
}

func (d *Doc) applyWinner(line model.ID, action string, content []string) error {
	ln, err := d.line(line)
	if err != nil {
		return err
	}
	switch action {
	case model.ActionEdit:
		if len(content) == 0 {
			return ErrContent
		}
		ln.Content = content[0]
		if len(content) > 1 {
			_, err = d.splice(line, content[1:])
			return err
		}
		return nil
	case model.ActionInsert:
		_, err = d.splice(line, content)
		return err
	default:
		return ErrAction
	}
}

func (d *Doc) sweepPending() {
	d.pending = slices.DeleteFunc(d.pending, func(p followPend) bool {
		return d.disputes[p.dispute] == nil
	})
}
