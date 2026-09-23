package document

import (
	"errors"
	"slices"

	"github.com/KDreamelf/collaborative-editor/internal/model"
)

var (
	ErrHead    = errors.New("正式行里没有「前」的那条不唯一，首行不确定")
	ErrBroken  = errors.New("正式行的链断了，或者残段没删干净")
	ErrAction  = errors.New("做法只能是改这行或插在后面")
	ErrContent = errors.New("主张没有内容")
	ErrLine    = errors.New("没有这条正式行")
)

// Doc 是一篇正在编辑的文档。内存是准的，调用方停一下再自己写入 MongoDB。
//
// ponytail: 一个人正在写、还没和第二个人撞上时，可收回的那一笔只放在进程里。
// 重启之后以已经落地的正式行和争议文档为准。挂起也不进库。
type Doc struct {
	article   model.Article
	lines     map[model.ID]*model.Line
	disputes  map[model.ID]*model.Dispute
	suspended map[model.ID]bool
	live      map[claimKey]*liveClaim
	pending   []followPend
}

type claimKey struct {
	line   model.ID
	action string
}

// liveClaim 是还没撞上的那一笔。撞上之后收回，改成争议文档。
type liveClaim struct {
	person  string
	content []string
	before  string
	oldNext model.ID
	spliced bool
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
		article:   model.Article{ID: model.NewID(), Title: title},
		lines:     map[model.ID]*model.Line{line: {ID: line}},
		disputes:  map[model.ID]*model.Dispute{},
		suspended: map[model.ID]bool{},
		live:      map[claimKey]*liveClaim{},
	}
	return d
}

func Load(article model.Article, lines []model.Line, disputes []model.Dispute) (*Doc, error) {
	d := &Doc{
		article:   article,
		lines:     make(map[model.ID]*model.Line, len(lines)),
		disputes:  make(map[model.ID]*model.Dispute, len(disputes)),
		suspended: map[model.ID]bool{},
		live:      map[claimKey]*liveClaim{},
	}
	for i := range lines {
		ln := lines[i]
		if ln.ID.IsZero() {
			return nil, ErrBroken
		}
		cp := ln
		d.lines[ln.ID] = &cp
	}
	for i := range disputes {
		item := disputes[i]
		if item.Followers == nil {
			item.Followers = []string{}
		}
		cp := item
		d.disputes[item.ID] = &cp
	}
	if _, err := d.ordered(); err != nil {
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
		out.Lines[i] = *ln
	}
	for id, item := range d.disputes {
		cp := *item
		cp.Content = append([]string(nil), item.Content...)
		cp.Followers = append([]string(nil), item.Followers...)
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
// 段尾的前仍是段里的上一行。
func (d *Doc) splice(anchor model.ID, texts []string) ([]model.ID, error) {
	if len(texts) == 0 {
		return nil, ErrContent
	}
	base, err := d.line(anchor)
	if err != nil {
		return nil, err
	}
	oldNext := base.Next
	prev := anchor
	ids := make([]model.ID, len(texts))
	for i, text := range texts {
		id := model.NewID()
		ln := &model.Line{ID: id, Prev: prev, Content: text}
		d.lines[id] = ln
		d.lines[prev].Next = id
		prev = id
		ids[i] = id
	}
	tail := d.lines[prev]
	tail.Next = oldNext
	if !oldNext.IsZero() {
		d.lines[oldNext].Prev = tail.ID
	}
	return ids, nil
}

func (d *Doc) group(line model.ID, action string) []*model.Dispute {
	var out []*model.Dispute
	for _, item := range d.disputes {
		if item.RealLine == line && item.Action == action {
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
