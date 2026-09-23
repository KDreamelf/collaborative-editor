package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/KDreamelf/collaborative-editor/internal/document"
	"github.com/KDreamelf/collaborative-editor/internal/model"
	"github.com/KDreamelf/collaborative-editor/internal/protocol"
	"github.com/gorilla/websocket"
)

// blockPersistDir：stateDir 指到普通文件，MkdirAll 失败，模拟真实写盘阻断。
func blockPersistDir(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "not-a-dir")
	if err := os.WriteFile(path, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestPersistPersonIDAndQueueSurviveRestart(t *testing.T) {
	dir := t.TempDir()
	doc := document.New("t")
	v, _ := doc.View()
	line := v.Lines[0].ID.Hex()

	app1 := NewAppWithStateDir(dir)
	app1.personID = "pid-stable"
	app1.articleID = "art-a"
	app1.name = "甲"
	app1.doc = doc
	app1.rememberAfterSeenLocked(v.Lines)
	app1.rememberServerSnapLocked(protocol.Snapshot{
		Type: protocol.TypeSnapshot,
		View: document.View{Article: v.Article, Lines: v.Lines, Disputes: v.Disputes},
	})
	if err := app1.SubmitEdit(line, "离线草稿"); err != nil {
		t.Fatal(err)
	}
	app1.mu.Lock()
	if len(app1.queue) != 1 || app1.queue[0].Submit == nil {
		app1.mu.Unlock()
		t.Fatal("应入队")
	}
	opID := app1.queue[0].ID
	lineIDs := append([]string(nil), app1.queue[0].Submit.LineIDs...)
	content := append([]string(nil), app1.queue[0].Submit.Content...)
	_ = app1.savePersistLocked()
	app1.mu.Unlock()

	if _, err := os.Stat(filepath.Join(dir, persistFileName)); err != nil {
		t.Fatalf("应写出状态文件: %v", err)
	}

	app2 := NewAppWithStateDir(dir)
	if app2.personID != "pid-stable" {
		t.Fatalf("personID 应恢复: %q", app2.personID)
	}
	app2.mu.Lock()
	app2.articleID = "art-a"
	app2.restoreSessionLocked("art-a")
	if len(app2.queue) != 1 {
		app2.mu.Unlock()
		t.Fatalf("队列应恢复: %d", len(app2.queue))
	}
	op := app2.queue[0]
	app2.mu.Unlock()
	if op.ID != opID {
		t.Fatalf("Op.ID 不变: got %q want %q", op.ID, opID)
	}
	if op.Submit == nil || op.Submit.Content[0] != "离线草稿" {
		t.Fatalf("Content 应保留: %+v", op.Submit)
	}
	if len(lineIDs) > 0 {
		if len(op.Submit.LineIDs) != len(lineIDs) || op.Submit.LineIDs[0] != lineIDs[0] {
			t.Fatalf("LineIDs 应不变: %+v vs %+v", op.Submit.LineIDs, lineIDs)
		}
	}
	if content[0] != "离线草稿" {
		t.Fatal("对照内容丢失")
	}
}

func TestPersistRejectedKeepsText(t *testing.T) {
	dir := t.TempDir()
	gotBatch := make(chan protocol.Batch, 1)
	up := websocket.Upgrader{CheckOrigin: func(*http.Request) bool { return true }}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c, err := up.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		defer c.Close()
		_, _, _ = c.ReadMessage()
		lineID := model.NewID()
		art := model.Article{ID: model.NewID(), Title: "t"}
		snap := protocol.Snapshot{
			Type: protocol.TypeSnapshot,
			View: document.View{Article: art, Lines: []model.Line{{ID: lineID, Content: "base"}}},
		}
		_ = c.WriteJSON(snap)
		_, data, err := c.ReadMessage()
		if err != nil {
			return
		}
		var batch protocol.Batch
		if err := json.Unmarshal(data, &batch); err != nil {
			return
		}
		gotBatch <- batch
		_ = c.WriteJSON(protocol.Ack{
			Type:    protocol.TypeAck,
			Seq:     batch.Seq,
			Applied: 0,
			Message: "服务端拒绝此步",
		})
		for {
			if _, _, err := c.ReadMessage(); err != nil {
				return
			}
		}
	}))
	t.Cleanup(srv.Close)

	app := NewAppWithStateDir(dir)
	app.serverURL = srv.URL
	app.personID = "p1"
	app.articleID = "art-rej"
	app.name = "甲"
	app.doc = document.New("t")
	v, _ := app.doc.View()
	line := v.Lines[0].ID.Hex()
	if err := app.SubmitEdit(line, "拒绝仍可取回"); err != nil {
		t.Fatal(err)
	}

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
	case <-gotBatch:
	case <-time.After(2 * time.Second):
		t.Fatal("未收到 batch")
	}
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		app.mu.Lock()
		n := len(app.rejected)
		q := len(app.queue)
		app.mu.Unlock()
		if n == 1 && q == 0 {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	items := app.ListUnsynced()
	if len(items) != 1 {
		t.Fatalf("应有未同步项: %+v", items)
	}
	if items[0].Text != "拒绝仍可取回" {
		t.Fatalf("原文应可取回: %q", items[0].Text)
	}
	if items[0].Summary == "" {
		t.Fatal("应有自然提醒")
	}

	app2 := NewAppWithStateDir(dir)
	app2.mu.Lock()
	app2.restoreSessionLocked("art-rej")
	app2.mu.Unlock()
	items2 := app2.ListUnsynced()
	if len(items2) != 1 || items2[0].Text != "拒绝仍可取回" {
		t.Fatalf("重启后原文仍在: %+v", items2)
	}
}

func TestPersistQueueIsolatedByArticle(t *testing.T) {
	dir := t.TempDir()
	docA := document.New("A")
	va, _ := docA.View()
	app := NewAppWithStateDir(dir)
	app.personID = "pid"
	app.articleID = "art-a"
	app.name = "甲"
	app.doc = docA
	app.rememberServerSnapLocked(protocol.Snapshot{
		Type: protocol.TypeSnapshot,
		View: document.View{Article: va.Article, Lines: va.Lines},
	})
	if err := app.SubmitEdit(va.Lines[0].ID.Hex(), "只属于A"); err != nil {
		t.Fatal(err)
	}
	app.mu.Lock()
	opA := app.queue[0].ID
	_ = app.savePersistLocked()
	app.mu.Unlock()

	// 切到 B：A 的队列不得带过去
	docB := document.New("B")
	vb, _ := docB.View()
	app.mu.Lock()
	_ = app.savePersistLocked()
	app.articleID = "art-b"
	app.restoreSessionLocked("art-b")
	app.doc = docB
	app.rememberServerSnapLocked(protocol.Snapshot{
		Type: protocol.TypeSnapshot,
		View: document.View{Article: vb.Article, Lines: vb.Lines},
	})
	app.mu.Unlock()
	if err := app.SubmitEdit(vb.Lines[0].ID.Hex(), "只属于B"); err != nil {
		t.Fatal(err)
	}
	app.mu.Lock()
	if len(app.queue) != 1 || app.queue[0].Submit.Content[0] != "只属于B" {
		app.mu.Unlock()
		t.Fatalf("B 队列串了 A: %+v", app.queue)
	}
	for _, op := range app.queue {
		if op.ID == opA {
			app.mu.Unlock()
			t.Fatal("A 的 Op 出现在 B 队列")
		}
	}
	_ = app.savePersistLocked()
	app.mu.Unlock()

	app2 := NewAppWithStateDir(dir)
	app2.mu.Lock()
	app2.restoreSessionLocked("art-a")
	if len(app2.queue) != 1 || app2.queue[0].ID != opA {
		app2.mu.Unlock()
		t.Fatalf("A 队列应独立恢复: %+v", app2.queue)
	}
	app2.restoreSessionLocked("art-b")
	if len(app2.queue) != 1 || app2.queue[0].Submit.Content[0] != "只属于B" {
		app2.mu.Unlock()
		t.Fatalf("B 队列应独立: %+v", app2.queue)
	}
	app2.mu.Unlock()
}

func TestPersistResendAfterRestartKeepsIDs(t *testing.T) {
	dir := t.TempDir()
	got := make(chan protocol.Batch, 1)
	up := websocket.Upgrader{CheckOrigin: func(*http.Request) bool { return true }}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c, err := up.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		defer c.Close()
		_, _, _ = c.ReadMessage()
		lineID := model.NewID()
		snap := protocol.Snapshot{
			Type: protocol.TypeSnapshot,
			View: document.View{
				Article: model.Article{ID: model.NewID(), Title: "t"},
				Lines:   []model.Line{{ID: lineID, Content: ""}},
			},
		}
		_ = c.WriteJSON(snap)
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

	doc := document.New("t")
	v, _ := doc.View()
	app1 := NewAppWithStateDir(dir)
	app1.serverURL = srv.URL
	app1.personID = "pid-resend"
	app1.articleID = "art-resend"
	app1.name = "甲"
	app1.doc = doc
	app1.rememberAfterSeenLocked(v.Lines)
	app1.rememberServerSnapLocked(protocol.Snapshot{
		Type: protocol.TypeSnapshot,
		View: document.View{Article: v.Article, Lines: v.Lines},
	})
	if err := app1.SubmitInsert(v.Lines[0].ID.Hex(), []string{"重发段"}); err != nil {
		t.Fatal(err)
	}
	app1.mu.Lock()
	wantID := app1.queue[0].ID
	wantLines := append([]string(nil), app1.queue[0].Submit.LineIDs...)
	_ = app1.savePersistLocked()
	app1.mu.Unlock()

	app2 := NewAppWithStateDir(dir)
	app2.serverURL = srv.URL
	app2.mu.Lock()
	app2.articleID = "art-resend"
	app2.name = "甲"
	app2.restoreSessionLocked("art-resend")
	app2.mu.Unlock()
	if err := app2.connect(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		app2.mu.Lock()
		app2.articleID = ""
		app2.joined = false
		c := app2.conn
		app2.mu.Unlock()
		if c != nil {
			_ = c.Close()
		}
	})
	select {
	case batch := <-got:
		if len(batch.Ops) != 1 {
			t.Fatalf("应重发 1 步: %+v", batch)
		}
		if batch.Ops[0].ID != wantID {
			t.Fatalf("Op.ID: got %q want %q", batch.Ops[0].ID, wantID)
		}
		if batch.Ops[0].Submit == nil || len(batch.Ops[0].Submit.LineIDs) != len(wantLines) {
			t.Fatalf("LineIDs: %+v want %+v", batch.Ops[0].Submit, wantLines)
		}
		for i := range wantLines {
			if batch.Ops[0].Submit.LineIDs[i] != wantLines[i] {
				t.Fatalf("LineIDs[%d] 变了", i)
			}
		}
	case <-time.After(2 * time.Second):
		t.Fatal("重启后未重发")
	}
}

func TestOfflineJoinReconnectsAndFlushesQueue(t *testing.T) {
	dir := t.TempDir()
	var n atomic.Int32
	got := make(chan protocol.Batch, 1)
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
		lineID := model.NewID()
		snap := protocol.Snapshot{
			Type: protocol.TypeSnapshot,
			View: document.View{
				Article: model.Article{ID: model.NewID(), Title: "t"},
				Lines:   []model.Line{{ID: lineID}},
			},
		}
		_ = c.WriteJSON(snap)
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

	doc := document.New("t")
	v, _ := doc.View()
	app := NewAppWithStateDir(dir)
	app.serverURL = srv.URL
	app.reconnectWait = time.Millisecond
	app.personID = "pid-off"
	app.articleID = "art-off"
	app.name = "甲"
	app.doc = doc
	app.rememberAfterSeenLocked(v.Lines)
	app.rememberServerSnapLocked(protocol.Snapshot{
		Type: protocol.TypeSnapshot,
		View: document.View{Article: v.Article, Lines: v.Lines},
	})
	if err := app.SubmitEdit(v.Lines[0].ID.Hex(), "离线待发"); err != nil {
		t.Fatal(err)
	}
	app.mu.Lock()
	wantID := app.queue[0].ID
	_ = app.savePersistLocked()
	app.mu.Unlock()

	app2 := NewAppWithStateDir(dir)
	app2.serverURL = srv.URL
	app2.reconnectWait = time.Millisecond
	if err := app2.Join("art-off", "甲"); err != nil {
		t.Fatalf("有缓存离线 Join 应成功: %v", err)
	}
	app2.mu.Lock()
	joined := app2.joined
	aid := app2.articleID
	app2.mu.Unlock()
	if !joined || aid != "art-off" {
		t.Fatalf("应进入离线会话: joined=%v id=%q", joined, aid)
	}
	t.Cleanup(func() {
		app2.mu.Lock()
		app2.articleID = ""
		app2.joined = false
		c := app2.conn
		app2.mu.Unlock()
		if c != nil {
			_ = c.Close()
		}
	})
	select {
	case batch := <-got:
		if len(batch.Ops) != 1 || batch.Ops[0].ID != wantID {
			t.Fatalf("恢复后应自动发出原队列: %+v", batch)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("离线重连后未自动发出队列")
	}
	if n.Load() < 3 {
		t.Fatalf("应先失败再恢复, n=%d", n.Load())
	}
}

func TestSwitchArticleBlockedWhenPersistFails(t *testing.T) {
	dir := t.TempDir()
	doc := document.New("A")
	v, _ := doc.View()
	app := NewAppWithStateDir(dir)
	app.personID = "pid"
	app.articleID = "art-a"
	app.name = "甲"
	app.doc = doc
	app.rememberServerSnapLocked(protocol.Snapshot{
		Type: protocol.TypeSnapshot,
		View: document.View{Article: v.Article, Lines: v.Lines},
	})
	if err := app.SubmitEdit(v.Lines[0].ID.Hex(), "留在A"); err != nil {
		t.Fatal(err)
	}
	app.mu.Lock()
	wantID := app.queue[0].ID
	qLen := len(app.queue)
	app.stateDir = blockPersistDir(t)
	app.mu.Unlock()

	err := app.Join("art-b", "乙")
	if err == nil || err.Error() != "本地保存失败，请检查磁盘后重试" {
		t.Fatalf("切文写盘失败应阻止: %v", err)
	}
	app.mu.Lock()
	defer app.mu.Unlock()
	if app.articleID != "art-a" {
		t.Fatalf("应仍在原文章: %q", app.articleID)
	}
	if len(app.queue) != qLen || app.queue[0].ID != wantID {
		t.Fatalf("原队列应保留: %+v", app.queue)
	}
}

func TestOnAckPersistFailKeepsFailedOpText(t *testing.T) {
	dir := t.TempDir()
	gotBatch := make(chan protocol.Batch, 1)
	up := websocket.Upgrader{CheckOrigin: func(*http.Request) bool { return true }}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c, err := up.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		defer c.Close()
		_, _, _ = c.ReadMessage()
		lineID := model.NewID()
		_ = c.WriteJSON(protocol.Snapshot{
			Type: protocol.TypeSnapshot,
			View: document.View{
				Article: model.Article{ID: model.NewID(), Title: "t"},
				Lines:   []model.Line{{ID: lineID, Content: "base"}},
			},
		})
		_, data, err := c.ReadMessage()
		if err != nil {
			return
		}
		var batch protocol.Batch
		if err := json.Unmarshal(data, &batch); err != nil {
			return
		}
		gotBatch <- batch
		_ = c.WriteJSON(protocol.Ack{
			Type:    protocol.TypeAck,
			Seq:     batch.Seq,
			Applied: 0,
			Message: "拒绝",
		})
		for {
			if _, _, err := c.ReadMessage(); err != nil {
				return
			}
		}
	}))
	t.Cleanup(srv.Close)

	app := NewAppWithStateDir(dir)
	app.serverURL = srv.URL
	app.personID = "p1"
	app.articleID = "art-ack"
	app.name = "甲"
	app.doc = document.New("t")
	v, _ := app.doc.View()
	if err := app.SubmitEdit(v.Lines[0].ID.Hex(), "落盘失败仍在"); err != nil {
		t.Fatal(err)
	}
	app.mu.Lock()
	wantID := app.queue[0].ID
	app.stateDir = blockPersistDir(t)
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
	case <-gotBatch:
	case <-time.After(2 * time.Second):
		t.Fatal("未收到 batch")
	}
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		app.mu.Lock()
		q := len(app.queue)
		r := len(app.rejected)
		sc := app.sentCount
		app.mu.Unlock()
		if q == 1 && r == 0 && sc == 0 {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	app.mu.Lock()
	defer app.mu.Unlock()
	if len(app.rejected) != 0 {
		t.Fatalf("落盘失败不应写入 rejected: %+v", app.rejected)
	}
	if len(app.queue) != 1 || app.queue[0].ID != wantID {
		t.Fatalf("原文应仍在队列: %+v", app.queue)
	}
	if app.queue[0].Submit == nil || app.queue[0].Submit.Content[0] != "落盘失败仍在" {
		t.Fatalf("Content 丢失: %+v", app.queue[0].Submit)
	}
}


func TestReportErrorPersistsByID(t *testing.T) {
	dir := t.TempDir()
	app := NewAppWithStateDir(dir)
	id := "E20260324-143052-a3f"
	app.ReportError(id+"\nforged-id", "secret boom\nforged line")
	data, err := os.ReadFile(filepath.Join(dir, clientLogName))
	if err != nil {
		t.Fatal(err)
	}
	text := string(data)
	if !strings.Contains(text, "["+id+"forged-id]") {
		t.Fatalf("id 内 CR/LF 应剥掉后仍可查: %q", text)
	}
	if strings.Contains(text, "]\nforged-id") || strings.Contains(text, "\nforged") {
		t.Fatalf("不得用换行伪造多行: %q", text)
	}
	if !strings.Contains(text, "secret boom") || !strings.Contains(text, "forged line") {
		t.Fatalf("应保留原文: %q", text)
	}
	if strings.Count(text, "\n") != 1 {
		t.Fatalf("整条须单行记录: %q", text)
	}
}

func TestNetworkFailShowsChinese(t *testing.T) {
	app := NewAppWithStateDir(t.TempDir())
	app.serverURL = "http://127.0.0.1:1"
	_, err := app.ListArticles()
	if err == nil || err.Error() != "无法连接服务器" {
		t.Fatalf("HTTP 失败应自然中文，得到 %v", err)
	}
	if err := app.Join("abc", "甲"); err == nil || err.Error() != "无法连接服务器" {
		t.Fatalf("初次 WS 失败应自然中文，得到 %v", err)
	}
	app.mu.Lock()
	defer app.mu.Unlock()
	if app.joined || app.articleID != "" {
		t.Fatalf("初次失败不该留下会话: joined=%v id=%q", app.joined, app.articleID)
	}
}
