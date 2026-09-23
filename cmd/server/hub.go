package main

import (
	"context"
	"encoding/json"
	"log"
	"net/http"
	"sync"
	"time"

	"github.com/KDreamelf/collaborative-editor/internal/document"
	"github.com/KDreamelf/collaborative-editor/internal/model"
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

func (c *wsClient) send(data []byte) {
	c.writeMu.Lock()
	defer c.writeMu.Unlock()
	_ = c.conn.WriteMessage(websocket.TextMessage, data)
}

type room struct {
	mu        sync.Mutex
	doc       *document.Doc
	joinOrder []string
	names     map[string]string
	clients   map[*wsClient]struct{}
	cursors   map[string]protocol.Cursor
	dirty     bool
}

type Hub struct {
	mu       sync.Mutex
	rooms    map[string]*room
	order    []string
	mongo    *mongoStore
	upgrader websocket.Upgrader
}

func NewHub(store *mongoStore) *Hub {
	h := &Hub{
		rooms: make(map[string]*room),
		mongo: store,
		upgrader: websocket.Upgrader{
			CheckOrigin: func(r *http.Request) bool { return true },
		},
	}
	if store != nil {
		h.loadAll()
	}
	return h
}

func (h *Hub) loadAll() {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	ids, err := h.mongo.listIDs(ctx)
	if err != nil {
		log.Printf("mongo list: %v", err)
		return
	}
	for _, id := range ids {
		doc, err := h.mongo.load(ctx, id)
		if err != nil {
			log.Printf("mongo load %s: %v", id, err)
			continue
		}
		h.rooms[id] = &room{
			doc:     doc,
			names:   map[string]string{},
			clients: map[*wsClient]struct{}{},
			cursors: map[string]protocol.Cursor{},
		}
		h.order = append(h.order, id)
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

func (r *room) holdersFor(lineID model.ID) []document.Presence {
	v, err := r.doc.View()
	if err != nil {
		return nil
	}
	suspended := map[string]bool{}
	for _, id := range v.Suspended {
		suspended[id] = true
	}
	disputeReal := map[string]model.ID{}
	personSuspendedOnLine := map[string]bool{}
	for _, d := range v.Disputes {
		disputeReal[d.ID.Hex()] = d.RealLine
		if d.RealLine == lineID && suspended[d.ID.Hex()] {
			personSuspendedOnLine[d.Person] = true
		}
	}
	var holders []document.Presence
	seen := map[string]bool{}
	for _, c := range r.cursors {
		onLine := false
		if c.LineID == lineID.Hex() {
			onLine = true
		}
		if c.DisputeID != "" {
			if real, ok := disputeReal[c.DisputeID]; ok && real == lineID {
				onLine = true
			}
		}
		if !onLine || seen[c.PersonID] {
			continue
		}
		seen[c.PersonID] = true
		holders = append(holders, document.Presence{
			Person: c.PersonID,
			Active: !personSuspendedOnLine[c.PersonID],
		})
	}
	return holders
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

func (r *room) toPerson(personID string, data []byte) []outbound {
	var out []outbound
	for c := range r.clients {
		if c.personID == personID {
			out = append(out, outbound{c, data})
		}
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

func (h *Hub) join(r *room, client *wsClient, personID, name string) []outbound {
	r.mu.Lock()
	defer r.mu.Unlock()

	client.personID = personID
	client.name = name
	r.clients[client] = struct{}{}
	r.names[personID] = name

	idx := r.personIndex(personID)
	if idx < 0 {
		idx = len(r.joinOrder)
		r.joinOrder = append(r.joinOrder, personID)
	}
	if err := r.doc.EnsureLines(idx + 1); err != nil {
		return []outbound{{client, mustJSON(protocol.ErrMsg{Type: protocol.TypeError, Message: err.Error()})}}
	}
	r.dirty = true

	v, err := r.doc.View()
	if err != nil {
		return []outbound{{client, mustJSON(protocol.ErrMsg{Type: protocol.TypeError, Message: err.Error()})}}
	}
	yourLine := v.Lines[idx].ID.Hex()
	if _, ok := r.cursors[personID]; !ok {
		r.cursors[personID] = protocol.Cursor{
			PersonID: personID,
			Name:     name,
			LineID:   yourLine,
			Offset:   0,
			SelEnd:   0,
		}
	}

	mine, err := r.buildSnapshot(yourLine)
	if err != nil {
		return []outbound{{client, mustJSON(protocol.ErrMsg{Type: protocol.TypeError, Message: err.Error()})}}
	}
	others, err := r.buildSnapshot("")
	if err != nil {
		return []outbound{{client, mustJSON(protocol.ErrMsg{Type: protocol.TypeError, Message: err.Error()})}}
	}
	msgs := []outbound{{client, mustJSON(mine)}}
	msgs = append(msgs, r.broadcastPayload(mustJSON(others), client)...)
	return msgs
}

func (h *Hub) applyBatch(r *room, client *wsClient, batch protocol.Batch) []outbound {
	r.mu.Lock()
	applied := 0
	var failMsg string
	for _, op := range batch.Ops {
		var err error
		switch op.Kind {
		case protocol.TypeSubmit:
			if op.Submit == nil {
				err = errString("缺少 submit")
				break
			}
			lineID, perr := model.ParseID(op.Submit.LineID)
			if perr != nil {
				err = perr
				break
			}
			err = r.doc.Submit(op.Submit.PersonID, lineID, op.Submit.Action, op.Submit.Content)
		case protocol.TypeDelete:
			if op.Delete == nil {
				err = errString("缺少 delete")
				break
			}
			lineID, perr := model.ParseID(op.Delete.LineID)
			if perr != nil {
				err = perr
				break
			}
			err = r.doc.DeleteIfIdle(op.Delete.PersonID, lineID, r.holdersFor(lineID))
		case protocol.TypeMerge:
			if op.Merge == nil {
				err = errString("缺少 merge")
				break
			}
			lineID, perr := model.ParseID(op.Merge.LineID)
			if perr != nil {
				err = perr
				break
			}
			err = r.doc.MergeUp(op.Merge.PersonID, lineID, r.holdersFor(lineID))
		default:
			err = errString("未知操作: " + op.Kind)
		}
		if err != nil {
			failMsg = err.Error()
			break
		}
		applied++
	}
	r.dirty = true
	ack := protocol.Ack{Type: protocol.TypeAck, Seq: batch.Seq, Applied: applied, Message: failMsg}
	snap, err := r.buildSnapshot("")
	var msgs []outbound
	if client != nil {
		msgs = append(msgs, outbound{client, mustJSON(ack)})
	}
	if err == nil {
		msgs = append(msgs, r.broadcastPayload(mustJSON(snap), nil)...)
	}
	r.mu.Unlock()
	return msgs
}

func (h *Hub) suspend(r *room, msg protocol.Suspend) []outbound {
	r.mu.Lock()
	defer r.mu.Unlock()
	lineID, err := model.ParseID(msg.LineID)
	if err != nil {
		return nil
	}
	if err := r.doc.SetSuspended(msg.PersonID, lineID, msg.Action, msg.Suspended); err != nil {
		return nil
	}
	r.dirty = true
	snap, err := r.buildSnapshot("")
	if err != nil {
		return nil
	}
	return r.broadcastPayload(mustJSON(snap), nil)
}

func (h *Hub) follow(r *room, client *wsClient, msg protocol.Follow) []outbound {
	r.mu.Lock()
	disputeID, err := model.ParseID(msg.DisputeID)
	var out document.FollowOutcome
	if err == nil {
		out, err = r.doc.RequestFollow(msg.PersonID, disputeID, msg.ClientTs)
	}
	var msgs []outbound
	if err != nil {
		if client != nil {
			msgs = append(msgs, outbound{client, mustJSON(protocol.ErrMsg{Type: protocol.TypeError, Message: err.Error()})})
		}
		r.mu.Unlock()
		return msgs
	}
	fromName := r.names[msg.PersonID]
	if client != nil && client.name != "" {
		fromName = client.name
	}
	if out.Status == document.FollowPending {
		ask := protocol.FollowAsk{
			Type:      protocol.TypeFollowAsk,
			FromID:    msg.PersonID,
			FromName:  fromName,
			DisputeID: msg.DisputeID,
			ClientTs:  msg.ClientTs,
		}
		msgs = append(msgs, r.toPerson(out.PeerID, mustJSON(ask))...)
	}
	if client != nil {
		msgs = append(msgs, outbound{client, mustJSON(protocol.FollowResult{
			Type:      protocol.TypeFollowResult,
			Status:    out.Status,
			DisputeID: msg.DisputeID,
		})})
	}
	if out.Status == document.FollowApplied || out.Status == document.FollowLost {
		r.dirty = true
		if out.PeerID != "" && out.PeerStatus != "" {
			msgs = append(msgs, r.toPerson(out.PeerID, mustJSON(protocol.FollowResult{
				Type:      protocol.TypeFollowResult,
				Status:    out.PeerStatus,
				DisputeID: out.DisputeID.Hex(),
			}))...)
		}
		if snap, serr := r.buildSnapshot(""); serr == nil {
			msgs = append(msgs, r.broadcastPayload(mustJSON(snap), nil)...)
		}
	}
	r.mu.Unlock()
	return msgs
}

func (h *Hub) followAnswer(r *room, msg protocol.FollowAnswer) []outbound {
	r.mu.Lock()
	disputeID, err := model.ParseID(msg.DisputeID)
	var out document.FollowOutcome
	if err == nil {
		out, err = r.doc.AnswerFollow(msg.PersonID, msg.FromID, disputeID, msg.Accept)
	}
	var msgs []outbound
	if err != nil {
		msgs = append(msgs, r.toPerson(msg.PersonID, mustJSON(protocol.ErrMsg{Type: protocol.TypeError, Message: err.Error()}))...)
		r.mu.Unlock()
		return msgs
	}
	resultSelf := protocol.FollowResult{Type: protocol.TypeFollowResult, Status: out.Status, DisputeID: msg.DisputeID}
	resultPeer := protocol.FollowResult{Type: protocol.TypeFollowResult, Status: out.PeerStatus, DisputeID: msg.DisputeID}
	msgs = append(msgs, r.toPerson(msg.PersonID, mustJSON(resultSelf))...)
	msgs = append(msgs, r.toPerson(out.PeerID, mustJSON(resultPeer))...)
	r.dirty = true
	if snap, serr := r.buildSnapshot(""); serr == nil {
		msgs = append(msgs, r.broadcastPayload(mustJSON(snap), nil)...)
	}
	r.mu.Unlock()
	return msgs
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
			sendAll(h.join(r, client, msg.PersonID, msg.Name))
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
			sendAll(h.applyBatch(joined, client, msg))
		case protocol.TypeSuspend:
			if joined == nil {
				continue
			}
			var msg protocol.Suspend
			if err := json.Unmarshal(data, &msg); err != nil {
				continue
			}
			sendAll(h.suspend(joined, msg))
		case protocol.TypeFollow:
			if joined == nil {
				continue
			}
			var msg protocol.Follow
			if err := json.Unmarshal(data, &msg); err != nil {
				continue
			}
			sendAll(h.follow(joined, client, msg))
		case protocol.TypeFollowAnswer:
			if joined == nil {
				continue
			}
			var msg protocol.FollowAnswer
			if err := json.Unmarshal(data, &msg); err != nil {
				continue
			}
			sendAll(h.followAnswer(joined, msg))
		case protocol.TypeCursor:
			if joined == nil {
				continue
			}
			var msg protocol.CursorMsg
			if err := json.Unmarshal(data, &msg); err != nil {
				continue
			}
			sendAll(h.setCursor(joined, msg.Cursor))
		default:
			client.send(mustJSON(protocol.ErrMsg{Type: protocol.TypeError, Message: "未知类型"}))
		}
	}
}
