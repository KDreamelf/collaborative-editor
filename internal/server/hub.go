package server

import (
	"context"
	"encoding/json"
	"log"
	"net/http"
	"sync"
	"time"

	"github.com/KDreamelf/collaborative-editor/internal/document"
	"github.com/KDreamelf/collaborative-editor/internal/protocol"
	"github.com/gorilla/websocket"
	"go.mongodb.org/mongo-driver/mongo"
)

type articleMeta struct {
	ID    string `json:"id"`
	Title string `json:"title"`
}

type wsClient struct {
	conn     *websocket.Conn
	writeMu  sync.Mutex
	personID string
	name     string
}

const wsWriteWait = 5 * time.Second

func (c *wsClient) send(data []byte) {
	if c == nil || c.conn == nil {
		return
	}
	c.writeMu.Lock()
	defer c.writeMu.Unlock()
	_ = c.conn.SetWriteDeadline(time.Now().Add(wsWriteWait))
	if err := c.conn.WriteMessage(websocket.TextMessage, data); err != nil {
		_ = c.conn.Close()
	}
}

type room struct {
	mu        sync.Mutex
	doc       *document.Doc
	joinOrder []string
	names     map[string]string
	clients   map[*wsClient]struct{}
	cursors   map[string]protocol.Cursor
	// ponytail: 仅进程内去重；若实际需要跨服务重启幂等，再持久化近期 ID。
	seenPayload map[string][32]byte
	dirty       bool
}

type Hub struct {
	mu       sync.Mutex
	rooms    map[string]*room
	order    []string
	mongo    articleStore
	upgrader websocket.Upgrader
}

func NewHub(store articleStore) *Hub {
	h := &Hub{
		rooms: make(map[string]*room),
		mongo: store,
		upgrader: websocket.Upgrader{
			CheckOrigin: func(r *http.Request) bool { return true },
		},
	}
	if store != nil {
		h.loadMissing()
	}
	return h
}

// loadMissing 从库补尚未在内存的文章。只添加，不覆盖已打开/已改房间。
// list/load 失败留给下轮；读失败不当成空集合。网络 I/O 不持全局锁。
func (h *Hub) loadMissing() {
	if h.mongo == nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	ids, err := h.mongo.listIDs(ctx)
	if err != nil {
		log.Printf("mongo list: %v", err)
		return
	}
	h.mu.Lock()
	need := make([]string, 0, len(ids))
	for _, id := range ids {
		if _, ok := h.rooms[id]; !ok {
			need = append(need, id)
		}
	}
	h.mu.Unlock()

	type loaded struct {
		id  string
		doc *document.Doc
	}
	var batch []loaded
	for _, id := range need {
		doc, err := h.mongo.load(ctx, id)
		if err != nil {
			if err != mongo.ErrNoDocuments {
				log.Printf("mongo load %s: %v", id, err)
			}
			continue
		}
		batch = append(batch, loaded{id, doc})
	}
	if len(batch) == 0 {
		return
	}

	h.mu.Lock()
	defer h.mu.Unlock()
	for _, p := range batch {
		if _, ok := h.rooms[p.id]; ok {
			continue
		}
		h.rooms[p.id] = &room{
			doc:     p.doc,
			names:   map[string]string{},
			clients: map[*wsClient]struct{}{},
			cursors: map[string]protocol.Cursor{},
		}
		h.order = append(h.order, p.id)
	}
}

func (h *Hub) StartFlush(stop <-chan struct{}) {
	if h.mongo == nil {
		return
	}
	t := time.NewTicker(2 * time.Second)
	defer t.Stop()
	for {
		select {
		case <-stop:
			return
		case <-t.C:
			h.loadMissing()
			h.flushDirty()
		}
	}
}

func (h *Hub) flushDirty() {
	h.mu.Lock()
	type pair struct {
		id   string
		view document.View
	}
	var batch []pair
	for id, r := range h.rooms {
		r.mu.Lock()
		if r.dirty {
			view, err := r.doc.View()
			if err != nil {
				log.Printf("snapshot %s: %v", id, err)
			} else {
				batch = append(batch, pair{id, view})
				r.dirty = false
			}
		}
		r.mu.Unlock()
	}
	h.mu.Unlock()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	for _, p := range batch {
		if err := h.mongo.save(ctx, p.view); err != nil {
			log.Printf("mongo save %s: %v", p.id, err)
			h.mu.Lock()
			if r := h.rooms[p.id]; r != nil {
				r.mu.Lock()
				r.dirty = true
				r.mu.Unlock()
			}
			h.mu.Unlock()
		}
	}
}

func (h *Hub) CreateArticle(title string) articleMeta {
	if title == "" {
		title = "未命名"
	}
	doc := document.New(title)
	meta := articleMeta{ID: doc.Article().ID.Hex(), Title: title}
	h.mu.Lock()
	h.rooms[meta.ID] = &room{
		doc:     doc,
		names:   map[string]string{},
		clients: map[*wsClient]struct{}{},
		cursors: map[string]protocol.Cursor{},
		dirty:   true,
	}
	h.order = append(h.order, meta.ID)
	h.mu.Unlock()
	return meta
}

func (h *Hub) ListArticles() []articleMeta {
	h.mu.Lock()
	defer h.mu.Unlock()
	out := make([]articleMeta, 0, len(h.order))
	for _, id := range h.order {
		r := h.rooms[id]
		if r == nil {
			continue
		}
		r.mu.Lock()
		out = append(out, articleMeta{ID: id, Title: r.doc.Article().Title})
		r.mu.Unlock()
	}
	return out
}

func (h *Hub) getRoom(id string) *room {
	h.mu.Lock()
	r := h.rooms[id]
	h.mu.Unlock()
	if r != nil {
		return r
	}
	if h.mongo == nil {
		return nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	doc, err := h.mongo.load(ctx, id)
	if err != nil {
		if err != mongo.ErrNoDocuments {
			log.Printf("mongo load %s: %v", id, err)
		}
		return nil
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	if r = h.rooms[id]; r != nil {
		return r
	}
	r = &room{
		doc:     doc,
		names:   map[string]string{},
		clients: map[*wsClient]struct{}{},
		cursors: map[string]protocol.Cursor{},
	}
	h.rooms[id] = r
	found := false
	for _, existing := range h.order {
		if existing == id {
			found = true
			break
		}
	}
	if !found {
		h.order = append(h.order, id)
	}
	return r
}

func (h *Hub) GetView(id string) (document.View, error) {
	r := h.getRoom(id)
	if r == nil {
		return document.View{}, errNotFound
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.doc.View()
}

var errNotFound = errString("文章不存在")

type errString string

func (e errString) Error() string { return string(e) }

func (r *room) personIndex(personID string) int {
	for i, id := range r.joinOrder {
		if id == personID {
			return i
		}
	}
	return -1
}

func (r *room) onlinePeople() []protocol.Person {
	seen := map[string]bool{}
	var out []protocol.Person
	for c := range r.clients {
		if seen[c.personID] {
			continue
		}
		seen[c.personID] = true
		out = append(out, protocol.Person{ID: c.personID, Name: c.name})
	}
	return out
}

func (r *room) cursorList() []protocol.Cursor {
	out := make([]protocol.Cursor, 0, len(r.cursors))
	for _, c := range r.cursors {
		out = append(out, c)
	}
	return out
}

func (r *room) buildSnapshot(yourLine string) (protocol.Snapshot, error) {
	v, err := r.doc.View()
	if err != nil {
		return protocol.Snapshot{}, err
	}
	return protocol.Snapshot{
		Type:     protocol.TypeSnapshot,
		YourLine: yourLine,
		View:     v,
		People:   r.onlinePeople(),
		Cursors:  r.cursorList(),
	}, nil
}

type outbound struct {
	client *wsClient
	data   []byte
}

func sendAll(msgs []outbound) {
	for _, m := range msgs {
		if m.client != nil {
			m.client.send(m.data)
		}
	}
}

func (r *room) broadcastPayload(data []byte, except *wsClient) []outbound {
	var out []outbound
	for c := range r.clients {
		if c == except {
			continue
		}
		out = append(out, outbound{c, data})
	}
	return out
}

func mustJSON(v any) []byte {
	b, err := json.Marshal(v)
	if err != nil {
		panic(err)
	}
	return b
}

type cursorsMsg struct {
	Type    string            `json:"type"`
	Cursors []protocol.Cursor `json:"cursors"`
}

func (h *Hub) setCursor(r *room, cur protocol.Cursor) []outbound {
	r.mu.Lock()
	if cur.Name == "" {
		cur.Name = r.names[cur.PersonID]
	}
	r.cursors[cur.PersonID] = cur
	payload := mustJSON(cursorsMsg{Type: protocol.TypeCursors, Cursors: r.cursorList()})
	msgs := r.broadcastPayload(payload, nil)
	r.mu.Unlock()
	return msgs
}

func (h *Hub) leave(r *room, client *wsClient) []outbound {
	r.mu.Lock()
	delete(r.clients, client)
	if client.personID != "" {
		delete(r.cursors, client.personID)
	}
	payload := mustJSON(cursorsMsg{Type: protocol.TypeCursors, Cursors: r.cursorList()})
	msgs := r.broadcastPayload(payload, nil)
	r.mu.Unlock()
	return msgs
}

func (h *Hub) handleWS(w http.ResponseWriter, req *http.Request) {
	conn, err := h.upgrader.Upgrade(w, req, nil)
	if err != nil {
		return
	}
	client := &wsClient{conn: conn}
	var joined *room
	defer func() {
		if joined != nil {
			sendAll(h.leave(joined, client))
		}
		_ = conn.Close()
	}()
	for {
		_, data, err := conn.ReadMessage()
		if err != nil {
			return
		}
		var head struct {
			Type string `json:"type"`
		}
		if err := json.Unmarshal(data, &head); err != nil {
			client.send(mustJSON(protocol.ErrMsg{Type: protocol.TypeError, Message: "坏消息"}))
			continue
		}
		switch head.Type {
		case protocol.TypeJoin:
			var msg protocol.Join
			if err := json.Unmarshal(data, &msg); err != nil {
				client.send(mustJSON(protocol.ErrMsg{Type: protocol.TypeError, Message: "坏消息"}))
				continue
			}
			r := h.getRoom(msg.ArticleID)
			if r == nil {
				client.send(mustJSON(protocol.ErrMsg{Type: protocol.TypeError, Message: "文章不存在"}))
				continue
			}
			if joined != nil && joined != r {
				sendAll(h.leave(joined, client))
			}
			joined = r
			sendAll(h.neutralJoin(r, client, msg.PersonID, msg.Name))
		case protocol.TypeBatch:
			if joined == nil {
				client.send(mustJSON(protocol.ErrMsg{Type: protocol.TypeError, Message: "未加入文章"}))
				continue
			}
			var msg protocol.Batch
			if err := json.Unmarshal(data, &msg); err != nil {
				client.send(mustJSON(protocol.ErrMsg{Type: protocol.TypeError, Message: "坏消息"}))
				continue
			}
			sendAll(h.neutralBatch(joined, client, msg))
		case protocol.TypeSuspend, protocol.TypeFollow, protocol.TypeFollowAnswer:
			// 正式客户端统一走 Batch；裸消息忽略，不入旧裁决入口。
			continue
		case protocol.TypeCursor:
			if joined == nil {
				continue
			}
			var msg protocol.CursorMsg
			if err := json.Unmarshal(data, &msg); err != nil {
				continue
			}
			// 强制用已 Join 身份，防光标冒充。
			msg.Cursor.PersonID = client.personID
			msg.Cursor.Name = client.name
			sendAll(h.setCursor(joined, msg.Cursor))
		default:
			client.send(mustJSON(protocol.ErrMsg{Type: protocol.TypeError, Message: "未知类型"}))
		}
	}
}
