package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/KDreamelf/collaborative-editor/internal/document"
	"github.com/KDreamelf/collaborative-editor/internal/model"
	"github.com/KDreamelf/collaborative-editor/internal/protocol"
	"github.com/gorilla/websocket"
)

func twoPersonDoc(t *testing.T, person string) (*document.Doc, string) {
	t.Helper()
	doc := document.New("t")
	v, err := doc.View()
	if err != nil || len(v.Lines) == 0 {
		t.Fatalf("空文档: %+v %v", v, err)
	}
	line := v.Lines[0].ID
	if err := doc.Submit("p1", line, model.ActionEdit, []string{"a"}); err != nil {
		t.Fatal(err)
	}
	if err := doc.Submit("p2", line, model.ActionEdit, []string{"b"}); err != nil {
		t.Fatal(err)
	}
	v, err = doc.View()
	if err != nil {
		t.Fatal(err)
	}
	var target string
	for _, d := range v.Disputes {
		if d.Person == person {
			target = d.ID.Hex()
		}
	}
	if target == "" {
		t.Fatal("找不到主张")
	}
	return doc, target
}

func TestJoinFailDoesNotMarkJoined(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "down", http.StatusBadGateway)
	}))
	t.Cleanup(srv.Close)

	app := NewAppWithStateDir(t.TempDir())
	app.serverURL = srv.URL
	if err := app.Join("abc", "甲"); err == nil {
		t.Fatal("初次 Join 失败应返回错误")
	}
	app.mu.Lock()
	defer app.mu.Unlock()
	if app.joined || app.articleID != "" {
		t.Fatalf("初次失败不该留下会话: joined=%v id=%q", app.joined, app.articleID)
	}
}

func TestHTTPDoTimeout(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		<-r.Context().Done()
	}))
	t.Cleanup(srv.Close)

	app := NewAppWithStateDir(t.TempDir())
	app.serverURL = srv.URL
	app.httpTimeout = 50 * time.Millisecond
	start := time.Now()
	_, err := app.ListArticles()
	elapsed := time.Since(start)
	if err == nil || err.Error() != "无法连接服务器" {
		t.Fatalf("应超时: %v", err)
	}
	if elapsed > 800*time.Millisecond {
		t.Fatalf("超时过慢: %v", elapsed)
	}
}

func TestConnectHandshakeTimeoutClearsDialing(t *testing.T) {
	hang := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		<-hang
	}))
	t.Cleanup(func() {
		close(hang)
		srv.Close()
	})

	app := NewAppWithStateDir(t.TempDir())
	app.serverURL = srv.URL
	app.httpTimeout = 50 * time.Millisecond
	app.articleID = "art"
	app.name = "甲"
	start := time.Now()
	err := app.connect()
	elapsed := time.Since(start)
	if err == nil || err.Error() != "无法连接服务器" {
		t.Fatalf("握手超时: %v", err)
	}
	if elapsed > 800*time.Millisecond {
		t.Fatalf("超时过慢: %v", elapsed)
	}
	app.mu.Lock()
	defer app.mu.Unlock()
	if app.dialing {
		t.Fatal("超时后 dialing 须清，避免永久 busy")
	}
	if app.conn != nil {
		t.Fatal("超时不得留下 conn")
	}
}

func TestOfflineFollowStaysInQueueInEditOrder(t *testing.T) {
	doc, target := twoPersonDoc(t, "p1")
	app := NewAppWithStateDir(t.TempDir())
	app.personID = "p2"
	app.articleID = "art"
	app.doc = doc
	v, _ := doc.View()
	line := v.Lines[0].ID.Hex()
	if err := app.SubmitEdit(line, "b2"); err != nil {
		t.Fatal(err)
	}
	if err := app.RequestFollow(target); err != nil {
		t.Fatal(err)
	}
	app.mu.Lock()
	defer app.mu.Unlock()
	if len(app.queue) != 2 {
		t.Fatalf("断线应留下编辑和追随: %d", len(app.queue))
	}
	if app.queue[0].Kind != protocol.TypeSubmit || app.queue[1].Kind != protocol.TypeFollow {
		t.Fatalf("顺序应是编辑再追随: %s %s", app.queue[0].Kind, app.queue[1].Kind)
	}
	if app.queue[0].ID == "" || app.queue[1].ID == "" {
		t.Fatal("可靠操作应带 ID")
	}
}

func TestReconnectRetriesUntilUp(t *testing.T) {
	var n atomic.Int32
	up := websocket.Upgrader{CheckOrigin: func(*http.Request) bool { return true }}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if n.Add(1) < 3 {
			http.Error(w, "down", http.StatusBadGateway)
			return
		}
		c, err := up.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		defer c.Close()
		_, _, _ = c.ReadMessage()
		for {
			if _, _, err := c.ReadMessage(); err != nil {
				return
			}
		}
	}))
	t.Cleanup(srv.Close)

	app := NewAppWithStateDir(t.TempDir())
	app.serverURL = srv.URL
	app.reconnectWait = time.Millisecond
	app.joined = true
	app.articleID = "art"
	app.name = "甲"
	t.Cleanup(func() {
		app.mu.Lock()
		app.articleID = ""
		app.joined = false
		c := app.conn
		app.mu.Unlock()
		if c != nil {
			_ = c.Close()
		}
	})
	done := make(chan error, 1)
	go func() { done <- func() error { app.reconnectLater(); return nil }() }()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("已加入后应持续重试直到连上")
	}
	if n.Load() < 3 {
		t.Fatalf("应多于一次重试，实际 %d", n.Load())
	}
	app.mu.Lock()
	defer app.mu.Unlock()
	if !app.joined || app.conn == nil {
		t.Fatal("连上后应保持 joined")
	}
}

func TestReconnectResendsFollowBatch(t *testing.T) {
	doc, target := twoPersonDoc(t, "p1")
	v0, _ := doc.View()
	followArt := v0.Article
	followLines := append([]model.Line(nil), v0.Lines...)
	followDisputes := append([]model.Dispute(nil), v0.Disputes...)
	got := make(chan protocol.Batch, 1)
	up := websocket.Upgrader{CheckOrigin: func(*http.Request) bool { return true }}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c, err := up.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		defer c.Close()
		_, _, err = c.ReadMessage()
		if err != nil {
			return
		}
		snap := protocol.Snapshot{
			Type: protocol.TypeSnapshot,
			View: document.View{
				Article:  followArt,
				Lines:    followLines,
				Disputes: followDisputes,
			},
		}
		if err := c.WriteJSON(snap); err != nil {
			return
		}
		_, data, err := c.ReadMessage()
		if err != nil {
			return
		}
		var batch protocol.Batch
		if err := json.Unmarshal(data, &batch); err != nil {
			return
		}
		select {
		case got <- batch:
		default:
		}
		_ = c.WriteJSON(protocol.Ack{Type: protocol.TypeAck, Seq: batch.Seq, Applied: len(batch.Ops)})
		for {
			if _, _, err := c.ReadMessage(); err != nil {
				return
			}
		}
	}))
	t.Cleanup(srv.Close)

	app := NewAppWithStateDir(t.TempDir())
	app.serverURL = srv.URL
	app.personID = "p2"
	app.doc = doc
	app.articleID = "art"
	app.name = "乙"
	if err := app.RequestFollow(target); err != nil {
		t.Fatal(err)
	}
	app.mu.Lock()
	if len(app.queue) != 1 || app.queue[0].Kind != protocol.TypeFollow {
		app.mu.Unlock()
		t.Fatal("断线追随应留在队列")
	}
	app.mu.Unlock()

	if err := app.connect(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		app.mu.Lock()
		app.articleID = ""
		app.joined = false
		c := app.conn
		app.mu.Unlock()
		if c != nil {
			_ = c.Close()
		}
	})
	select {
	case batch := <-got:
		if len(batch.Ops) != 1 || batch.Ops[0].Kind != protocol.TypeFollow || batch.Ops[0].ID == "" {
			t.Fatalf("重连应按队列重发追随: %+v", batch)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("重连后未收到追随 batch")
	}
}

func TestAfterSeenFromServerSnapshotOnly(t *testing.T) {
	doc := document.New("t")
	if err := doc.EnsureLines(2); err != nil {
		t.Fatal(err)
	}
	v, _ := doc.View()
	anchor, tail := v.Lines[0].ID, v.Lines[1].ID

	app := NewAppWithStateDir(t.TempDir())
	app.personID = "甲"
	app.articleID = "art"
	app.doc = doc
	app.rememberAfterSeenLocked(v.Lines)

	if err := app.SubmitInsert(anchor.Hex(), []string{"A1"}); err != nil {
		t.Fatal(err)
	}
	app.mu.Lock()
	defer app.mu.Unlock()
	if app.afterSeen[anchor.Hex()] != tail.Hex() {
		t.Fatalf("乐观插入不得改基准后继: got %q want %q", app.afterSeen[anchor.Hex()], tail.Hex())
	}
	if len(app.queue) != 1 || app.queue[0].Submit == nil {
		t.Fatal("应入队 submit")
	}
	s := app.queue[0].Submit
	if s.AfterSeen == nil || *s.AfterSeen != tail.Hex() {
		t.Fatalf("AfterSeen 应来自快照: %+v", s.AfterSeen)
	}
	if len(s.LineIDs) != 1 || s.LineIDs[0] == "" {
		t.Fatalf("应预生 LineIDs: %+v", s.LineIDs)
	}
	view2, err := app.doc.View()
	if err != nil {
		t.Fatal(err)
	}
	if len(view2.Lines) < 3 || view2.Lines[1].ID.Hex() != s.LineIDs[0] {
		t.Fatalf("本地新行 ID 应与包内一致: line=%s ids=%v", view2.Lines[1].ID.Hex(), s.LineIDs)
	}
}

func TestEditBaseFromServerSnapshotOnly(t *testing.T) {
	doc := document.New("t")
	v, _ := doc.View()
	line := v.Lines[0].ID

	app := NewAppWithStateDir(t.TempDir())
	app.personID = "乙"
	app.articleID = "art"
	app.doc = doc
	app.rememberAfterSeenLocked(v.Lines)

	if err := app.SubmitEdit(line.Hex(), "z"); err != nil {
		t.Fatal(err)
	}
	app.mu.Lock()
	defer app.mu.Unlock()
	if app.lineBase[line.Hex()] != "" {
		t.Fatalf("乐观改写不得污染基准正文: %+v", app.lineBase)
	}
	if len(app.queue) != 1 || app.queue[0].Submit == nil || app.queue[0].ID == "" {
		t.Fatalf("应入队且保留 Op.ID: %+v", app.queue)
	}
	s := app.queue[0].Submit
	if s.BaseContent == nil || *s.BaseContent != "" {
		t.Fatalf("BaseContent 应来自快照空串: %+v", s.BaseContent)
	}
}

func loadThreeLineDoc(t *testing.T) (*document.Doc, model.ID, model.ID, model.ID) {
	t.Helper()
	l1, l2, l3 := model.NewID(), model.NewID(), model.NewID()
	art := model.Article{ID: model.NewID(), Title: "span"}
	doc, err := document.Load(art, []model.Line{
		{ID: l1, Next: l2, Content: "L1"},
		{ID: l2, Prev: l1, Next: l3, Content: "L2"},
		{ID: l3, Prev: l2, Content: "L3"},
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	return doc, l1, l2, l3
}

func TestSpanEditOneOpFromServerSnap(t *testing.T) {
	doc, l1, l2, l3 := loadThreeLineDoc(t)
	v, err := doc.View()
	if err != nil {
		t.Fatal(err)
	}
	app := NewAppWithStateDir(t.TempDir())
	app.personID = "甲"
	app.articleID = "art"
	app.doc = doc
	app.rememberAfterSeenLocked(v.Lines)
	app.rememberServerSnapLocked(protocol.Snapshot{
		Type: protocol.TypeSnapshot,
		View: document.View{Article: v.Article, Lines: v.Lines},
	})
	// 污染 afterSeen/lineBase 映射；基准仍须只读 serverSnap.Lines
	app.lineBase[l1.Hex()] = "脏"
	app.afterSeen[l2.Hex()] = model.NewID().Hex()
	if err := app.SubmitSpanEdit(l1.Hex(), l2.Hex(), []string{"X", "Y"}); err != nil {
		t.Fatal(err)
	}
	app.mu.Lock()
	defer app.mu.Unlock()
	if len(app.queue) != 1 || app.queue[0].Kind != protocol.TypeSpanEdit || app.queue[0].ID == "" {
		t.Fatalf("一个范围一个 Op: %+v", app.queue)
	}
	se := app.queue[0].SpanEdit
	if se == nil || len(se.BaseIDs) != 2 || se.BaseIDs[0] != l1.Hex() || se.BaseIDs[1] != l2.Hex() {
		t.Fatalf("BaseIDs: %+v", se)
	}
	if se.BaseTexts[0] != "L1" || se.BaseTexts[1] != "L2" {
		t.Fatalf("基准正文须来自 serverSnap: %+v", se.BaseTexts)
	}
	if se.AfterSeen != l3.Hex() {
		t.Fatalf("AfterSeen 应为 end.Next: %q want %q", se.AfterSeen, l3.Hex())
	}
	if len(se.Replacement) != 2 || se.Replacement[0] != "X" || len(se.LineIDs) != 1 || se.LineIDs[0] == "" {
		t.Fatalf("Replacement/LineIDs: %+v", se)
	}
	after, err := app.doc.View()
	if err != nil {
		t.Fatal(err)
	}
	if len(after.Lines) != 3 || after.Lines[0].Content != "X" || after.Lines[1].Content != "Y" || after.Lines[2].Content != "L3" {
		t.Fatalf("本地正文: %+v", after.Lines)
	}
	if after.Lines[1].ID.Hex() != se.LineIDs[0] {
		t.Fatalf("预生 ID 一致: %s vs %s", after.Lines[1].ID.Hex(), se.LineIDs[0])
	}
}

func TestSpanEditRejectDoesNotEnqueue(t *testing.T) {
	doc, l1, _, _ := loadThreeLineDoc(t)
	v, _ := doc.View()
	app := NewAppWithStateDir(t.TempDir())
	app.personID = "甲"
	app.doc = doc
	app.rememberServerSnapLocked(protocol.Snapshot{
		Type: protocol.TypeSnapshot,
		View: document.View{Article: v.Article, Lines: v.Lines},
	})
	if err := app.SubmitSpanEdit(l1.Hex(), l1.Hex(), []string{"X"}); err == nil {
		t.Fatal("单行跨度应拒绝")
	}
	if err := app.SubmitSpanEdit(l1.Hex(), v.Lines[1].ID.Hex(), nil); err == nil {
		t.Fatal("空 replacement 应拒绝")
	}
	app.mu.Lock()
	defer app.mu.Unlock()
	if len(app.queue) != 0 {
		t.Fatalf("失败不得入队: %+v", app.queue)
	}
	after, _ := app.doc.View()
	if after.Lines[0].Content != "L1" || after.Lines[1].Content != "L2" {
		t.Fatalf("失败不得改正文: %+v", after.Lines)
	}
}

// 领域 ErrSpanBlocked：队列/正式 doc 不动；Replacement 进 rejected，重启可取回。
func TestSpanEditLocalBlockedSavesRejected(t *testing.T) {
	dir := t.TempDir()
	doc, l1, l2, _ := loadThreeLineDoc(t)
	snapView, err := doc.View()
	if err != nil {
		t.Fatal(err)
	}
	// 正式快照仍是 L1–L3；doc 另挂未决插入，领域拒整段。
	if err := doc.Submit("丙", l1, model.ActionInsert, []string{"ins"}); err != nil {
		t.Fatal(err)
	}
	before, err := doc.View()
	if err != nil {
		t.Fatal(err)
	}
	app := NewAppWithStateDir(dir)
	app.personID = "甲"
	app.articleID = "art-span-block"
	app.name = "甲"
	app.doc = doc
	app.rememberServerSnapLocked(protocol.Snapshot{
		Type: protocol.TypeSnapshot,
		View: document.View{Article: snapView.Article, Lines: snapView.Lines},
	})
	err = app.SubmitSpanEdit(l1.Hex(), l2.Hex(), []string{"KEEP", "ME"})
	if err == nil || err.Error() != msgLocalSpanReject {
		t.Fatalf("应稳定中文: %v", err)
	}
	// 连打同跨度只留最新一份
	err = app.SubmitSpanEdit(l1.Hex(), l2.Hex(), []string{"KEEP", "ME", "LATEST"})
	if err == nil || err.Error() != msgLocalSpanReject {
		t.Fatalf("二次拒绝: %v", err)
	}
	app.mu.Lock()
	if len(app.queue) != 0 {
		app.mu.Unlock()
		t.Fatalf("queue 不得增: %+v", app.queue)
	}
	if len(app.rejected) != 1 {
		app.mu.Unlock()
		t.Fatalf("同 BaseIDs 应覆盖: %+v", app.rejected)
	}
	se := app.rejected[0].Op.SpanEdit
	app.mu.Unlock()
	if se == nil || len(se.BaseIDs) != 2 || se.BaseIDs[0] != l1.Hex() || se.BaseIDs[1] != l2.Hex() {
		t.Fatalf("BaseIDs: %+v", se)
	}
	if len(se.Replacement) != 3 || se.Replacement[2] != "LATEST" || len(se.LineIDs) != 2 {
		t.Fatalf("Rejected 须含完整 Replacement/LineIDs: %+v", se)
	}
	after, err := app.doc.View()
	if err != nil {
		t.Fatal(err)
	}
	if len(after.Lines) != len(before.Lines) || len(after.Disputes) != len(before.Disputes) {
		t.Fatalf("doc 不得变: before=%+v after=%+v", before.Lines, after.Lines)
	}
	items := app.ListUnsynced()
	if len(items) != 1 || items[0].Text != "KEEP\nME\nLATEST" {
		t.Fatalf("ListUnsynced: %+v", items)
	}

	app2 := NewAppWithStateDir(dir)
	app2.mu.Lock()
	app2.articleID = "art-span-block"
	app2.restoreSessionLocked("art-span-block")
	app2.mu.Unlock()
	items2 := app2.ListUnsynced()
	if len(items2) != 1 || items2[0].Text != "KEEP\nME\nLATEST" {
		t.Fatalf("重启恢复: %+v", items2)
	}
	app2.mu.Lock()
	defer app2.mu.Unlock()
	if len(app2.rejected) != 1 || app2.rejected[0].Op.SpanEdit == nil {
		t.Fatalf("rejected 包: %+v", app2.rejected)
	}
	se2 := app2.rejected[0].Op.SpanEdit
	if len(se2.BaseIDs) != 2 || se2.BaseIDs[0] != l1.Hex() || len(se2.Replacement) != 3 {
		t.Fatalf("恢复字段不全: %+v", se2)
	}
}

// serverSnap 缺刚乐观插入的 end：不入队、doc 不动；原文进 rejected，重启可取回。
func TestSpanEditMissingSnapEndSavesRejected(t *testing.T) {
	dir := t.TempDir()
	doc, l1, l2, _ := loadThreeLineDoc(t)
	snapView, err := doc.View()
	if err != nil {
		t.Fatal(err)
	}
	app := NewAppWithStateDir(dir)
	app.personID = "甲"
	app.articleID = "art-span-miss-end"
	app.name = "甲"
	app.doc = doc
	app.rememberAfterSeenLocked(snapView.Lines)
	app.rememberServerSnapLocked(protocol.Snapshot{
		Type: protocol.TypeSnapshot,
		View: document.View{Article: snapView.Article, Lines: snapView.Lines},
	})
	if err := app.SubmitInsert(l2.Hex(), []string{"ins"}); err != nil {
		t.Fatal(err)
	}
	afterIns, err := app.doc.View()
	if err != nil {
		t.Fatal(err)
	}
	var endID string
	for _, ln := range afterIns.Lines {
		if ln.Content == "ins" {
			endID = ln.ID.Hex()
			break
		}
	}
	if endID == "" {
		t.Fatal("缺乐观插入行")
	}
	app.mu.Lock()
	qBefore := len(app.queue)
	app.mu.Unlock()

	err = app.SubmitSpanEdit(l1.Hex(), endID, []string{"KEEP", "ME"})
	if err == nil || err.Error() != msgLocalSpanReject {
		t.Fatalf("应稳定中文: %v", err)
	}
	app.mu.Lock()
	if len(app.queue) != qBefore {
		app.mu.Unlock()
		t.Fatalf("queue 不得增: before=%d after=%+v", qBefore, app.queue)
	}
	if len(app.rejected) != 1 {
		app.mu.Unlock()
		t.Fatalf("rejected: %+v", app.rejected)
	}
	se := app.rejected[0].Op.SpanEdit
	app.mu.Unlock()
	if se == nil || len(se.BaseIDs) != 2 || se.BaseIDs[0] != l1.Hex() || se.BaseIDs[1] != endID {
		t.Fatalf("草稿起止: %+v", se)
	}
	if len(se.Replacement) != 2 || se.Replacement[0] != "KEEP" || se.Replacement[1] != "ME" {
		t.Fatalf("Replacement: %+v", se)
	}
	if len(se.BaseTexts) != 0 || len(se.LineIDs) != 0 {
		t.Fatalf("最小草稿不得带 BaseTexts/LineIDs: %+v", se)
	}
	after, err := app.doc.View()
	if err != nil {
		t.Fatal(err)
	}
	if len(after.Lines) != len(afterIns.Lines) {
		t.Fatalf("doc 不得变: before=%+v after=%+v", afterIns.Lines, after.Lines)
	}
	for i := range after.Lines {
		if after.Lines[i].ID != afterIns.Lines[i].ID || after.Lines[i].Content != afterIns.Lines[i].Content {
			t.Fatalf("doc 不得变: before=%+v after=%+v", afterIns.Lines, after.Lines)
		}
	}
	items := app.ListUnsynced()
	if len(items) != 1 || items[0].Text != "KEEP\nME" {
		t.Fatalf("ListUnsynced: %+v", items)
	}

	app2 := NewAppWithStateDir(dir)
	app2.mu.Lock()
	app2.articleID = "art-span-miss-end"
	app2.restoreSessionLocked("art-span-miss-end")
	app2.mu.Unlock()
	items2 := app2.ListUnsynced()
	if len(items2) != 1 || items2[0].Text != "KEEP\nME" {
		t.Fatalf("重启恢复: %+v", items2)
	}
}

func TestSpanEditOfflineRestore(t *testing.T) {
	dir := t.TempDir()
	doc, l1, l2, l3 := loadThreeLineDoc(t)
	v, _ := doc.View()
	app1 := NewAppWithStateDir(dir)
	app1.personID = "pid-span"
	app1.articleID = "art-span"
	app1.name = "甲"
	app1.doc = doc
	app1.rememberAfterSeenLocked(v.Lines)
	app1.rememberServerSnapLocked(protocol.Snapshot{
		Type: protocol.TypeSnapshot,
		View: document.View{Article: v.Article, Lines: v.Lines, Disputes: v.Disputes},
	})
	if err := app1.SubmitSpanEdit(l1.Hex(), l2.Hex(), []string{"X", "Y"}); err != nil {
		t.Fatal(err)
	}
	app1.mu.Lock()
	opID := app1.queue[0].ID
	lineID := app1.queue[0].SpanEdit.LineIDs[0]
	_ = app1.savePersistLocked()
	app1.mu.Unlock()

	app2 := NewAppWithStateDir(dir)
	if app2.personID != "pid-span" {
		t.Fatalf("personID: %q", app2.personID)
	}
	app2.mu.Lock()
	app2.articleID = "art-span"
	app2.restoreSessionLocked("art-span")
	if len(app2.queue) != 1 || app2.queue[0].Kind != protocol.TypeSpanEdit || app2.queue[0].ID != opID {
		app2.mu.Unlock()
		t.Fatalf("队列恢复: %+v", app2.queue)
	}
	se := app2.queue[0].SpanEdit
	app2.mu.Unlock()
	if se == nil || se.AfterSeen != l3.Hex() || len(se.LineIDs) != 1 || se.LineIDs[0] != lineID {
		t.Fatalf("SpanEdit 包应完整: %+v", se)
	}
	after, err := app2.doc.View()
	if err != nil {
		t.Fatal(err)
	}
	if len(after.Lines) != 3 || after.Lines[0].Content != "X" || after.Lines[1].Content != "Y" || after.Lines[1].ID.Hex() != lineID {
		t.Fatalf("离线重放正文: %+v", after.Lines)
	}
	if text := opPlainText(app2.queue[0]); text != "X\nY" {
		t.Fatalf("opPlainText: %q", text)
	}
}

func TestSubmitEditClaimSetsWholeClaim(t *testing.T) {
	doc := document.New("t")
	v, _ := doc.View()
	line := v.Lines[0].ID
	app := NewAppWithStateDir(t.TempDir())
	app.personID = "甲"
	app.articleID = "art"
	app.doc = doc
	app.rememberAfterSeenLocked(v.Lines)

	if err := app.SubmitPaste(line.Hex(), []string{"P", "Q"}); err != nil {
		t.Fatal(err)
	}
	app.mu.Lock()
	if app.queue[0].Submit == nil || app.queue[0].Submit.WholeClaim {
		app.mu.Unlock()
		t.Fatalf("SubmitPaste 默认 WholeClaim=false: %+v", app.queue[0].Submit)
	}
	app.mu.Unlock()

	if err := app.SubmitEdit(line.Hex(), "R"); err != nil {
		t.Fatal(err)
	}
	after, _ := app.doc.View()
	if len(after.Lines) != 2 || after.Lines[0].Content != "R" || after.Lines[1].Content != "Q" {
		t.Fatalf("普通 Edit 保留尾: %+v", after.Lines)
	}
	app.mu.Lock()
	if app.queue[1].Submit == nil || app.queue[1].Submit.WholeClaim {
		app.mu.Unlock()
		t.Fatalf("SubmitEdit 默认 WholeClaim=false: %+v", app.queue[1].Submit)
	}
	app.mu.Unlock()

	if err := app.SubmitEditClaim(line.Hex(), []string{"R"}); err != nil {
		t.Fatal(err)
	}
	sh, _ := app.doc.View()
	if len(sh.Lines) != 1 || sh.Lines[0].Content != "R" {
		t.Fatalf("WholeClaim 缩段删 Q: %+v", sh.Lines)
	}
	app.mu.Lock()
	defer app.mu.Unlock()
	s := app.queue[2].Submit
	if s == nil || !s.WholeClaim {
		t.Fatalf("SubmitEditClaim 须 WholeClaim=true: %+v", s)
	}
	raw, err := json.Marshal(s)
	if err != nil {
		t.Fatal(err)
	}
	var back protocol.Submit
	if err := json.Unmarshal(raw, &back); err != nil {
		t.Fatal(err)
	}
	if !back.WholeClaim {
		t.Fatalf("JSON 往返丢 WholeClaim: %s", raw)
	}
}

func TestApplyOpReplayWholeClaim(t *testing.T) {
	doc := document.New("t")
	v, _ := doc.View()
	line := v.Lines[0].ID
	qID := model.NewID()
	if err := doc.SubmitWith("甲", line, model.ActionEdit, []string{"P", "Q"}, document.SubmitOpts{
		BaseContent: strPtr(""),
		LineIDs:     []model.ID{qID},
	}); err != nil {
		t.Fatal(err)
	}
	app := NewAppWithStateDir(t.TempDir())
	app.personID = "甲"
	app.doc = doc
	op := protocol.Op{
		ID:   model.NewID().Hex(),
		Kind: protocol.TypeSubmit,
		Submit: &protocol.Submit{
			Type:       protocol.TypeSubmit,
			PersonID:   "甲",
			LineID:     line.Hex(),
			Action:     model.ActionEdit,
			Content:    []string{"R"},
			WholeClaim: true,
			BaseContent: strPtr("P"),
		},
	}
	app.mu.Lock()
	defer app.mu.Unlock()
	if err := app.applyOpLocked(op); err != nil {
		t.Fatal(err)
	}
	after, err := app.doc.View()
	if err != nil {
		t.Fatal(err)
	}
	if len(after.Lines) != 1 || after.Lines[0].Content != "R" {
		t.Fatalf("回放 WholeClaim 应删 Q: %+v", after.Lines)
	}
}

func TestBeforeSeenFromServerSnapshotOnly(t *testing.T) {
	doc := document.New("t")
	if err := doc.EnsureLines(2); err != nil {
		t.Fatal(err)
	}
	v, _ := doc.View()
	l1, l2 := v.Lines[0].ID, v.Lines[1].ID

	app := NewAppWithStateDir(t.TempDir())
	app.personID = "甲"
	app.articleID = "art"
	app.doc = doc
	app.rememberAfterSeenLocked(v.Lines)

	if err := app.SubmitInsertBefore(l2.Hex(), []string{"B1"}); err != nil {
		t.Fatal(err)
	}
	app.mu.Lock()
	defer app.mu.Unlock()
	if app.beforeSeen[l2.Hex()] != l1.Hex() {
		t.Fatalf("乐观插入不得改基准前驱: got %q want %q", app.beforeSeen[l2.Hex()], l1.Hex())
	}
	if len(app.queue) != 1 || app.queue[0].Submit == nil {
		t.Fatal("应入队 submit")
	}
	s := app.queue[0].Submit
	if s.Action != model.ActionInsertBefore {
		t.Fatalf("Action: %q", s.Action)
	}
	if s.BeforeSeen == nil || *s.BeforeSeen != l1.Hex() {
		t.Fatalf("BeforeSeen 应来自快照: %+v want %s", s.BeforeSeen, l1.Hex())
	}
	if s.AfterSeen != nil {
		t.Fatalf("插在前面不得带 AfterSeen: %+v", s.AfterSeen)
	}
	if len(s.LineIDs) != 1 || s.LineIDs[0] == "" {
		t.Fatalf("应预生 LineIDs: %+v", s.LineIDs)
	}
	if text := opPlainText(app.queue[0]); text != "B1" {
		t.Fatalf("opPlainText 须识别 before 原文: %q", text)
	}
}

func TestBeforeSeenDocStartEmptyString(t *testing.T) {
	doc := document.New("t")
	v, _ := doc.View()
	head := v.Lines[0].ID

	app := NewAppWithStateDir(t.TempDir())
	app.personID = "甲"
	app.articleID = "art"
	app.doc = doc
	app.rememberAfterSeenLocked(v.Lines)

	if err := app.SubmitInsertBefore(head.Hex(), []string{"HEAD"}); err != nil {
		t.Fatal(err)
	}
	app.mu.Lock()
	defer app.mu.Unlock()
	s := app.queue[0].Submit
	if s == nil || s.BeforeSeen == nil || *s.BeforeSeen != "" {
		t.Fatalf("文首 BeforeSeen 须非 nil 空串: %+v", s)
	}
}

func TestAfterSeenStillFromServerSnapshot(t *testing.T) {
	doc := document.New("t")
	if err := doc.EnsureLines(2); err != nil {
		t.Fatal(err)
	}
	v, _ := doc.View()
	anchor, tail := v.Lines[0].ID, v.Lines[1].ID

	app := NewAppWithStateDir(t.TempDir())
	app.personID = "甲"
	app.articleID = "art"
	app.doc = doc
	app.rememberAfterSeenLocked(v.Lines)

	if err := app.SubmitInsert(anchor.Hex(), []string{"A1"}); err != nil {
		t.Fatal(err)
	}
	app.mu.Lock()
	defer app.mu.Unlock()
	s := app.queue[0].Submit
	if s.AfterSeen == nil || *s.AfterSeen != tail.Hex() {
		t.Fatalf("旧 after 不变: %+v", s.AfterSeen)
	}
	if s.BeforeSeen != nil {
		t.Fatalf("插在后面不得带 BeforeSeen: %+v", s.BeforeSeen)
	}
}

func strPtr(s string) *string { return &s }
