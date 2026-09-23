package server

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/KDreamelf/collaborative-editor/internal/document"
	"github.com/KDreamelf/collaborative-editor/internal/protocol"
	"github.com/gorilla/websocket"
)

type fakeStore struct {
	mu      sync.Mutex
	ids     []string
	docs    map[string]*document.Doc
	listErr error
	loadErr error
	saveErr error
	saves   int
}

func (f *fakeStore) listIDs(ctx context.Context) ([]string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.listErr != nil {
		return nil, f.listErr
	}
	out := make([]string, len(f.ids))
	copy(out, f.ids)
	return out, nil
}

func (f *fakeStore) load(ctx context.Context, idHex string) (*document.Doc, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.loadErr != nil {
		return nil, f.loadErr
	}
	doc, ok := f.docs[idHex]
	if !ok {
		return nil, errString("文章不存在")
	}
	return doc, nil
}

func (f *fakeStore) save(ctx context.Context, v document.View) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.saves++
	if f.saveErr != nil {
		return f.saveErr
	}
	return nil
}

func TestConnectMongoKeepsClientWhenPingFails(t *testing.T) {
	// 未监听端口：Connect 仍成功，Ping 失败；修复前会 Disconnect 并返回 nil。
	store := connectMongo("mongodb://127.0.0.1:1", "editor_persist_test")
	if store == nil {
		t.Fatal("Ping 失败仍应保留 mongo client，供后续重连读写")
	}
	defer func() { _ = store.client.Disconnect(context.Background()) }()
	if store.client == nil || store.db == nil {
		t.Fatal("client/db 不应为空")
	}
}

func TestLoadMissingAddsOnlyAbsentRooms(t *testing.T) {
	keep := document.New("内存已有")
	keepID := keep.Article().ID.Hex()
	fromDB := document.New("库里补来")
	dbID := fromDB.Article().ID.Hex()

	fs := &fakeStore{
		ids:  []string{keepID, dbID},
		docs: map[string]*document.Doc{keepID: document.New("库里旧版"), dbID: fromDB},
	}
	h := NewHub(nil)
	h.mu.Lock()
	h.rooms[keepID] = &room{
		doc:     keep,
		names:   map[string]string{},
		clients: map[*wsClient]struct{}{},
		cursors: map[string]protocol.Cursor{},
		dirty:   true,
	}
	h.order = append(h.order, keepID)
	h.mongo = fs
	h.mu.Unlock()

	h.loadMissing()

	h.mu.Lock()
	defer h.mu.Unlock()
	if len(h.rooms) != 2 {
		t.Fatalf("应补进缺失文章: rooms=%d", len(h.rooms))
	}
	if h.rooms[keepID].doc.Article().Title != "内存已有" {
		t.Fatalf("已打开房间不得被库覆盖: %q", h.rooms[keepID].doc.Article().Title)
	}
	if h.rooms[dbID] == nil || h.rooms[dbID].doc.Article().Title != "库里补来" {
		t.Fatalf("缺失文章应加载: %+v", h.rooms[dbID])
	}
}

func TestLoadMissingListFailLeavesMemory(t *testing.T) {
	h := NewHub(nil)
	meta := h.CreateArticle("活着")
	fs := &fakeStore{listErr: errors.New("dial timeout")}
	h.mu.Lock()
	h.mongo = fs
	h.mu.Unlock()

	h.loadMissing()

	list := h.ListArticles()
	if len(list) != 1 || list[0].ID != meta.ID {
		t.Fatalf("list 失败不得清空内存文章: %+v", list)
	}
}

func TestLoadMissingPartialLoadKeepsOthers(t *testing.T) {
	okDoc := document.New("能读")
	okID := okDoc.Article().ID.Hex()
	badID := document.New("坏的").Article().ID.Hex()
	fs := &fakeStore{
		ids:  []string{badID, okID},
		docs: map[string]*document.Doc{okID: okDoc},
		// badID 不在 docs → load 返回错误，不得阻断 okID
	}
	h := NewHub(nil)
	h.mu.Lock()
	h.mongo = fs
	h.mu.Unlock()
	h.loadMissing()

	list := h.ListArticles()
	if len(list) != 1 || list[0].ID != okID {
		t.Fatalf("部分 load 失败仍应装入可读文章: %+v", list)
	}
}

func TestFlushDirtyRetainsFlagOnSaveFail(t *testing.T) {
	fs := &fakeStore{docs: map[string]*document.Doc{}, saveErr: errors.New("write concern failed")}
	h := NewHub(fs)
	meta := h.CreateArticle("待落盘")
	r := h.getRoom(meta.ID)
	if r == nil {
		t.Fatal("房间应存在")
	}
	r.mu.Lock()
	r.dirty = true
	r.mu.Unlock()

	h.flushDirty()

	r.mu.Lock()
	dirty := r.dirty
	r.mu.Unlock()
	fs.mu.Lock()
	saves := fs.saves
	fs.mu.Unlock()
	if saves != 1 {
		t.Fatalf("应尝试 save 一次，得到 %d", saves)
	}
	if !dirty {
		t.Fatal("save 失败后 dirty 须保留以便下轮重试")
	}
}

func TestHandleCreateRejectsInvalidJSON(t *testing.T) {
	h := NewHub(nil)
	mux := http.NewServeMux()
	mux.HandleFunc("POST /api/articles", h.handleCreate)

	before := len(h.ListArticles())
	req := httptest.NewRequest(http.MethodPost, "/api/articles", strings.NewReader(`{not-json`))
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("非法 JSON 应 400，得到 %d %s", rec.Code, rec.Body.String())
	}
	if len(h.ListArticles()) != before {
		t.Fatal("非法创建不得产生幽灵文章")
	}

	req = httptest.NewRequest(http.MethodPost, "/api/articles", strings.NewReader(``))
	rec = httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("空 body 应 400，得到 %d", rec.Code)
	}
	if len(h.ListArticles()) != before {
		t.Fatal("空 body 不得创建幽灵文章")
	}
}

func TestWSClientSendClosesBrokenConn(t *testing.T) {
	upgrader := websocket.Upgrader{CheckOrigin: func(r *http.Request) bool { return true }}
	accepted := make(chan *websocket.Conn, 1)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		accepted <- c
	}))
	defer srv.Close()

	url := "ws" + strings.TrimPrefix(srv.URL, "http")
	clientConn, _, err := websocket.DefaultDialer.Dial(url, nil)
	if err != nil {
		t.Fatal(err)
	}
	serverConn := <-accepted
	_ = serverConn.Close()

	c := &wsClient{conn: clientConn}
	done := make(chan struct{})
	go func() {
		c.send([]byte(`{"type":"ping"}`))
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("坏连接 send 应在写超时内返回并关闭，不得永久阻塞")
	}
	_ = clientConn.SetReadDeadline(time.Now().Add(200 * time.Millisecond))
	if _, _, err := clientConn.ReadMessage(); err == nil {
		t.Fatal("send 失败后应关闭连接")
	}
}
