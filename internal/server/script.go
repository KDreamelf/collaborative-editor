package server

import (
	"encoding/json"
	"fmt"

	"github.com/KDreamelf/collaborative-editor/internal/document"
	"github.com/KDreamelf/collaborative-editor/internal/model"
	"github.com/KDreamelf/collaborative-editor/internal/protocol"
)

// Script 按一行一条 JSON 操作内存里的文档。测试夹具走这里，不进桌面端正式编译。
type Script struct {
	h       *Hub
	current string
	seq     int64
}

func NewScript() *Script {
	return &Script{h: NewHub(nil)}
}

type step struct {
	Op     string   `json:"op"`
	Title  string   `json:"title"`
	Person string   `json:"person"`
	Name   string   `json:"name"`
	Line   int      `json:"line"`
	Text   string   `json:"text"`
	Lines  []string `json:"lines"`
	Action string   `json:"action"`
	Target string   `json:"target"`
	From   string   `json:"from"`
	Accept *bool    `json:"accept"`
	On     *bool    `json:"on"`
	Ts     int64    `json:"ts"`
}

func (s *Script) Exec(raw []byte) (document.View, error) {
	var st step
	if err := json.Unmarshal(raw, &st); err != nil {
		return document.View{}, err
	}
	if err := s.do(st); err != nil {
		return document.View{}, err
	}
	if s.current == "" {
		return document.View{}, nil
	}
	return s.h.GetView(s.current)
}

func (s *Script) do(st step) error {
	switch st.Op {
	case "create":
		meta := s.h.CreateArticle(st.Title)
		s.current = meta.ID
		return nil
	case "join":
		r, err := s.room()
		if err != nil {
			return err
		}
		name := st.Name
		if name == "" {
			name = st.Person
		}
		s.h.join(r, &wsClient{}, st.Person, name)
		return nil
	case "edit", "insert":
		return s.submit(st)
	case "caret":
		return s.caret(st)
	case "delete":
		return s.deleteLine(st)
	case "merge":
		return s.merge(st)
	case "suspend":
		return s.suspend(st)
	case "follow":
		return s.follow(st)
	case "answer":
		return s.answer(st)
	case "view":
		_, err := s.room()
		return err
	default:
		return fmt.Errorf("未知操作 %s", st.Op)
	}
}

func (s *Script) room() (*room, error) {
	if s.current == "" {
		return nil, fmt.Errorf("还没有文章")
	}
	r := s.h.getRoom(s.current)
	if r == nil {
		return nil, errNotFound
	}
	return r, nil
}

func (s *Script) lineID(index int) (model.ID, error) {
	v, err := s.h.GetView(s.current)
	if err != nil {
		return model.ID{}, err
	}
	if index < 0 || index >= len(v.Lines) {
		return model.ID{}, fmt.Errorf("没有第 %d 行", index)
	}
	return v.Lines[index].ID, nil
}

func (s *Script) submit(st step) error {
	r, err := s.room()
	if err != nil {
		return err
	}
	id, err := s.lineID(st.Line)
	if err != nil {
		return err
	}
	action := st.Action
	content := st.Lines
	if st.Op == "insert" {
		if action == "" {
			action = model.ActionInsert
		}
		if len(content) == 0 {
			content = []string{st.Text}
		}
	} else {
		if action == "" {
			action = model.ActionEdit
		}
		if len(content) == 0 {
			content = []string{st.Text}
		}
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if err := r.doc.Submit(st.Person, id, action, content); err != nil {
		return err
	}
	r.dirty = true
	return nil
}

func (s *Script) caret(st step) error {
	r, err := s.room()
	if err != nil {
		return err
	}
	id, err := s.lineID(st.Line)
	if err != nil {
		return err
	}
	name := st.Name
	if name == "" {
		name = st.Person
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.cursors[st.Person] = protocol.Cursor{PersonID: st.Person, Name: name, LineID: id.Hex()}
	return nil
}

func (s *Script) deleteLine(st step) error {
	r, err := s.room()
	if err != nil {
		return err
	}
	id, err := s.lineID(st.Line)
	if err != nil {
		return err
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if err := r.doc.DeleteIfIdle(st.Person, id, r.holdersFor(id)); err != nil {
		return err
	}
	r.dirty = true
	return nil
}

func (s *Script) merge(st step) error {
	r, err := s.room()
	if err != nil {
		return err
	}
	id, err := s.lineID(st.Line)
	if err != nil {
		return err
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if err := r.doc.MergeUp(st.Person, id, r.holdersFor(id)); err != nil {
		return err
	}
	r.dirty = true
	return nil
}

func (s *Script) suspend(st step) error {
	r, err := s.room()
	if err != nil {
		return err
	}
	id, err := s.lineID(st.Line)
	if err != nil {
		return err
	}
	action := st.Action
	if action == "" {
		action = model.ActionEdit
	}
	on := true
	if st.On != nil {
		on = *st.On
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if err := r.doc.SetSuspended(st.Person, id, action, on); err != nil {
		return err
	}
	r.dirty = true
	return nil
}

func (s *Script) disputeID(person, action string, line model.ID) (model.ID, error) {
	v, err := s.h.GetView(s.current)
	if err != nil {
		return model.ID{}, err
	}
	if action == "" {
		action = model.ActionEdit
	}
	for _, d := range v.Disputes {
		if d.Person == person && d.Action == action && d.RealLine == line {
			return d.ID, nil
		}
	}
	return model.ID{}, fmt.Errorf("没有 %s 的主张", person)
}

func (s *Script) follow(st step) error {
	r, err := s.room()
	if err != nil {
		return err
	}
	line, err := s.lineID(st.Line)
	if err != nil {
		return err
	}
	id, err := s.disputeID(st.Target, st.Action, line)
	if err != nil {
		return err
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	_, err = r.doc.RequestFollow(st.Person, id, st.Ts)
	return err
}

func (s *Script) answer(st step) error {
	r, err := s.room()
	if err != nil {
		return err
	}
	line, err := s.lineID(st.Line)
	if err != nil {
		return err
	}
	id, err := s.disputeID(st.Person, st.Action, line)
	if err != nil {
		return err
	}
	accept := true
	if st.Accept != nil {
		accept = *st.Accept
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	_, err = r.doc.AnswerFollow(st.Person, st.From, id, accept)
	return err
}
