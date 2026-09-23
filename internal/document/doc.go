package document

import (
	"errors"
	"slices"

	"github.com/KDreamelf/collaborative-editor/internal/model"
)

var (
	ErrHead          = errors.New("正式行里没有「前」的那条不唯一，首行不确定")
	ErrBroken        = errors.New("正式行的链断了，或者残段没删干净")
	ErrAction        = errors.New("做法只能是改这行、插在后面或删这行")
	ErrContent       = errors.New("主张没有内容")
	ErrLine          = errors.New("没有这条正式行")
	ErrNotEmpty      = errors.New("非空行不能直接删除")
	ErrHasClaims     = errors.New("还有未决主张，不能合并")
	ErrLineIDs       = errors.New("新行 ID 数量或唯一性不对")
	ErrStaleInsert   = errors.New("插入基准无效或不在链上")
	ErrStaleEdit     = errors.New("改这行基准与最近编辑不对齐，一人出处无法安全展开")
	ErrPromoteUnsafe = errors.New("段内有无法安全提升的编辑或插入，暂不能转争议")
	ErrSpanBlocked   = errors.New("跨度内有未决插入、子争议或跨越锚点，不能整段替换")
	ErrSpanBase      = errors.New("跨度基准无效：行数、正文或后继对不上")
)

// CurrentBodyPerson 旧数据无编辑者时，用「当前正文」占候选人名字。
const CurrentBodyPerson = "当前正文"

// Doc 是一篇正在编辑的文档。内存是准的，调用方停一下再自己写入 MongoDB。
//
// 可收回的插入/单行编辑会把出处写进正式行（插入来源、最近编辑）；Load 据此恢复
// insertHistory 与编辑 live。挂起标记仍只在进程里。
type Doc struct {
	article       model.Article
	lines         map[model.ID]*model.Line
	disputes      map[model.ID]*model.Dispute
	suspended     map[model.ID]bool
	live          map[claimKey]*liveClaim
	insertHistory map[model.ID][]*liveClaim // 每锚点已入链的插入，旧→新；live 只留最新段
	pending       []followPend
}

type claimKey struct {
	line   model.ID
	action string
}

// liveClaim 是还没撞上的那一笔。撞上之后收回，改成争议文档。
// ids 记录这次 splice 进链的正式行，收回时只拆这些，不误伤叠在上面的别人的段。
// baseIDs/baseTexts 非空表示跨度整段替换：收回时按原跨度恢复，首行 ID 稳定。
type liveClaim struct {
	person    string
	content   []string
	before    string
	oldNext   model.ID
	spliced   bool
	ids       []model.ID
	baseIDs   []model.ID
	baseTexts []string
}

// SubmitOpts 是 Submit 的可选扩展。零值保持旧调用语义。
// AfterSeen：nil=未提供；非 nil（含零值 ID）=客户端快照里看见的锚点后继。
// BaseContent：nil=旧包；非 nil（含空串）=改这行时客户端快照里的正式行内容。
type SubmitOpts struct {
	AfterSeen   *model.ID
	BaseContent *string
	LineIDs     []model.ID
}

// SpanEditOpts 跨多条正式行的一次替换/粘贴/删除，一份整段主张。
// 首行 ID = BaseIDs[0] 稳定；LineIDs 只含替换结果中第 2 行起的新 ID。
type SpanEditOpts struct {
	BaseIDs     []model.ID
	BaseTexts   []string
	AfterSeen   model.ID
	Replacement []string
	LineIDs     []model.ID
}

type followPend struct {
	from, to string
	dispute  model.ID
	ts       int64
}

// Presence 是服务端知道的、停在这一行上的人。Active 表示输入光标还在、且没挂起。
type Presence struct {
	Person string
	Active bool
}

type View struct {
	Article   model.Article   `json:"article"`
	Lines     []model.Line    `json:"lines"`
	Disputes  []model.Dispute `json:"disputes"`
	Suspended []string        `json:"suspended"`
}

func New(title string) *Doc {
	line := model.NewID()
	d := &Doc{
		article:       model.Article{ID: model.NewID(), Title: title},
		lines:         map[model.ID]*model.Line{line: {ID: line}},
		disputes:      map[model.ID]*model.Dispute{},
		suspended:     map[model.ID]bool{},
		live:          map[claimKey]*liveClaim{},
		insertHistory: map[model.ID][]*liveClaim{},
	}
	return d
}

func Load(article model.Article, lines []model.Line, disputes []model.Dispute) (*Doc, error) {
	d := &Doc{
		article:       article,
		lines:         make(map[model.ID]*model.Line, len(lines)),
		disputes:      make(map[model.ID]*model.Dispute, len(disputes)),
		suspended:     map[model.ID]bool{},
		live:          map[claimKey]*liveClaim{},
		insertHistory: map[model.ID][]*liveClaim{},
	}
	for i := range lines {
		ln := lines[i]
		if ln.ID.IsZero() {
			return nil, ErrBroken
		}
		cp := copyLine(ln)
		d.lines[ln.ID] = &cp
	}
	for i := range disputes {
		item := disputes[i]
		if item.Followers == nil {
			item.Followers = []string{}
		}
		for _, p := range item.Pending {
			d.pending = append(d.pending, followPend{
				from:    p.From,
				to:      p.To,
				dispute: item.ID,
				ts:      p.ClientTs,
			})
		}
		item.Pending = nil
		cp := item
		d.disputes[item.ID] = &cp
	}
	if _, err := d.ordered(); err != nil {
		return nil, err
	}
	if err := d.rebuildFromOrigins(); err != nil {
		return nil, err
	}
	return d, nil
}

func (d *Doc) Article() model.Article { return d.article }

func (d *Doc) View() (View, error) {
	lines, err := d.ordered()
	if err != nil {
		return View{}, err
	}
	out := View{
		Article:   d.article,
		Lines:     make([]model.Line, len(lines)),
		Disputes:  make([]model.Dispute, 0, len(d.disputes)),
		Suspended: []string{},
	}
	for i, ln := range lines {
		out.Lines[i] = copyLine(*ln)
	}
	for id, item := range d.disputes {
		cp := *item
		// 空内容必须是 [] 不是 null，Mongo/JSON 原始结构可读。
		cp.Content = append([]string{}, item.Content...)
		cp.BaseIDs = append([]model.ID{}, item.BaseIDs...)
		cp.Followers = append([]string{}, item.Followers...)
		cp.Pending = nil
		for _, p := range d.pending {
			if p.dispute == id {
				cp.Pending = append(cp.Pending, model.PendingConfirm{
					From:     p.from,
					To:       p.to,
					ClientTs: p.ts,
				})
			}
		}
		if cp.Pending == nil {
			cp.Pending = []model.PendingConfirm{}
		}
		out.Disputes = append(out.Disputes, cp)
		if d.suspended[id] {
			out.Suspended = append(out.Suspended, id.Hex())
		}
	}
	slices.SortFunc(out.Disputes, func(a, b model.Dispute) int {
		if a.ID.Hex() < b.ID.Hex() {
			return -1
		}
		if a.ID.Hex() > b.ID.Hex() {
			return 1
		}
		return 0
	})
	slices.Sort(out.Suspended)
	return out, nil
}

// EnsureLines 让正式行至少有 n 条。不够就在末尾补空行。这些空行是正文，不是争议。
func (d *Doc) EnsureLines(n int) error {
	for {
		lines, err := d.ordered()
		if err != nil {
			return err
		}
		if len(lines) >= n {
			return nil
		}
		tail := lines[len(lines)-1]
		if _, err := d.splice(tail.ID, []string{""}); err != nil {
			return err
		}
	}
}

func (d *Doc) ordered() ([]*model.Line, error) {
	if len(d.lines) == 0 {
		return nil, ErrHead
	}
	var head *model.Line
	heads := 0
	for _, ln := range d.lines {
		if ln.Prev.IsZero() {
			heads++
			head = ln
		}
	}
	if heads != 1 {
		return nil, ErrHead
	}
	out := make([]*model.Line, 0, len(d.lines))
	seen := make(map[model.ID]bool, len(d.lines))
	for ln := head; ln != nil; {
		if seen[ln.ID] {
			return nil, ErrBroken
		}
		seen[ln.ID] = true
		out = append(out, ln)
		if ln.Next.IsZero() {
			break
		}
		next := d.lines[ln.Next]
		if next == nil || next.Prev != ln.ID {
			return nil, ErrBroken
		}
		ln = next
	}
	if len(out) != len(d.lines) {
		return nil, ErrBroken
	}
	return out, nil
}

func (d *Doc) line(id model.ID) (*model.Line, error) {
	ln := d.lines[id]
	if ln == nil {
		return nil, ErrLine
	}
	return ln, nil
}

// splice 把一段接到 anchor 和原来的后一行之间。
// 新行的前后在出生时写好。旧行只改两处：anchor 的后，原来后一行的前。
// 段尾的前仍是段里的上一行。ids 为空则服务端自生；非空则用客户端预生 ID。
func (d *Doc) splice(anchor model.ID, texts []string) ([]model.ID, error) {
	return d.spliceIDs(anchor, texts, nil)
}

func (d *Doc) spliceIDs(anchor model.ID, texts []string, ids []model.ID) ([]model.ID, error) {
	if len(texts) == 0 {
		return nil, ErrContent
	}
	base, err := d.line(anchor)
	if err != nil {
		return nil, err
	}
	if len(ids) == 0 {
		ids = make([]model.ID, len(texts))
		for i := range texts {
			ids[i] = model.NewID()
		}
	} else {
		if len(ids) != len(texts) {
			return nil, ErrLineIDs
		}
		seen := map[model.ID]bool{}
		for _, id := range ids {
			if id.IsZero() || seen[id] || d.lines[id] != nil {
				return nil, ErrLineIDs
			}
			seen[id] = true
		}
	}
	oldNext := base.Next
	prev := anchor
	for i, text := range texts {
		id := ids[i]
		ln := &model.Line{ID: id, Prev: prev, Content: text}
		d.lines[id] = ln
		d.lines[prev].Next = id
		prev = id
	}
	tail := d.lines[prev]
	tail.Next = oldNext
	if !oldNext.IsZero() {
		d.lines[oldNext].Prev = tail.ID
	}
	return ids, nil
}

func (d *Doc) archiveLiveInsert(anchor model.ID, live *liveClaim) {
	if live == nil {
		return
	}
	cp := cloneLive(live)
	if d.insertHistory == nil {
		d.insertHistory = map[model.ID][]*liveClaim{}
	}
	d.insertHistory[anchor] = append(d.insertHistory[anchor], cp)
}

func copyLine(ln model.Line) model.Line {
	out := ln
	if ln.InsertOrigin != nil {
		o := *ln.InsertOrigin
		o.LineIDs = append([]model.ID(nil), ln.InsertOrigin.LineIDs...)
		o.Content = append([]string(nil), ln.InsertOrigin.Content...)
		out.InsertOrigin = &o
	}
	if ln.RecentEdit != nil {
		e := *ln.RecentEdit
		out.RecentEdit = &e
	}
	if ln.EditOrigin != nil {
		o := *ln.EditOrigin
		o.BaseIDs = append([]model.ID(nil), ln.EditOrigin.BaseIDs...)
		o.BaseTexts = append([]string(nil), ln.EditOrigin.BaseTexts...)
		o.LineIDs = append([]model.ID(nil), ln.EditOrigin.LineIDs...)
		o.Content = append([]string(nil), ln.EditOrigin.Content...)
		out.EditOrigin = &o
	}
	return out
}

func cloneLive(live *liveClaim) *liveClaim {
	if live == nil {
		return nil
	}
	return &liveClaim{
		person:    live.person,
		content:   append([]string(nil), live.content...),
		before:    live.before,
		oldNext:   live.oldNext,
		spliced:   live.spliced,
		ids:       append([]model.ID(nil), live.ids...),
		baseIDs:   append([]model.ID(nil), live.baseIDs...),
		baseTexts: append([]string(nil), live.baseTexts...),
	}
}

func (d *Doc) setInsertOrigin(ids []model.ID, person string, anchor, oldNext model.ID, content []string) {
	if len(ids) == 0 {
		return
	}
	head := d.lines[ids[0]]
	if head == nil {
		return
	}
	head.InsertOrigin = &model.InsertOrigin{
		Person:  person,
		Anchor:  anchor,
		OldNext: oldNext,
		LineIDs: append([]model.ID(nil), ids...),
		Content: append([]string(nil), content...),
	}
	for _, id := range ids[1:] {
		if ln := d.lines[id]; ln != nil {
			ln.InsertOrigin = nil
		}
	}
}

func (d *Doc) clearInsertOrigin(ids []model.ID) {
	for _, id := range ids {
		if ln := d.lines[id]; ln != nil {
			ln.InsertOrigin = nil
		}
	}
}

func (d *Doc) setRecentEdit(line model.ID, person, before string) {
	ln := d.lines[line]
	if ln == nil {
		return
	}
	ln.RecentEdit = &model.RecentEdit{Person: person, Before: before}
	ln.EditOrigin = nil
}

func (d *Doc) clearRecentEdit(line model.ID) {
	if ln := d.lines[line]; ln != nil {
		ln.RecentEdit = nil
	}
}

func (d *Doc) setEditOrigin(resultIDs []model.ID, person string, baseIDs []model.ID, baseTexts []string, oldNext model.ID, content []string) {
	if len(resultIDs) == 0 {
		return
	}
	head := d.lines[resultIDs[0]]
	if head == nil {
		return
	}
	head.EditOrigin = &model.EditOrigin{
		Person:    person,
		BaseIDs:   append([]model.ID(nil), baseIDs...),
		BaseTexts: append([]string(nil), baseTexts...),
		OldNext:   oldNext,
		LineIDs:   append([]model.ID(nil), resultIDs...),
		Content:   append([]string(nil), content...),
	}
	head.RecentEdit = nil
	for _, id := range resultIDs[1:] {
		if ln := d.lines[id]; ln != nil {
			ln.EditOrigin = nil
		}
	}
}

func (d *Doc) clearEditOrigin(ids []model.ID) {
	for _, id := range ids {
		if ln := d.lines[id]; ln != nil {
			ln.EditOrigin = nil
		}
	}
}

// rebuildFromOrigins：从正式行上来源字段恢复 insertHistory 与编辑 live。
// 已入链插入一律进 history（旧→新）；与现网判断兼容。旧行无字段则跳过。
func (d *Doc) rebuildFromOrigins() error {
	type seg struct {
		anchor model.ID
		dist   int
		live   *liveClaim
	}
	var segs []seg
	for _, ln := range d.lines {
		if ln.InsertOrigin == nil {
			continue
		}
		claim, dist, err := d.parseInsertOrigin(ln)
		if err != nil {
			return err
		}
		segs = append(segs, seg{anchor: ln.InsertOrigin.Anchor, dist: dist, live: claim})
	}
	byAnchor := map[model.ID][]seg{}
	for _, s := range segs {
		byAnchor[s.anchor] = append(byAnchor[s.anchor], s)
	}
	for anchor, list := range byAnchor {
		slices.SortFunc(list, func(a, b seg) int { return a.dist - b.dist })
		// dist 小=靠近锚点=较新；history 要旧→新，故倒序写入。
		for i := len(list) - 1; i >= 0; i-- {
			d.insertHistory[anchor] = append(d.insertHistory[anchor], list[i].live)
		}
	}
	for id, ln := range d.lines {
		if ln.EditOrigin != nil {
			claim, err := d.parseEditOrigin(ln)
			if err != nil {
				return err
			}
			if d.byPerson(id, model.ActionEdit, claim.person) != nil {
				ln.EditOrigin = nil
				continue
			}
			d.live[claimKey{id, model.ActionEdit}] = claim
			continue
		}
		if ln.RecentEdit == nil {
			continue
		}
		if ln.RecentEdit.Person == "" {
			return ErrBroken
		}
		if d.byPerson(id, model.ActionEdit, ln.RecentEdit.Person) != nil {
			ln.RecentEdit = nil
			continue
		}
		// 多行粘贴的 splice 态不在「最近编辑」里；只恢复普通单行。
		d.live[claimKey{id, model.ActionEdit}] = &liveClaim{
			person:  ln.RecentEdit.Person,
			content: []string{ln.Content},
			before:  ln.RecentEdit.Before,
		}
	}
	return nil
}

func (d *Doc) parseEditOrigin(ln *model.Line) (*liveClaim, error) {
	o := ln.EditOrigin
	if o.Person == "" || len(o.BaseIDs) < 2 || len(o.BaseIDs) != len(o.BaseTexts) {
		return nil, ErrBroken
	}
	if len(o.LineIDs) == 0 || len(o.LineIDs) != len(o.Content) || o.LineIDs[0] != ln.ID {
		return nil, ErrBroken
	}
	if o.BaseIDs[0] != ln.ID {
		return nil, ErrBroken
	}
	for i, id := range o.LineIDs {
		row := d.lines[id]
		if row == nil {
			return nil, ErrBroken
		}
		if i > 0 && row.Prev != o.LineIDs[i-1] {
			return nil, ErrBroken
		}
		if i+1 < len(o.LineIDs) && row.Next != o.LineIDs[i+1] {
			return nil, ErrBroken
		}
		if i > 0 && row.EditOrigin != nil {
			return nil, ErrBroken
		}
	}
	last := d.lines[o.LineIDs[len(o.LineIDs)-1]]
	if last.Next != o.OldNext {
		return nil, ErrBroken
	}
	if !o.OldNext.IsZero() {
		if n := d.lines[o.OldNext]; n == nil || n.Prev != last.ID {
			return nil, ErrBroken
		}
	}
	newIDs := append([]model.ID(nil), o.LineIDs[1:]...)
	return &liveClaim{
		person:    o.Person,
		content:   append([]string(nil), o.Content...),
		before:    o.BaseTexts[0],
		oldNext:   o.OldNext,
		spliced:   true,
		ids:       newIDs,
		baseIDs:   append([]model.ID(nil), o.BaseIDs...),
		baseTexts: append([]string(nil), o.BaseTexts...),
	}, nil
}

func (d *Doc) parseInsertOrigin(ln *model.Line) (*liveClaim, int, error) {
	o := ln.InsertOrigin
	if o.Person == "" || len(o.LineIDs) == 0 || len(o.LineIDs) != len(o.Content) {
		return nil, 0, ErrBroken
	}
	if o.LineIDs[0] != ln.ID {
		return nil, 0, ErrBroken
	}
	if _, err := d.line(o.Anchor); err != nil {
		return nil, 0, err
	}
	for i, id := range o.LineIDs {
		row := d.lines[id]
		if row == nil {
			return nil, 0, ErrBroken
		}
		if i > 0 && row.Prev != o.LineIDs[i-1] {
			return nil, 0, ErrBroken
		}
		if i+1 < len(o.LineIDs) && row.Next != o.LineIDs[i+1] {
			return nil, 0, ErrBroken
		}
		if i > 0 && row.InsertOrigin != nil {
			return nil, 0, ErrBroken
		}
	}
	last := d.lines[o.LineIDs[len(o.LineIDs)-1]]
	if last.Next != o.OldNext {
		return nil, 0, ErrBroken
	}
	if !o.OldNext.IsZero() {
		if n := d.lines[o.OldNext]; n == nil || n.Prev != last.ID {
			return nil, 0, ErrBroken
		}
	}
	// 同步堆叠后旧段段首 Prev 不再等于锚点；只需锚点沿链能走到段首。
	dist := distAlong(d, o.Anchor, ln.ID)
	if dist < 1 {
		return nil, 0, ErrBroken
	}
	return &liveClaim{
		person:  o.Person,
		content: append([]string(nil), o.Content...),
		oldNext: o.OldNext,
		spliced: true,
		ids:     append([]model.ID(nil), o.LineIDs...),
	}, dist, nil
}

// distAlong：从 anchor 沿「后」走到 target 的步数；不可达返回 -1。
func distAlong(d *Doc, anchor, target model.ID) int {
	seen := map[model.ID]bool{}
	id := anchor
	for steps := 0; steps <= len(d.lines); steps++ {
		if id == target {
			return steps
		}
		ln := d.lines[id]
		if ln == nil || ln.Next.IsZero() || seen[ln.Next] {
			return -1
		}
		seen[ln.Next] = true
		id = ln.Next
	}
	return -1
}

func (d *Doc) alreadySpliced(ids []model.ID, texts []string) bool {
	if len(ids) == 0 || len(ids) != len(texts) {
		return false
	}
	for i, id := range ids {
		ln := d.lines[id]
		if ln == nil || ln.Content != texts[i] {
			return false
		}
		if i > 0 && ln.Prev != ids[i-1] {
			return false
		}
		if i+1 < len(ids) && ln.Next != ids[i+1] {
			return false
		}
	}
	return true
}

// alreadyHaveIDs：重发确认——行 ID 已在链上且相连即视为已执行，不比对正文、不改 live。
func (d *Doc) alreadyHaveIDs(ids []model.ID) bool {
	if len(ids) == 0 {
		return false
	}
	for i, id := range ids {
		ln := d.lines[id]
		if ln == nil {
			return false
		}
		if i > 0 && ln.Prev != ids[i-1] {
			return false
		}
		if i+1 < len(ids) && ln.Next != ids[i+1] {
			return false
		}
	}
	return true
}

// checkNewLineIDs：预生 ID 不得与已有正式行碰撞（完整重发已由 alreadyHaveIDs 短路）。
func (d *Doc) checkNewLineIDs(ids []model.ID) error {
	for _, id := range ids {
		if d.lines[id] != nil {
			return ErrLineIDs
		}
	}
	return nil
}

// checkSegmentUnlink：只读预检，与 unlinkSegment 成功条件一致。
// 同步堆叠后旧段 Prev 不是锚点，只要求邻接指针与 oldNext 自洽。
func (d *Doc) checkSegmentUnlink(anchor model.ID, seg *liveClaim) error {
	if seg == nil || !seg.spliced {
		return nil
	}
	ids := seg.ids
	if len(ids) == 0 {
		if _, err := d.between(anchor, seg.oldNext); err != nil {
			return err
		}
		return nil
	}
	for i, id := range ids {
		ln := d.lines[id]
		if ln == nil {
			return ErrBroken
		}
		if i > 0 && ln.Prev != ids[i-1] {
			return ErrBroken
		}
		if i+1 < len(ids) && ln.Next != ids[i+1] {
			return ErrBroken
		}
	}
	first, last := d.lines[ids[0]], d.lines[ids[len(ids)-1]]
	if last.Next != seg.oldNext {
		return ErrBroken
	}
	if first.Prev.IsZero() {
		return ErrBroken
	}
	if p := d.lines[first.Prev]; p == nil || p.Next != first.ID {
		return ErrBroken
	}
	if !last.Next.IsZero() {
		if n := d.lines[last.Next]; n == nil || n.Prev != last.ID {
			return ErrBroken
		}
	}
	return nil
}

func (d *Doc) unlinkSegment(ids []model.ID) error {
	if len(ids) == 0 {
		return ErrBroken
	}
	for i, id := range ids {
		ln := d.lines[id]
		if ln == nil {
			return ErrBroken
		}
		if i > 0 && ln.Prev != ids[i-1] {
			return ErrBroken
		}
		if i+1 < len(ids) && ln.Next != ids[i+1] {
			return ErrBroken
		}
	}
	first, last := d.lines[ids[0]], d.lines[ids[len(ids)-1]]
	prev, next := first.Prev, last.Next
	if !prev.IsZero() {
		if p := d.lines[prev]; p != nil {
			p.Next = next
		}
	}
	if !next.IsZero() {
		if n := d.lines[next]; n != nil {
			n.Prev = prev
		}
	}
	for _, id := range ids {
		delete(d.lines, id)
		d.clearLineClaims(id)
	}
	return nil
}

// sameDisputeSlot：同一真实行上，「改这行」与「删这行」同一场争议；「插在后面」独立。
func sameDisputeSlot(a, b string) bool {
	if a == b {
		return true
	}
	return lineBodyAction(a) && lineBodyAction(b)
}

func lineBodyAction(a string) bool {
	return a == model.ActionEdit || a == model.ActionDelete
}

func (d *Doc) hasLiveInsert(line model.ID) bool {
	return d.live[claimKey{line, model.ActionInsert}] != nil
}

// blocksMergeLive：同锚点有 live 插入，或别人的 live 编辑时，合并会清掉它们。
func (d *Doc) blocksMergeLive(line model.ID, self string) bool {
	for key, live := range d.live {
		if key.line != line {
			continue
		}
		if key.action == model.ActionInsert {
			return true
		}
		if live != nil && live.person != self {
			return true
		}
	}
	return false
}

func (d *Doc) group(line model.ID, action string) []*model.Dispute {
	var out []*model.Dispute
	for _, item := range d.disputes {
		if item.RealLine == line && sameDisputeSlot(item.Action, action) {
			out = append(out, item)
		}
	}
	return out
}

func (d *Doc) byPerson(line model.ID, action, person string) *model.Dispute {
	for _, item := range d.group(line, action) {
		if item.Person == person {
			return item
		}
	}
	return nil
}

func followedLeader(group []*model.Dispute, person string) *model.Dispute {
	for _, item := range group {
		if slices.Contains(item.Followers, person) {
			return item
		}
	}
	return nil
}

func (d *Doc) standers(group []*model.Dispute) []*model.Dispute {
	var out []*model.Dispute
	for _, item := range group {
		if followedLeader(group, item.Person) == nil {
			out = append(out, item)
		}
	}
	return out
}
