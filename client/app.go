package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/KDreamelf/collaborative-editor/internal/document"
	"github.com/KDreamelf/collaborative-editor/internal/model"
	"github.com/KDreamelf/collaborative-editor/internal/protocol"
	"github.com/gorilla/websocket"
)

type ArticleBrief struct {
	ID    string `json:"id"`
	Title string `json:"title"`
}

type App struct {
	ctx context.Context

	mu        sync.Mutex
	serverURL string
	personID  string
	name      string
	articleID string

	doc     *document.Doc
	cursors []protocol.Cursor
	people  []protocol.Person

	conn    *websocket.Conn
	dialing bool
	stop    chan struct{}

	queue      []protocol.Op
	deferred   []any
	sentCount  int
	sentSeq    int64
	nextSeq    int64
	oldestAt   time.Time
	lastEnqAt  time.Time
	flushTimer *time.Timer
}

func NewApp() *App {
	return &App{
		serverURL: "http://127.0.0.1:8787",
		personID:  model.NewID().Hex(),
		nextSeq:   1,
		stop:      make(chan struct{}),
	}
}

func (a *App) startup(ctx context.Context) {
	a.ctx = ctx
}

func (a *App) PersonID() string {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.personID
}

func (a *App) SetServer(raw string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return
	}
	a.serverURL = strings.TrimRight(raw, "/")
}

func (a *App) CreateArticle(title string) (string, error) {
	title = strings.TrimSpace(title)
	if title == "" {
		title = "未命名"
	}
	body, _ := json.Marshal(map[string]string{"title": title})
	resp, err := a.httpDo(http.MethodPost, "/api/articles", body)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", err
	}
	if resp.StatusCode >= 300 {
		return "", fmt.Errorf("%s", strings.TrimSpace(string(data)))
	}
	var out struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(data, &out); err != nil {
		return "", err
	}
	if out.ID == "" {
		return "", fmt.Errorf("服务端未返回文章 id")
	}
	return out.ID, nil
}

func (a *App) ListArticles() ([]ArticleBrief, error) {
	resp, err := a.httpDo(http.MethodGet, "/api/articles", nil)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode >= 300 {
		return nil, fmt.Errorf("%s", strings.TrimSpace(string(data)))
	}
	var list []ArticleBrief
	if err := json.Unmarshal(data, &list); err != nil {
		return nil, err
	}
	if list == nil {
		list = []ArticleBrief{}
	}
	return list, nil
}

func (a *App) Join(articleID, name string) error {
	articleID = strings.TrimSpace(articleID)
	name = strings.TrimSpace(name)
	if articleID == "" {
		return fmt.Errorf("缺少文章 id")
	}
	if name == "" {
		name = "未命名"
	}

	a.mu.Lock()
	a.articleID = articleID
	a.name = name
	if a.doc == nil {
		a.doc = document.New("")
	}
	a.mu.Unlock()

	return a.connect()
}

func (a *App) SubmitEdit(lineID, content string) error {
	return a.enqueueSubmit(lineID, model.ActionEdit, []string{content})
}

func (a *App) SubmitPaste(lineID string, lines []string) error {
	if len(lines) == 0 {
		return fmt.Errorf("粘贴内容为空")
	}
	return a.enqueueSubmit(lineID, model.ActionEdit, lines)
}

func (a *App) SubmitInsert(lineID string, lines []string) error {
	if lines == nil {
		lines = []string{""}
	}
	if len(lines) == 0 {
		lines = []string{""}
	}
	return a.enqueueSubmit(lineID, model.ActionInsert, lines)
}

func (a *App) DeleteLine(lineID string) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.doc == nil {
		return fmt.Errorf("尚未加入文档")
	}
	id, err := model.ParseID(lineID)
	if err != nil {
		return err
	}
	holders := a.holdersLocked(lineID)
	if err := a.doc.DeleteIfIdle(a.personID, id, holders); err != nil {
		return err
	}
	op := protocol.Op{
		Kind: protocol.TypeDelete,
		Delete: &protocol.Delete{
			Type:     protocol.TypeDelete,
			PersonID: a.personID,
			LineID:   lineID,
		},
	}
	a.pushOpLocked(op)
	a.emitSnapshotLocked()
	return nil
}

func (a *App) MergeUp(lineID string) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.doc == nil {
		return fmt.Errorf("尚未加入文档")
	}
	id, err := model.ParseID(lineID)
	if err != nil {
		return err
	}
	holders := a.holdersLocked(lineID)
	if err := a.doc.MergeUp(a.personID, id, holders); err != nil {
		return err
	}
	op := protocol.Op{
		Kind: protocol.TypeMerge,
		Merge: &protocol.Merge{
			Type:     protocol.TypeMerge,
			PersonID: a.personID,
			LineID:   lineID,
		},
	}
	a.pushOpLocked(op)
	a.emitSnapshotLocked()
	return nil
}

func (a *App) MoveCaret(lineID, disputeID string, offset, selEnd int) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	msg := protocol.CursorMsg{
		Type: protocol.TypeCursor,
		Cursor: protocol.Cursor{
			PersonID:  a.personID,
			Name:      a.name,
			LineID:    lineID,
			DisputeID: disputeID,
			Offset:    offset,
			SelEnd:    selEnd,
		},
	}
	return a.sendAfterEditsLocked(msg)
}

func (a *App) Suspend(lineID, action string, on bool) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.doc == nil {
		return fmt.Errorf("尚未加入文档")
	}
	id, err := model.ParseID(lineID)
	if err != nil {
		return err
	}
	if err := a.doc.SetSuspended(a.personID, id, action, on); err != nil {
		return err
	}
	msg := protocol.Suspend{
		Type:      protocol.TypeSuspend,
		PersonID:  a.personID,
		LineID:    lineID,
		Action:    action,
		Suspended: on,
	}
	if err := a.sendAfterEditsLocked(msg); err != nil {
		return err
	}
	a.emitSnapshotLocked()
	return nil
}

func (a *App) RequestFollow(disputeID string) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.doc == nil {
		return fmt.Errorf("尚未加入文档")
	}
	id, err := model.ParseID(disputeID)
	if err != nil {
		return err
	}
	ts := time.Now().UnixMilli()
	if _, err := a.doc.RequestFollow(a.personID, id, ts); err != nil {
		return err
	}
	msg := protocol.Follow{
		Type:      protocol.TypeFollow,
		PersonID:  a.personID,
		DisputeID: disputeID,
		ClientTs:  ts,
	}
	if err := a.sendAfterEditsLocked(msg); err != nil {
		return err
	}
	a.emitSnapshotLocked()
	return nil
}

func (a *App) AnswerFollow(fromID, disputeID string, accept bool) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.doc == nil {
		return fmt.Errorf("尚未加入文档")
	}
	id, err := model.ParseID(disputeID)
	if err != nil {
		return err
	}
	if _, err := a.doc.AnswerFollow(a.personID, fromID, id, accept); err != nil {
		return err
	}
	msg := protocol.FollowAnswer{
		Type:      protocol.TypeFollowAnswer,
		PersonID:  a.personID,
		FromID:    fromID,
		DisputeID: disputeID,
		Accept:    accept,
	}
	if err := a.sendAfterEditsLocked(msg); err != nil {
		return err
	}
	a.emitSnapshotLocked()
	return nil
}

func (a *App) enqueueSubmit(lineID, action string, content []string) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.doc == nil {
		return fmt.Errorf("尚未加入文档")
	}
	id, err := model.ParseID(lineID)
	if err != nil {
		return err
	}
	content = append([]string(nil), content...)
	if err := a.doc.Submit(a.personID, id, action, content); err != nil {
		return err
	}
	op := protocol.Op{
		Kind: protocol.TypeSubmit,
		Submit: &protocol.Submit{
			Type:     protocol.TypeSubmit,
			PersonID: a.personID,
			LineID:   lineID,
			Action:   action,
			Content:  content,
			ClientTs: time.Now().UnixMilli(),
		},
	}
	a.pushOpLocked(op)
	a.emitSnapshotLocked()
	return nil
}

func (a *App) httpDo(method, path string, body []byte) (*http.Response, error) {
	a.mu.Lock()
	base := a.serverURL
	a.mu.Unlock()
	var rdr io.Reader
	if body != nil {
		rdr = bytes.NewReader(body)
	}
	req, err := http.NewRequest(method, base+path, rdr)
	if err != nil {
		return nil, err
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	return http.DefaultClient.Do(req)
}

func (a *App) wsURL() (string, error) {
	u, err := url.Parse(a.serverURL)
	if err != nil {
		return "", err
	}
	switch u.Scheme {
	case "https":
		u.Scheme = "wss"
	default:
		u.Scheme = "ws"
	}
	u.Path = "/ws"
	u.RawQuery = ""
	u.Fragment = ""
	return u.String(), nil
}

func (a *App) holdersLocked(lineID string) []document.Presence {
	out := make([]document.Presence, 0, len(a.cursors))
	for _, c := range a.cursors {
		if c.PersonID == a.personID || c.LineID != lineID {
			continue
		}
		out = append(out, document.Presence{Person: c.PersonID, Active: true})
	}
	return out
}
