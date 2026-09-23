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

func TestPersistInsertBeforeSurviveRestart(t *testing.T) {
	dir := t.TempDir()
	doc := document.New("t")
	if err := doc.EnsureLines(2); err != nil {
		t.Fatal(err)
	}
	v, _ := doc.View()
	l1, l2 := v.Lines[0].ID, v.Lines[1].ID

	app1 := NewAppWithStateDir(dir)
	app1.personID = "pid-before"
	app1.articleID = "art-before"
	app1.name = "乙"
	app1.doc = doc
	app1.rememberAfterSeenLocked(v.Lines)
	app1.rememberServerSnapLocked(protocol.Snapshot{
		Type: protocol.TypeSnapshot,
		View: document.View{Article: v.Article, Lines: v.Lines, Disputes: v.Disputes},
	})
	if err := app1.SubmitInsertBefore(l2.Hex(), []string{"前插", "两行"}); err != nil {
		t.Fatal(err)
	}
	app1.mu.Lock()
	if len(app1.queue) != 1 || app1.queue[0].Submit == nil {
		app1.mu.Unlock()
		t.Fatal("应入队")
	}
	opID := app1.queue[0].ID
	before := ""
	if app1.queue[0].Submit.BeforeSeen != nil {
		before = *app1.queue[0].Submit.BeforeSeen
	}
	lineIDs := append([]string(nil), app1.queue[0].Submit.LineIDs...)
	action := app1.queue[0].Submit.Action
	_ = app1.savePersistLocked()
	app1.mu.Unlock()

	if before != l1.Hex() {
		t.Fatalf("BeforeSeen 落盘前: %q want %q", before, l1.Hex())
	}
	if len(lineIDs) != 2 {
		t.Fatalf("LineIDs 应等长 Content: %+v", lineIDs)
	}

	app2 := NewAppWithStateDir(dir)
	app2.mu.Lock()
	app2.articleID = "art-before"
	app2.restoreSessionLocked("art-before")
	if len(app2.queue) != 1 {
		app2.mu.Unlock()
		t.Fatalf("队列应恢复: %d", len(app2.queue))
	}
	op := app2.queue[0]
	app2.mu.Unlock()
	if op.ID != opID {
		t.Fatalf("Op.ID 不变: got %q want %q", op.ID, opID)
	}
	s := op.Submit
	if s == nil || s.Action != action || s.Action != model.ActionInsertBefore {
		t.Fatalf("Action 应保留: %+v", s)
	}
	if s.BeforeSeen == nil || *s.BeforeSeen != l1.Hex() {
		t.Fatalf("BeforeSeen 重启不变: %+v", s.BeforeSeen)
	}
	if len(s.LineIDs) != 2 || s.LineIDs[0] != lineIDs[0] || s.LineIDs[1] != lineIDs[1] {
		t.Fatalf("LineIDs 不变: %+v vs %+v", s.LineIDs, lineIDs)
	}
	if len(s.Content) != 2 || s.Content[0] != "前插" || s.Content[1] != "两行" {
		t.Fatalf("Content: %+v", s.Content)
	}
	if text := opPlainText(op); text != "前插\n两行" {
		t.Fatalf("未同步原文: %q", text)
	}
}

func TestPersistRejectedKeepsText(t *testing.T) {
	dir := t.TempDir()
	gotBatch := make(chan protocol.Batch, 1)
	var rejArt model.Article
	var rejLines []model.Line
	up := websocket.Upgrader{CheckOrigin: func(*http.Request) bool { return true }}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c, err := up.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		defer c.Close()
		_, _, _ = c.ReadMessage()
		snap := protocol.Snapshot{
			Type: protocol.TypeSnapshot,
			View: document.View{Article: rejArt, Lines: rejLines},
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
	rejArt = v.Article
	rejLines = append([]model.Line(nil), v.Lines...)
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
	doc := document.New("t")
	v0, _ := doc.View()
	resendArt := v0.Article
	resendLines := append([]model.Line(nil), v0.Lines...)
	up := websocket.Upgrader{CheckOrigin: func(*http.Request) bool { return true }}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c, err := up.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		defer c.Close()
		_, _, _ = c.ReadMessage()
		snap := protocol.Snapshot{
			Type: protocol.TypeSnapshot,
			View: document.View{
				Article: resendArt,
				Lines:   resendLines,
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
	doc := document.New("t")
	v0, _ := doc.View()
	offArt := v0.Article
	offLines := append([]model.Line(nil), v0.Lines...)
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
		snap := protocol.Snapshot{
			Type: protocol.TypeSnapshot,
			View: document.View{
				Article: offArt,
				Lines:   offLines,
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
	doc := document.New("t")
	v0, _ := doc.View()
	ackArt := v0.Article
	ackLines := append([]model.Line(nil), v0.Lines...)
	up := websocket.Upgrader{CheckOrigin: func(*http.Request) bool { return true }}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c, err := up.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		defer c.Close()
		_, _, _ = c.ReadMessage()
		_ = c.WriteJSON(protocol.Snapshot{
			Type: protocol.TypeSnapshot,
			View: document.View{
				Article: ackArt,
				Lines:   ackLines,
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
	app.doc = doc
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

func TestPersistFailKeepsDocQueueAndRetries(t *testing.T) {
	dir := t.TempDir()
	doc := document.New("t")
	v, _ := doc.View()
	line := v.Lines[0].ID.Hex()
	app := NewAppWithStateDir(dir)
	app.personID = "pid"
	app.articleID = "art-pf"
	app.name = "甲"
	app.doc = doc
	app.rememberAfterSeenLocked(v.Lines)
	app.rememberServerSnapLocked(protocol.Snapshot{
		Type: protocol.TypeSnapshot,
		View: document.View{Article: v.Article, Lines: v.Lines},
	})
	app.stateDir = blockPersistDir(t)

	if err := app.SubmitEdit(line, "磁盘坏了也要留着"); err != nil {
		t.Fatalf("写盘失败不得向调用方报错诱发回滚: %v", err)
	}
	app.mu.Lock()
	if len(app.queue) != 1 || app.queue[0].Submit == nil || app.queue[0].Submit.Content[0] != "磁盘坏了也要留着" {
		app.mu.Unlock()
		t.Fatalf("队列应保留原文: %+v", app.queue)
	}
	view, err := app.doc.View()
	if err != nil {
		app.mu.Unlock()
		t.Fatal(err)
	}
	found := false
	for _, ln := range view.Lines {
		if ln.Content == "磁盘坏了也要留着" {
			found = true
			break
		}
	}
	warn := app.saveWarning
	app.mu.Unlock()
	if !found {
		t.Fatal("Doc 正文应保留用户输入")
	}
	if warn == "" || app.GetSaveWarning() == "" {
		t.Fatal("应有 saveWarning")
	}

	// 恢复可写目录，等 AfterFunc 重试落盘。
	app.mu.Lock()
	app.stateDir = dir
	app.mu.Unlock()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if app.GetSaveWarning() == "" {
			if _, err := os.Stat(filepath.Join(dir, persistFileName)); err == nil {
				return
			}
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("恢复磁盘后应清空 warning 并写成 state.json；warning=%q", app.GetSaveWarning())
}

func TestReplayApplyErrorKeepsQueueAndDraft(t *testing.T) {
	dir := t.TempDir()
	app := NewAppWithStateDir(dir)
	app.personID = "p1"
	app.articleID = "art-replay"
	app.name = "甲"
	doc := document.New("base")
	v, _ := doc.View()
	app.doc = doc
	app.rememberServerSnapLocked(protocol.Snapshot{
		Type: protocol.TypeSnapshot,
		View: document.View{Article: v.Article, Lines: v.Lines},
	})
	badID := model.NewID().Hex()
	app.queue = []protocol.Op{{
		ID:   "op-bad",
		Kind: protocol.TypeSubmit,
		Submit: &protocol.Submit{
			Type:     protocol.TypeSubmit,
			PersonID: "p1",
			LineID:   badID,
			Action:   model.ActionEdit,
			Content:  []string{"重放失败也要可复制"},
		},
	}}
	app.connGen = 7
	app.onSnapshot(protocol.Snapshot{
		Type: protocol.TypeSnapshot,
		View: document.View{Article: v.Article, Lines: v.Lines},
	}, 7)
	app.mu.Lock()
	defer app.mu.Unlock()
	if len(app.queue) != 1 || app.queue[0].ID != "op-bad" {
		t.Fatalf("本地重放失败仍须保留待送队列: %+v", app.queue)
	}
	if len(app.rejected) != 1 || app.rejected[0].Op.Submit == nil {
		t.Fatalf("应进未同步可复制: %+v", app.rejected)
	}
	if app.rejected[0].Op.Submit.Content[0] != "重放失败也要可复制" {
		t.Fatalf("原文丢失: %+v", app.rejected[0])
	}
}

func TestReplayInFlightAckDropsByOriginalIndex(t *testing.T) {
	app := NewAppWithStateDir(t.TempDir())
	app.personID = "p1"
	app.articleID = "art-inflight"
	app.name = "甲"
	doc := document.New("base")
	v, _ := doc.View()
	line := v.Lines[0].ID.Hex()
	app.doc = doc
	app.rememberServerSnapLocked(protocol.Snapshot{
		Type: protocol.TypeSnapshot,
		View: document.View{Article: v.Article, Lines: v.Lines},
	})
	badID := model.NewID().Hex()
	opBad := protocol.Op{
		ID:   "op-bad",
		Kind: protocol.TypeSubmit,
		Submit: &protocol.Submit{
			Type: protocol.TypeSubmit, PersonID: "p1", LineID: badID,
			Action: model.ActionEdit, Content: []string{"坏"},
		},
	}
	opGood := protocol.Op{
		ID:   "op-good",
		Kind: protocol.TypeSubmit,
		Submit: &protocol.Submit{
			Type: protocol.TypeSubmit, PersonID: "p1", LineID: line,
			Action: model.ActionEdit, Content: []string{"好"},
		},
	}
	app.queue = []protocol.Op{opBad, opGood}
	app.sentCount = 2
	app.sentSeq = 3
	app.connGen = 1
	app.replayQueueLocked()
	if len(app.queue) != 2 {
		t.Fatalf("不得剥 in-flight 前缀: %+v", app.queue)
	}
	if len(app.rejected) != 1 || app.rejected[0].Op.ID != "op-bad" {
		t.Fatalf("坏 Op 应有可复制草稿: %+v", app.rejected)
	}
	// 服务端两笔都 Applied：应按原队列下标丢，并清误判草稿。
	app.onAck(protocol.Ack{Type: protocol.TypeAck, Seq: 3, Applied: 2}, 1)
	app.mu.Lock()
	defer app.mu.Unlock()
	if len(app.queue) != 0 {
		t.Fatalf("ack 后队列应空: %+v", app.queue)
	}
	if len(app.rejected) != 0 {
		t.Fatalf("成功 ack 应清本地重放误判草稿: %+v", app.rejected)
	}
}

func TestReplayServerRejectKeepsDraft(t *testing.T) {
	app := NewAppWithStateDir(t.TempDir())
	app.personID = "p1"
	app.articleID = "art-rej"
	app.doc = document.New("base")
	v, _ := app.doc.View()
	badID := model.NewID().Hex()
	app.queue = []protocol.Op{{
		ID: "op-bad",
		Kind: protocol.TypeSubmit,
		Submit: &protocol.Submit{
			Type: protocol.TypeSubmit, PersonID: "p1", LineID: badID,
			Action: model.ActionEdit, Content: []string{"服务端拒"},
		},
	}}
	app.sentCount = 1
	app.sentSeq = 9
	app.connGen = 1
	app.replayQueueLocked()
	app.onAck(protocol.Ack{Type: protocol.TypeAck, Seq: 9, Applied: 0, Message: "冲突"}, 1)
	app.mu.Lock()
	defer app.mu.Unlock()
	if len(app.queue) != 0 {
		t.Fatalf("拒绝后应出队: %+v", app.queue)
	}
	if len(app.rejected) != 1 || app.rejected[0].Message != "冲突" {
		t.Fatalf("真实服务器拒绝须保留: %+v", app.rejected)
	}
	_ = v
}

func TestEnqueueSubmitDomainRejectKeepsDraft(t *testing.T) {
	dir := t.TempDir()
	app := NewAppWithStateDir(dir)
	app.personID = "甲"
	app.articleID = "art-dom"
	app.name = "甲"
	app.doc = document.New("base")
	// 行已不在链上：领域拒绝，仍须保可复制草稿（同目标连打覆盖）。
	gone := model.NewID().Hex()
	err := app.SubmitEdit(gone, "甲想改")
	if err == nil || err.Error() != msgLocalSpanReject {
		t.Fatalf("应稳定中文: %v", err)
	}
	err = app.SubmitEdit(gone, "甲再改一次")
	if err == nil || err.Error() != msgLocalSpanReject {
		t.Fatalf("二次: %v", err)
	}
	app.mu.Lock()
	defer app.mu.Unlock()
	if len(app.queue) != 0 {
		t.Fatalf("领域拒绝不得入队: %+v", app.queue)
	}
	if len(app.rejected) != 1 {
		t.Fatalf("同目标应覆盖草稿: %+v", app.rejected)
	}
	if app.rejected[0].Op.Submit == nil || app.rejected[0].Op.Submit.Content[0] != "甲再改一次" {
		t.Fatalf("应保留最新原文: %+v", app.rejected[0])
	}
	if app.rejected[0].Op.Submit.LineID != gone {
		t.Fatalf("目标行: %+v", app.rejected[0].Op.Submit)
	}
}

func TestAckPersistFailRetryFlushesQueue(t *testing.T) {
	dir := t.TempDir()
	got := make(chan protocol.Batch, 2)
	doc := document.New("t")
	v0, _ := doc.View()
	art, lines := v0.Article, append([]model.Line(nil), v0.Lines...)
	up := websocket.Upgrader{CheckOrigin: func(*http.Request) bool { return true }}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c, err := up.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		defer c.Close()
		_, _, _ = c.ReadMessage()
		_ = c.WriteJSON(protocol.Snapshot{
			Type: protocol.TypeSnapshot,
			View: document.View{Article: art, Lines: lines},
		})
		for {
			_, data, err := c.ReadMessage()
			if err != nil {
				return
			}
			var head struct {
				Type string `json:"type"`
			}
			if json.Unmarshal(data, &head) != nil || head.Type != protocol.TypeBatch {
				continue
			}
			var batch protocol.Batch
			if json.Unmarshal(data, &batch) != nil {
				continue
			}
			got <- batch
			// 不自动 ack：由测试驱动 onAck，避免与读循环竞态。
		}
	}))
	t.Cleanup(srv.Close)

	app := NewAppWithStateDir(dir)
	app.serverURL = srv.URL
	app.personID = "p1"
	app.articleID = "art-ack-disk"
	app.name = "甲"
	app.doc = doc
	app.rememberAfterSeenLocked(lines)
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
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		app.mu.Lock()
		ok := app.serverSnap != nil
		app.mu.Unlock()
		if ok {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if err := app.SubmitEdit(lines[0].ID.Hex(), "待确认"); err != nil {
		t.Fatal(err)
	}
	select {
	case batch := <-got:
		app.mu.Lock()
		app.stateDir = blockPersistDir(t)
		gen := app.connGen
		app.mu.Unlock()
		app.onAck(protocol.Ack{Type: protocol.TypeAck, Seq: batch.Seq, Applied: len(batch.Ops)}, gen)
	case <-time.After(2 * time.Second):
		t.Fatal("未收到首批")
	}
	app.mu.Lock()
	if len(app.queue) != 1 || app.sentCount != 0 {
		app.mu.Unlock()
		t.Fatalf("ack 写盘失败应回退队列并清 sent: q=%d sent=%d", len(app.queue), app.sentCount)
	}
	if app.saveWarning == "" {
		app.mu.Unlock()
		t.Fatal("应有 saveWarning")
	}
	app.stateDir = dir
	app.mu.Unlock()
	select {
	case batch := <-got:
		if len(batch.Ops) != 1 || batch.Ops[0].Submit == nil || batch.Ops[0].Submit.Content[0] != "待确认" {
			t.Fatalf("恢复后应自动重发: %+v", batch.Ops)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("写盘恢复后应自动继续发送，无需新输入")
	}
}

func TestSetServerRejectsPendingAndValidates(t *testing.T) {
	dir := t.TempDir()
	app := NewAppWithStateDir(dir)
	if err := app.SetServer("ftp://x"); err == nil {
		t.Fatal("非 http(s) 应拒绝")
	}
	for _, bad := range []string{
		"http://user:pass@127.0.0.1:9001",
		"http://127.0.0.1:9001/api",
		"http://127.0.0.1:9001?x=1",
		"http://127.0.0.1:9001#frag",
		"http://127.0.0.1:99999",
		"http://",
	} {
		if err := app.SetServer(bad); err == nil {
			t.Fatalf("应拒绝 %q", bad)
		}
	}
	if err := app.SetServer("http://127.0.0.1:9001"); err != nil {
		t.Fatal(err)
	}
	if app.GetServer() != "http://127.0.0.1:9001" {
		t.Fatalf("GetServer=%q", app.GetServer())
	}
	app.queue = []protocol.Op{{ID: "pending", Kind: protocol.TypeSubmit}}
	old := app.GetServer()
	if err := app.SetServer("http://127.0.0.1:9002"); err == nil {
		t.Fatal("有待送应拒绝")
	}
	if app.GetServer() != old {
		t.Fatalf("拒绝时不得改 URL: %q", app.GetServer())
	}
	app.queue = nil
	app.rejected = []rejectedOp{{ID: "u1", Op: protocol.Op{ID: "u1"}}}
	if err := app.SetServer("http://127.0.0.1:9002"); err == nil {
		t.Fatal("有未同步应拒绝")
	}
	app.rejected = nil
	// 干净缓存仍属旧服务：切换成功必须清掉。
	app.articleID = "art-old"
	app.lastArt = "art-old"
	app.doc = document.New("旧文")
	app.serverSnap = &protocol.Snapshot{Type: protocol.TypeSnapshot}
	app.sessions = map[string]*persistSession{
		"art-old": {Name: "甲", Snapshot: app.serverSnap},
	}
	pid := app.personID
	if err := app.SetServer("https://example.com/"); err != nil {
		t.Fatal(err)
	}
	if app.GetServer() != "https://example.com" {
		t.Fatalf("应规范化: %q", app.GetServer())
	}
	if app.GetCachedArticle() != nil {
		t.Fatal("切换成功不得带回旧服务 cachedArticle")
	}
	app.mu.Lock()
	defer app.mu.Unlock()
	if app.articleID != "" || app.lastArt != "" || app.doc != nil || app.serverSnap != nil || len(app.sessions) != 0 {
		t.Fatalf("旧服务状态未清: art=%q last=%q doc=%v snap=%v sess=%d",
			app.articleID, app.lastArt, app.doc != nil, app.serverSnap != nil, len(app.sessions))
	}
	if app.personID != pid {
		t.Fatalf("个人身份应保留: %q", app.personID)
	}
}

func TestStaleConnSnapshotIgnored(t *testing.T) {
	app := NewAppWithStateDir(t.TempDir())
	app.personID = "p1"
	app.articleID = "art-a"
	app.doc = document.New("old")
	app.connGen = 1
	lineID := model.NewID()
	app.onSnapshot(protocol.Snapshot{
		Type: protocol.TypeSnapshot,
		View: document.View{
			Article: model.Article{ID: model.NewID(), Title: "new"},
			Lines:   []model.Line{{ID: lineID, Content: "来自旧连接"}},
		},
	}, 0) // 旧 gen
	app.mu.Lock()
	defer app.mu.Unlock()
	v, _ := app.doc.View()
	if len(v.Lines) == 1 && v.Lines[0].Content == "来自旧连接" {
		t.Fatal("旧连接 snapshot 不得覆盖当前 Doc")
	}
}

func TestMoveCaretRangeSendsExtendedCursor(t *testing.T) {
	got := make(chan protocol.CursorMsg, 1)
	doc := document.New("t")
	v0, _ := doc.View()
	curArt := v0.Article
	curLines := append([]model.Line(nil), v0.Lines...)
	up := websocket.Upgrader{CheckOrigin: func(*http.Request) bool { return true }}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c, err := up.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		defer c.Close()
		_, _, _ = c.ReadMessage() // join
		_ = c.WriteJSON(protocol.Snapshot{
			Type: protocol.TypeSnapshot,
			View: document.View{
				Article: curArt,
				Lines:   curLines,
			},
		})
		for {
			_, data, err := c.ReadMessage()
			if err != nil {
				return
			}
			var head struct {
				Type string `json:"type"`
			}
			if json.Unmarshal(data, &head) != nil {
				continue
			}
			if head.Type != protocol.TypeCursor {
				continue
			}
			var msg protocol.CursorMsg
			if json.Unmarshal(data, &msg) != nil {
				continue
			}
			got <- msg
		}
	}))
	t.Cleanup(srv.Close)

	app := NewAppWithStateDir(t.TempDir())
	app.serverURL = srv.URL
	app.personID = "p1"
	app.articleID = "art-cur"
	app.name = "甲"
	app.doc = doc
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
	// 等 snapshot 建立在线
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		app.mu.Lock()
		ok := app.serverSnap != nil
		app.mu.Unlock()
		if ok {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	endLine := model.NewID().Hex()
	if err := app.MoveCaretRange("L1", "D1", 2, 3, endLine, "D2", 1, 9); err != nil {
		t.Fatal(err)
	}
	select {
	case msg := <-got:
		c := msg.Cursor
		if c.LineID != "L1" || c.DisputeID != "D1" || c.PartIndex != 2 || c.Offset != 3 {
			t.Fatalf("起点字段: %+v", c)
		}
		if c.SelEndLineID != endLine || c.SelEndDisputeID != "D2" || c.SelEndPartIndex != 1 || c.SelEnd != 9 {
			t.Fatalf("终点字段: %+v", c)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("未收到 cursor")
	}
}

func TestOnAckInvalidAppliedPreservesQueue(t *testing.T) {
	app := NewAppWithStateDir(t.TempDir())
	app.personID = "p1"
	app.articleID = "art-inv-ack"
	app.doc = document.New("base")
	mk := func(id, line, text string) protocol.Op {
		return protocol.Op{
			ID:   id,
			Kind: protocol.TypeSubmit,
			Submit: &protocol.Submit{
				Type: protocol.TypeSubmit, PersonID: "p1", LineID: line,
				Action: model.ActionEdit, Content: []string{text},
			},
		}
	}
	// in-flight 前缀 A,B；随后新排队 C。
	app.queue = []protocol.Op{mk("a", "L1", "A"), mk("b", "L2", "B"), mk("c", "L3", "C")}
	app.sentCount = 2
	app.sentSeq = 7
	app.connGen = 1

	app.onAck(protocol.Ack{Type: protocol.TypeAck, Seq: 7, Applied: -1}, 1)
	app.mu.Lock()
	if len(app.queue) != 3 || app.sentCount != 2 || app.sentSeq != 7 {
		app.mu.Unlock()
		t.Fatalf("负数 Applied 应忽略: q=%d sent=%d seq=%d", len(app.queue), app.sentCount, app.sentSeq)
	}
	if app.queue[0].ID != "a" || app.queue[1].ID != "b" || app.queue[2].ID != "c" {
		app.mu.Unlock()
		t.Fatalf("队列应原样: %+v", app.queue)
	}
	app.mu.Unlock()

	// Applied > sentCount：旧逻辑按 len(queue) 裁剪会误丢 C。
	app.onAck(protocol.Ack{Type: protocol.TypeAck, Seq: 7, Applied: 3}, 1)
	app.mu.Lock()
	if len(app.queue) != 3 || app.sentCount != 2 || app.sentSeq != 7 || app.queue[2].ID != "c" {
		app.mu.Unlock()
		t.Fatalf(">sentCount 不得当成功/丢未发送: q=%+v sent=%d seq=%d", app.queue, app.sentCount, app.sentSeq)
	}
	app.mu.Unlock()
}

func TestOnAckFullAppliedOrphanMessageKeepsUnsent(t *testing.T) {
	app := NewAppWithStateDir(t.TempDir())
	app.personID = "p1"
	app.articleID = "art-orphan-msg"
	app.doc = document.New("base")
	mk := func(id, line, text string) protocol.Op {
		return protocol.Op{
			ID:   id,
			Kind: protocol.TypeSubmit,
			Submit: &protocol.Submit{
				Type: protocol.TypeSubmit, PersonID: "p1", LineID: line,
				Action: model.ActionEdit, Content: []string{text},
			},
		}
	}
	// in-flight A,B；未发送 C。Applied==sentCount 且带孤儿 Message 不得误拒 C。
	app.queue = []protocol.Op{mk("a", "L1", "A"), mk("b", "L2", "B"), mk("c", "L3", "C")}
	app.sentCount = 2
	app.sentSeq = 7
	app.connGen = 1

	app.onAck(protocol.Ack{Type: protocol.TypeAck, Seq: 7, Applied: 2, Message: "bad"}, 1)
	app.mu.Lock()
	defer app.mu.Unlock()
	if len(app.queue) != 1 || app.queue[0].ID != "c" {
		t.Fatalf("应只消费已确认前缀，C 仍待送: q=%+v", app.queue)
	}
	if app.sentCount != 0 || app.sentSeq != 0 {
		t.Fatalf("应清 sent: sent=%d seq=%d", app.sentCount, app.sentSeq)
	}
	if len(app.rejected) != 0 {
		t.Fatalf("孤儿 Message 不得进 rejected: %+v", app.rejected)
	}
}
