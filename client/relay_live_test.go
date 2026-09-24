package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/KDreamelf/collaborative-editor/internal/document"
	"github.com/KDreamelf/collaborative-editor/internal/model"
	"github.com/KDreamelf/collaborative-editor/internal/protocol"
	"github.com/KDreamelf/collaborative-editor/internal/server"
	"github.com/gorilla/websocket"
)

func createRelayArticle(t *testing.T) (*httptest.Server, string) {
	t.Helper()
	srv := httptest.NewServer(server.NewHub(nil).Handler())
	t.Cleanup(srv.Close)
	resp, err := http.Post(srv.URL+"/api/articles", "application/json",
		strings.NewReader(`{"title":"在线Edit"}`))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("create status=%d", resp.StatusCode)
	}
	var meta struct {
		ID string `json:"id"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&meta); err != nil || meta.ID == "" {
		t.Fatalf("meta=%+v err=%v", meta, err)
	}
	return srv, meta.ID
}

func waitRelayReady(t *testing.T, app *App, d time.Duration) {
	t.Helper()
	deadline := time.Now().Add(d)
	for time.Now().Before(deadline) {
		app.mu.Lock()
		ready := app.relayReady && app.doc != nil
		app.mu.Unlock()
		if ready {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("等待 Bootstrap/relayReady 超时")
}

func flushApp(app *App) {
	app.mu.Lock()
	defer app.mu.Unlock()
	app.forceFlushLocked()
}

func appView(t *testing.T, app *App) document.View {
	t.Helper()
	app.mu.Lock()
	defer app.mu.Unlock()
	if app.doc == nil {
		t.Fatal("doc nil")
	}
	v, err := app.doc.View()
	if err != nil {
		t.Fatal(err)
	}
	return v
}

func countEditDisputes(v document.View, line model.ID) int {
	n := 0
	for _, d := range v.Disputes {
		if d.RealLine == line && d.Action == model.ActionEdit {
			n++
		}
	}
	return n
}

func waitEditDisputes(t *testing.T, app *App, line model.ID, want int, d time.Duration) document.View {
	t.Helper()
	deadline := time.Now().Add(d)
	var v document.View
	for time.Now().Before(deadline) {
		v = appView(t, app)
		if countEditDisputes(v, line) == want {
			return v
		}
		flushApp(app)
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("争议数=%d want=%d view=%+v", countEditDisputes(v, line), want, v.Disputes)
	return v
}

func TestRelayEditDisputeCCStepwise(t *testing.T) {
	line := model.NewID()
	base := document.View{
		Article: model.Article{ID: model.NewID(), Title: "t"},
		Lines:   []model.Line{{ID: line, Content: "底稿"}},
	}
	docA, err := document.Load(base.Article, base.Lines, nil)
	if err != nil {
		t.Fatal(err)
	}
	docB, err := document.Load(base.Article, base.Lines, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := docA.SubmitWith("甲", line, model.ActionEdit, []string{"甲文"}, document.SubmitOpts{}); err != nil {
		t.Fatal(err)
	}
	if err := docB.SubmitWith("乙", line, model.ActionEdit, []string{"乙文"}, document.SubmitOpts{}); err != nil {
		t.Fatal(err)
	}

	appA := NewAppWithStateDir(t.TempDir())
	appA.personID = "甲"
	appA.name = "甲"
	appA.doc = docA
	appA.attentionLine = line.Hex()
	appA.attentionActive = true
	appA.relayReady = true
	appA.serverSnap = &protocol.Snapshot{Type: protocol.TypeSnapshot, View: base}

	appB := NewAppWithStateDir(t.TempDir())
	appB.personID = "乙"
	appB.name = "乙"
	appB.doc = docB
	appB.attentionLine = line.Hex()
	appB.attentionActive = true
	appB.relayReady = true
	appB.serverSnap = &protocol.Snapshot{Type: protocol.TypeSnapshot, View: base}

	editB := protocol.Op{
		ID:   model.NewID().Hex(),
		Kind: protocol.TypeSubmit,
		Submit: &protocol.Submit{
			Type:     protocol.TypeSubmit,
			PersonID: "乙",
			LineID:   line.Hex(),
			Action:   model.ActionEdit,
			Content:  []string{"乙文"},
		},
	}
	appA.onRelay(protocol.RelayEvent{Type: protocol.TypeRelay, Op: editB}, appA.connGen)

	vA := appView(t, appA)
	if countEditDisputes(vA, line) != 0 {
		t.Fatalf("收到普通 Edit 后、foreign CC 前应零争议: %+v", vA.Disputes)
	}
	appA.mu.Lock()
	var ccOp *protocol.Op
	for i := range appA.queue {
		if appA.queue[i].Kind == protocol.TypeDisputeCC {
			op := appA.queue[i]
			ccOp = &op
			break
		}
	}
	appA.mu.Unlock()
	if ccOp == nil || ccOp.DisputeCC == nil {
		t.Fatal("应入队本人 DisputeCC")
	}
	if ccOp.DisputeCC.Claim.Person != "甲" || ccOp.DisputeCC.Claim.Content[0] != "甲文" {
		t.Fatalf("cc=%+v", ccOp.DisputeCC)
	}

	appA.onRelay(protocol.RelayEvent{Type: protocol.TypeRelay, Op: editB}, appA.connGen)
	appA.mu.Lock()
	ccCount := 0
	for _, op := range appA.queue {
		if op.Kind == protocol.TypeDisputeCC {
			ccCount++
		}
	}
	appA.mu.Unlock()
	if ccCount != 1 {
		t.Fatalf("重复 Relay CC 数=%d", ccCount)
	}

	foreignCC := protocol.Op{
		ID:   model.NewID().Hex(),
		Kind: protocol.TypeDisputeCC,
		DisputeCC: &protocol.DisputeCC{
			TargetPersonID: "甲",
			Claim: model.Dispute{
				ID:       model.NewID(),
				RealLine: line,
				Action:   model.ActionEdit,
				Person:   "乙",
				Content:  []string{"乙文"},
			},
		},
	}
	appA.onRelay(protocol.RelayEvent{Type: protocol.TypeRelay, Op: foreignCC}, appA.connGen)
	vA = appView(t, appA)
	if countEditDisputes(vA, line) != 2 {
		t.Fatalf("foreign CC 后应两候选: %+v", vA.Disputes)
	}
	appA.onRelay(protocol.RelayEvent{Type: protocol.TypeRelay, Op: foreignCC}, appA.connGen)
	vA = appView(t, appA)
	if countEditDisputes(vA, line) != 2 {
		t.Fatalf("重复 CC 仍两候选: %+v", vA.Disputes)
	}

	editA := protocol.Op{
		ID:   model.NewID().Hex(),
		Kind: protocol.TypeSubmit,
		Submit: &protocol.Submit{
			Type:     protocol.TypeSubmit,
			PersonID: "甲",
			LineID:   line.Hex(),
			Action:   model.ActionEdit,
			Content:  []string{"甲文"},
		},
	}
	appB.onRelay(protocol.RelayEvent{Type: protocol.TypeRelay, Op: editA}, appB.connGen)
	if countEditDisputes(appView(t, appB), line) != 0 {
		t.Fatal("B 收普通 Edit 后应零争议")
	}
	appB.onRelay(protocol.RelayEvent{Type: protocol.TypeRelay, Op: *ccOp}, appB.connGen)
	if countEditDisputes(appView(t, appB), line) != 2 {
		t.Fatalf("B 收 foreign CC 后应两候选: %+v", appView(t, appB).Disputes)
	}
}

func TestRelayBootstrapRebuildKeepsOwnBeliefAndForeignCC(t *testing.T) {
	line := model.NewID()
	art := model.Article{ID: model.NewID(), Title: "t"}
	baseLines := []model.Line{{ID: line, Content: "底稿"}}
	doc, err := document.Load(art, baseLines, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := doc.SubmitWith("甲", line, model.ActionEdit, []string{"本地已改"}, document.SubmitOpts{}); err != nil {
		t.Fatal(err)
	}

	app := NewAppWithStateDir(t.TempDir())
	app.personID = "甲"
	app.doc = doc
	app.attentionActive = true
	app.attentionLine = line.Hex()
	app.serverSnap = &protocol.Snapshot{
		Type: protocol.TypeSnapshot,
		View: document.View{Article: art, Lines: baseLines},
	}
	app.connGen = 1
	ownID := model.NewID()
	app.claimIDs = map[string]model.ID{line.Hex() + "\x00" + model.ActionEdit: ownID}

	foreignID := model.NewID()
	boot := protocol.Bootstrap{
		Type: protocol.TypeBootstrap,
		Base: document.View{Article: art, Lines: []model.Line{{ID: line, Content: "服务器链"}}},
		Disputes: []model.Dispute{{
			ID:       foreignID,
			RealLine: line,
			Action:   model.ActionEdit,
			Person:   "乙",
			Content:  []string{"乙主张"},
		}},
		YourLine: line.Hex(),
	}
	app.onBootstrap(boot, 1)
	v := appView(t, app)
	if countEditDisputes(v, line) != 2 {
		t.Fatalf("应入场 foreign + 回填本人: %+v", v.Disputes)
	}
	got := map[string]string{}
	gotID := map[string]model.ID{}
	for _, d := range v.Disputes {
		if d.Action == model.ActionEdit && d.RealLine == line {
			got[d.Person] = d.Content[0]
			gotID[d.Person] = d.ID
		}
	}
	if got["甲"] != "本地已改" || got["乙"] != "乙主张" {
		t.Fatalf("候选内容=%v", got)
	}
	if gotID["甲"] != ownID {
		t.Fatalf("本人 claimID 漂移: %v want %v", gotID["甲"], ownID)
	}
	app.mu.Lock()
	if app.attentionActive {
		t.Fatal("重连后 attentionActive 须失活")
	}
	if app.yourLine != line.Hex() || !app.relayReady {
		t.Fatalf("yourLine/ready: %q %v", app.yourLine, app.relayReady)
	}
	app.mu.Unlock()
}

func TestRelayBootstrapLoadBaseFirstJoin(t *testing.T) {
	line := model.NewID()
	art := model.Article{ID: model.NewID(), Title: "新文"}
	boot := protocol.Bootstrap{
		Type:     protocol.TypeBootstrap,
		Base:     document.View{Article: art, Lines: []model.Line{{ID: line, Content: "服底"}}},
		YourLine: line.Hex(),
		People:   []protocol.Person{{ID: "甲", Name: "甲"}},
	}
	app := NewAppWithStateDir(t.TempDir())
	app.personID = "甲"
	app.doc = document.New("")
	app.connGen = 3
	app.onBootstrap(boot, 3)
	v := appView(t, app)
	if len(v.Lines) != 1 || v.Lines[0].ID != line || v.Lines[0].Content != "服底" {
		t.Fatalf("首次应 Load Base: %+v", v.Lines)
	}
}

func TestRelaySoloEditNoSelfDispute(t *testing.T) {
	srv, articleID := createRelayArticle(t)

	app := NewAppWithStateDir(t.TempDir())
	app.serverURL = srv.URL
	if err := app.Join(articleID, "甲"); err != nil {
		t.Fatal(err)
	}
	waitRelayReady(t, app, 2*time.Second)

	app.mu.Lock()
	line := app.yourLine
	app.mu.Unlock()
	if line == "" {
		line = appView(t, app).Lines[0].ID.Hex()
	}
	if err := app.MoveCaret(line, "", 0, 0); err != nil {
		t.Fatal(err)
	}
	if err := app.SubmitEdit(line, "一"); err != nil {
		t.Fatal(err)
	}
	flushApp(app)
	if err := app.SubmitEdit(line, "二"); err != nil {
		t.Fatal(err)
	}
	flushApp(app)
	time.Sleep(200 * time.Millisecond)

	lid, _ := model.ParseID(line)
	v := appView(t, app)
	if countEditDisputes(v, lid) != 0 {
		t.Fatalf("单人连续 Edit 不应自争议: %+v", v.Disputes)
	}
	if len(v.Lines) != 1 || v.Lines[0].Content != "二" {
		t.Fatalf("正文=%+v", v.Lines)
	}
}

func waitQueueEmpty(t *testing.T, app *App, d time.Duration) {
	t.Helper()
	deadline := time.Now().Add(d)
	for time.Now().Before(deadline) {
		app.mu.Lock()
		n := len(app.queue)
		app.mu.Unlock()
		if n == 0 {
			return
		}
		flushApp(app)
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("等待 ACK 清空队列超时")
}

func stopAppSession(app *App) {
	app.mu.Lock()
	defer app.mu.Unlock()
	app.joined = false
	app.articleID = ""
	if app.conn != nil {
		_ = app.conn.Close()
		app.conn = nil
	}
}

func TestRelayPersistSoloEditSurvivesRestart(t *testing.T) {
	srv, articleID := createRelayArticle(t)
	dir := t.TempDir()

	app := NewAppWithStateDir(dir)
	app.serverURL = srv.URL
	if err := app.Join(articleID, "甲"); err != nil {
		t.Fatal(err)
	}
	waitRelayReady(t, app, 2*time.Second)

	app.mu.Lock()
	line := app.yourLine
	app.mu.Unlock()
	if line == "" {
		line = appView(t, app).Lines[0].ID.Hex()
	}
	if err := app.MoveCaret(line, "", 0, 0); err != nil {
		t.Fatal(err)
	}
	if err := app.SubmitEdit(line, "本端正文"); err != nil {
		t.Fatal(err)
	}
	flushApp(app)
	waitQueueEmpty(t, app, 3*time.Second)

	v1 := appView(t, app)
	if len(v1.Lines) != 1 || v1.Lines[0].Content != "本端正文" {
		t.Fatalf("写盘前正文=%+v", v1.Lines)
	}
	stopAppSession(app)

	app2 := NewAppWithStateDir(dir)
	app2.serverURL = srv.URL
	if err := app2.Join(articleID, "甲"); err != nil {
		t.Fatal(err)
	}
	waitRelayReady(t, app2, 2*time.Second)
	v2 := appView(t, app2)
	lid, _ := model.ParseID(line)
	if len(v2.Lines) != 1 || v2.Lines[0].Content != "本端正文" {
		t.Fatalf("恢复后正文回滚: %+v", v2.Lines)
	}
	if countEditDisputes(v2, lid) != 0 {
		t.Fatalf("恢复后自争议: %+v", v2.Disputes)
	}
	app2.mu.Lock()
	booted := app2.relayBooted
	app2.mu.Unlock()
	if !booted {
		t.Fatal("恢复后应 relayBooted")
	}
	stopAppSession(app2)
}

func TestRelayPersistEditDisputeSurvivesRestart(t *testing.T) {
	srv, articleID := createRelayArticle(t)
	dirA := t.TempDir()
	dirB := t.TempDir()

	appA := NewAppWithStateDir(dirA)
	appA.serverURL = srv.URL
	appB := NewAppWithStateDir(dirB)
	appB.serverURL = srv.URL
	if err := appA.Join(articleID, "甲"); err != nil {
		t.Fatal(err)
	}
	if err := appB.Join(articleID, "乙"); err != nil {
		t.Fatal(err)
	}
	waitRelayReady(t, appA, 2*time.Second)
	waitRelayReady(t, appB, 2*time.Second)

	appA.mu.Lock()
	line := appA.yourLine
	appA.mu.Unlock()
	if line == "" {
		line = appView(t, appA).Lines[0].ID.Hex()
	}
	lid, err := model.ParseID(line)
	if err != nil {
		t.Fatal(err)
	}
	if err := appA.MoveCaret(line, "", 0, 0); err != nil {
		t.Fatal(err)
	}
	if err := appB.MoveCaret(line, "", 0, 0); err != nil {
		t.Fatal(err)
	}
	if err := appA.SubmitEdit(line, "甲方正文"); err != nil {
		t.Fatal(err)
	}
	if err := appB.SubmitEdit(line, "乙方正文"); err != nil {
		t.Fatal(err)
	}
	flushApp(appA)
	flushApp(appB)

	vA := waitEditDisputes(t, appA, lid, 2, 3*time.Second)
	waitEditDisputes(t, appB, lid, 2, 3*time.Second)
	waitQueueEmpty(t, appA, 3*time.Second)

	before := map[string]string{}
	beforeIDs := map[string]model.ID{}
	for _, d := range vA.Disputes {
		if d.Action == model.ActionEdit && d.RealLine == lid {
			before[d.Person] = d.Content[0]
			beforeIDs[d.Person] = d.ID
		}
	}
	idA, idB := appA.PersonID(), appB.PersonID()
	stopAppSession(appA)

	appA2 := NewAppWithStateDir(dirA)
	appA2.serverURL = srv.URL
	if err := appA2.Join(articleID, "甲"); err != nil {
		t.Fatal(err)
	}
	waitRelayReady(t, appA2, 2*time.Second)
	v2 := waitEditDisputes(t, appA2, lid, 2, 3*time.Second)
	after := map[string]string{}
	afterIDs := map[string]model.ID{}
	for _, d := range v2.Disputes {
		if d.Action == model.ActionEdit && d.RealLine == lid {
			after[d.Person] = d.Content[0]
			afterIDs[d.Person] = d.ID
		}
	}
	if after[idA] != "甲方正文" || after[idB] != "乙方正文" {
		t.Fatalf("恢复候选内容=%v want 甲方/乙方", after)
	}
	if afterIDs[idA] != beforeIDs[idA] || afterIDs[idB] != beforeIDs[idB] {
		t.Fatalf("候选 ID 重复或漂移: before=%v after=%v", beforeIDs, afterIDs)
	}
	if countEditDisputes(v2, lid) != 2 {
		t.Fatalf("争议数=%d", countEditDisputes(v2, lid))
	}
	stopAppSession(appA2)
	stopAppSession(appB)
}

func TestRelayPersistForeignEditNoAttentionStepwise(t *testing.T) {
	// 断线重连时序下别人 Edit 的完整联调仍受 hub 重放窗口限制；此处用同包 handler 单步验证：
	// attentionActive=false 时整合远端普通 Edit、落盘，恢复后正文在、不发 CC。
	line := model.NewID()
	art := model.Article{ID: model.NewID(), Title: "t"}
	baseLines := []model.Line{{ID: line, Content: "底稿"}}
	doc, err := document.Load(art, baseLines, nil)
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	app := NewAppWithStateDir(dir)
	app.personID = "甲"
	app.name = "甲"
	app.articleID = "art-persist-attn"
	app.doc = doc
	app.attentionLine = line.Hex()
	app.attentionActive = false
	app.relayReady = true
	app.relayBooted = true
	app.serverSnap = &protocol.Snapshot{Type: protocol.TypeSnapshot, View: document.View{Article: art, Lines: baseLines}}
	app.sessions = map[string]*persistSession{}

	editB := protocol.Op{
		ID:   model.NewID().Hex(),
		Kind: protocol.TypeSubmit,
		Submit: &protocol.Submit{
			Type:     protocol.TypeSubmit,
			PersonID: "乙",
			LineID:   line.Hex(),
			Action:   model.ActionEdit,
			Content:  []string{"别人改"},
		},
	}
	app.onRelay(protocol.RelayEvent{Type: protocol.TypeRelay, Op: editB}, app.connGen)
	v := appView(t, app)
	if len(v.Lines) != 1 || v.Lines[0].Content != "别人改" {
		t.Fatalf("无注意力应直接整合: %+v", v.Lines)
	}
	app.mu.Lock()
	ccN := 0
	for _, op := range app.queue {
		if op.Kind == protocol.TypeDisputeCC {
			ccN++
		}
	}
	app.mu.Unlock()
	if ccN != 0 {
		t.Fatalf("无注意力不应发 CC: %d", ccN)
	}

	app.mu.Lock()
	if err := app.savePersistLocked(); err != nil {
		app.mu.Unlock()
		t.Fatal(err)
	}
	app.mu.Unlock()

	app2 := NewAppWithStateDir(dir)
	app2.mu.Lock()
	app2.restoreSessionLocked("art-persist-attn")
	app2.mu.Unlock()
	v2 := appView(t, app2)
	if len(v2.Lines) != 1 || v2.Lines[0].Content != "别人改" {
		t.Fatalf("恢复后正文=%+v", v2.Lines)
	}
	app2.mu.Lock()
	if !app2.relayBooted || !app2.relaySeenLocked(editB.ID) {
		app2.mu.Unlock()
		t.Fatalf("恢复后 booted/seen 丢失")
	}
	for _, op := range app2.queue {
		if op.Kind == protocol.TypeDisputeCC {
			app2.mu.Unlock()
			t.Fatal("恢复后不应有 CC 队列")
		}
	}
	app2.mu.Unlock()

	// 重放同一 Op：幂等，仍不发 CC。
	app2.relayReady = true
	app2.onRelay(protocol.RelayEvent{Type: protocol.TypeRelay, Op: editB}, app2.connGen)
	app2.mu.Lock()
	for _, op := range app2.queue {
		if op.Kind == protocol.TypeDisputeCC {
			app2.mu.Unlock()
			t.Fatal("seen 重放仍发 CC")
		}
	}
	app2.mu.Unlock()
}

func TestRelayLiveTwoAppsEditDisputeSmoke(t *testing.T) {
	srv, articleID := createRelayArticle(t)

	appA := NewAppWithStateDir(t.TempDir())
	appA.serverURL = srv.URL
	appB := NewAppWithStateDir(t.TempDir())
	appB.serverURL = srv.URL

	if err := appA.Join(articleID, "甲"); err != nil {
		t.Fatal(err)
	}
	if err := appB.Join(articleID, "乙"); err != nil {
		t.Fatal(err)
	}
	waitRelayReady(t, appA, 2*time.Second)
	waitRelayReady(t, appB, 2*time.Second)

	appA.mu.Lock()
	line := appA.yourLine
	appA.mu.Unlock()
	if line == "" {
		line = appView(t, appA).Lines[0].ID.Hex()
	}
	lid, err := model.ParseID(line)
	if err != nil {
		t.Fatal(err)
	}

	if err := appA.MoveCaret(line, "", 0, 0); err != nil {
		t.Fatal(err)
	}
	if err := appB.MoveCaret(line, "", 0, 0); err != nil {
		t.Fatal(err)
	}
	if err := appA.SubmitEdit(line, "甲方正文"); err != nil {
		t.Fatal(err)
	}
	if err := appB.SubmitEdit(line, "乙方正文"); err != nil {
		t.Fatal(err)
	}
	flushApp(appA)
	flushApp(appB)

	vA := waitEditDisputes(t, appA, lid, 2, 3*time.Second)
	vB := waitEditDisputes(t, appB, lid, 2, 3*time.Second)

	idA, idB := appA.PersonID(), appB.PersonID()
	personsA := map[string]string{}
	for _, d := range vA.Disputes {
		if d.Action == model.ActionEdit && d.RealLine == lid {
			personsA[d.Person] = d.Content[0]
		}
	}
	if personsA[idA] != "甲方正文" || personsA[idB] != "乙方正文" {
		t.Fatalf("A 候选=%v want %s/%s", personsA, idA, idB)
	}
	personsB := map[string]string{}
	for _, d := range vB.Disputes {
		if d.Action == model.ActionEdit && d.RealLine == lid {
			personsB[d.Person] = d.Content[0]
		}
	}
	if personsB[idA] != "甲方正文" || personsB[idB] != "乙方正文" {
		t.Fatalf("B 候选=%v want %s/%s", personsB, idA, idB)
	}
}

func findDisputeByPerson(v document.View, line model.ID, person string) (model.Dispute, bool) {
	for _, d := range v.Disputes {
		if d.RealLine == line && d.Action == model.ActionEdit && d.Person == person {
			return d, true
		}
	}
	return model.Dispute{}, false
}

func queueFollowAnswers(app *App) []protocol.Op {
	app.mu.Lock()
	defer app.mu.Unlock()
	var out []protocol.Op
	for _, op := range app.queue {
		if op.Kind == protocol.TypeFollowAnswer && op.FollowAnswer != nil {
			out = append(out, op)
		}
	}
	return out
}

func queueKindCount(app *App, kind string) int {
	app.mu.Lock()
	defer app.mu.Unlock()
	n := 0
	for _, op := range app.queue {
		if op.Kind == kind {
			n++
		}
	}
	return n
}

func waitFollowers(t *testing.T, app *App, claimID model.ID, person string, d time.Duration) document.View {
	t.Helper()
	deadline := time.Now().Add(d)
	var v document.View
	for time.Now().Before(deadline) {
		v = appView(t, app)
		for _, dsp := range v.Disputes {
			if dsp.ID == claimID {
				for _, f := range dsp.Followers {
					if f == person {
						return v
					}
				}
			}
		}
		// 争议已收口：正文已是目标
		if len(v.Disputes) == 0 {
			return v
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("等待追随收口超时 claim=%s person=%s disputes=%+v", claimID.Hex(), person, v.Disputes)
	return v
}

// 双边 CC：B 追随 A → A 立即 Accept → B 收口。
func TestRelayFollowAutoAcceptBilateral(t *testing.T) {
	line := model.NewID()
	base := document.View{
		Article: model.Article{ID: model.NewID(), Title: "t"},
		Lines:   []model.Line{{ID: line, Content: "底稿"}},
	}
	docA, err := document.Load(base.Article, base.Lines, nil)
	if err != nil {
		t.Fatal(err)
	}
	docB, err := document.Load(base.Article, base.Lines, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := docA.SubmitWith("甲", line, model.ActionEdit, []string{"甲文"}, document.SubmitOpts{}); err != nil {
		t.Fatal(err)
	}
	if err := docB.SubmitWith("乙", line, model.ActionEdit, []string{"乙文"}, document.SubmitOpts{}); err != nil {
		t.Fatal(err)
	}

	appA := NewAppWithStateDir(t.TempDir())
	appA.personID = "甲"
	appA.name = "甲"
	appA.doc = docA
	appA.attentionLine = line.Hex()
	appA.attentionActive = true
	appA.relayReady = true
	appA.relayBooted = true
	appA.serverSnap = &protocol.Snapshot{Type: protocol.TypeSnapshot, View: base}

	appB := NewAppWithStateDir(t.TempDir())
	appB.personID = "乙"
	appB.name = "乙"
	appB.doc = docB
	appB.attentionLine = line.Hex()
	appB.attentionActive = true
	appB.relayReady = true
	appB.relayBooted = true
	appB.serverSnap = &protocol.Snapshot{Type: protocol.TypeSnapshot, View: base}

	editB := protocol.Op{
		ID:   model.NewID().Hex(),
		Kind: protocol.TypeSubmit,
		Submit: &protocol.Submit{
			Type: protocol.TypeSubmit, PersonID: "乙", LineID: line.Hex(),
			Action: model.ActionEdit, Content: []string{"乙文"},
		},
	}
	editA := protocol.Op{
		ID:   model.NewID().Hex(),
		Kind: protocol.TypeSubmit,
		Submit: &protocol.Submit{
			Type: protocol.TypeSubmit, PersonID: "甲", LineID: line.Hex(),
			Action: model.ActionEdit, Content: []string{"甲文"},
		},
	}
	appA.onRelay(protocol.RelayEvent{Type: protocol.TypeRelay, Op: editB}, appA.connGen)
	appB.onRelay(protocol.RelayEvent{Type: protocol.TypeRelay, Op: editA}, appB.connGen)

	var ccA, ccB *protocol.Op
	appA.mu.Lock()
	for i := range appA.queue {
		if appA.queue[i].Kind == protocol.TypeDisputeCC {
			op := appA.queue[i]
			ccA = &op
			break
		}
	}
	appA.mu.Unlock()
	appB.mu.Lock()
	for i := range appB.queue {
		if appB.queue[i].Kind == protocol.TypeDisputeCC {
			op := appB.queue[i]
			ccB = &op
			break
		}
	}
	appB.mu.Unlock()
	if ccA == nil || ccB == nil {
		t.Fatal("双方应入队本人 CC")
	}
	appA.onRelay(protocol.RelayEvent{Type: protocol.TypeRelay, Op: *ccB}, appA.connGen)
	appB.onRelay(protocol.RelayEvent{Type: protocol.TypeRelay, Op: *ccA}, appB.connGen)
	if countEditDisputes(appView(t, appA), line) != 2 || countEditDisputes(appView(t, appB), line) != 2 {
		t.Fatalf("双边 CC 后应两候选 A=%+v B=%+v", appView(t, appA).Disputes, appView(t, appB).Disputes)
	}

	claimA, ok := findDisputeByPerson(appView(t, appB), line, "甲")
	if !ok {
		t.Fatal("B 应有甲候选")
	}
	if err := appB.RequestFollow(claimA.ID.Hex()); err != nil {
		t.Fatal(err)
	}
	if queueKindCount(appB, protocol.TypeFollow) != 1 {
		t.Fatal("B 应入队 Follow")
	}
	var followOp protocol.Op
	appB.mu.Lock()
	for _, op := range appB.queue {
		if op.Kind == protocol.TypeFollow {
			followOp = op
			break
		}
	}
	appB.mu.Unlock()

	appA.onRelay(protocol.RelayEvent{Type: protocol.TypeRelay, Op: followOp}, appA.connGen)
	ansList := queueFollowAnswers(appA)
	if len(ansList) != 1 || !ansList[0].FollowAnswer.Accept {
		t.Fatalf("A 应立即 Accept: %+v", ansList)
	}
	if ansList[0].FollowAnswer.FromID != "乙" || ansList[0].FollowAnswer.PersonID != "甲" {
		t.Fatalf("Answer 身份错: %+v", ansList[0].FollowAnswer)
	}

	// 重复 Follow 不重复答复
	appA.onRelay(protocol.RelayEvent{Type: protocol.TypeRelay, Op: followOp}, appA.connGen)
	if len(queueFollowAnswers(appA)) != 1 {
		t.Fatalf("重复 Follow 又答复: %d", len(queueFollowAnswers(appA)))
	}

	appB.onRelay(protocol.RelayEvent{Type: protocol.TypeRelay, Op: ansList[0]}, appB.connGen)
	vB := waitFollowers(t, appB, claimA.ID, "乙", time.Second)
	// 收口后正文为甲文，或甲候选带乙追随
	if len(vB.Lines) != 1 || vB.Lines[0].Content != "甲文" {
		if d, ok := findDisputeByPerson(vB, line, "甲"); !ok || len(d.Followers) == 0 || d.Followers[0] != "乙" {
			t.Fatalf("B 未收口: lines=%+v disputes=%+v", vB.Lines, vB.Disputes)
		}
	}

	// 重复 Answer 不炸
	appB.onRelay(protocol.RelayEvent{Type: protocol.TypeRelay, Op: ansList[0]}, appB.connGen)
}

// 单向 CC：A 无争议，target=本人已发 claimID → 仍 Accept；B 收口。
func TestRelayFollowAutoAcceptUnidirectionalOwnClaim(t *testing.T) {
	line := model.NewID()
	base := document.View{
		Article: model.Article{ID: model.NewID(), Title: "t"},
		Lines:   []model.Line{{ID: line, Content: "底稿"}},
	}
	docA, err := document.Load(base.Article, base.Lines, nil)
	if err != nil {
		t.Fatal(err)
	}
	docB, err := document.Load(base.Article, base.Lines, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := docA.SubmitWith("甲", line, model.ActionEdit, []string{"甲文"}, document.SubmitOpts{}); err != nil {
		t.Fatal(err)
	}
	if err := docB.SubmitWith("乙", line, model.ActionEdit, []string{"乙文"}, document.SubmitOpts{}); err != nil {
		t.Fatal(err)
	}

	appA := NewAppWithStateDir(t.TempDir())
	appA.personID = "甲"
	appA.doc = docA
	appA.attentionLine = line.Hex()
	appA.attentionActive = true
	appA.relayReady = true
	appA.relayBooted = true
	appA.serverSnap = &protocol.Snapshot{Type: protocol.TypeSnapshot, View: base}

	appB := NewAppWithStateDir(t.TempDir())
	appB.personID = "乙"
	appB.doc = docB
	appB.attentionLine = line.Hex()
	appB.attentionActive = true
	appB.relayReady = true
	appB.relayBooted = true
	appB.serverSnap = &protocol.Snapshot{Type: protocol.TypeSnapshot, View: base}

	editB := protocol.Op{
		ID:   model.NewID().Hex(),
		Kind: protocol.TypeSubmit,
		Submit: &protocol.Submit{
			Type: protocol.TypeSubmit, PersonID: "乙", LineID: line.Hex(),
			Action: model.ActionEdit, Content: []string{"乙文"},
		},
	}
	appA.onRelay(protocol.RelayEvent{Type: protocol.TypeRelay, Op: editB}, appA.connGen)
	if countEditDisputes(appView(t, appA), line) != 0 {
		t.Fatalf("A 发 CC 后仍应零争议: %+v", appView(t, appA).Disputes)
	}
	var ccA *protocol.Op
	appA.mu.Lock()
	for i := range appA.queue {
		if appA.queue[i].Kind == protocol.TypeDisputeCC {
			op := appA.queue[i]
			ccA = &op
			break
		}
	}
	claimID := appA.stableClaimIDLocked(line.Hex(), model.ActionEdit)
	appA.mu.Unlock()
	if ccA == nil || ccA.DisputeCC.Claim.ID != claimID {
		t.Fatalf("A CC 主张 ID 不对: %+v want %s", ccA, claimID.Hex())
	}

	appB.onRelay(protocol.RelayEvent{Type: protocol.TypeRelay, Op: *ccA}, appB.connGen)
	if countEditDisputes(appView(t, appB), line) != 2 {
		t.Fatalf("B 收 A CC 后应两候选: %+v", appView(t, appB).Disputes)
	}
	if err := appB.RequestFollow(claimID.Hex()); err != nil {
		t.Fatal(err)
	}
	var followOp protocol.Op
	appB.mu.Lock()
	for _, op := range appB.queue {
		if op.Kind == protocol.TypeFollow {
			followOp = op
			break
		}
	}
	appB.mu.Unlock()
	if followOp.Follow == nil {
		t.Fatal("B 未入队 Follow")
	}

	appA.onRelay(protocol.RelayEvent{Type: protocol.TypeRelay, Op: followOp}, appA.connGen)
	if countEditDisputes(appView(t, appA), line) != 0 {
		t.Fatalf("A 收 Follow 不得自建争议: %+v", appView(t, appA).Disputes)
	}
	ansList := queueFollowAnswers(appA)
	if len(ansList) != 1 || !ansList[0].FollowAnswer.Accept {
		t.Fatalf("A 零争议仍应 Accept: %+v", ansList)
	}

	appA.onRelay(protocol.RelayEvent{Type: protocol.TypeRelay, Op: followOp}, appA.connGen)
	if len(queueFollowAnswers(appA)) != 1 {
		t.Fatal("重复 Follow 又答复")
	}

	appB.onRelay(protocol.RelayEvent{Type: protocol.TypeRelay, Op: ansList[0]}, appB.connGen)
	vB := waitFollowers(t, appB, claimID, "乙", time.Second)
	if len(vB.Lines) != 1 || vB.Lines[0].Content != "甲文" {
		if d, ok := findDisputeByPerson(vB, line, "甲"); !ok || len(d.Followers) == 0 || d.Followers[0] != "乙" {
			t.Fatalf("B 单向追随未收口: lines=%+v disputes=%+v", vB.Lines, vB.Disputes)
		}
	}

	// 未知目标拒绝
	unknown := protocol.Op{
		ID:   model.NewID().Hex(),
		Kind: protocol.TypeFollow,
		Follow: &protocol.Follow{
			Type: protocol.TypeFollow, PersonID: "丙",
			DisputeID: model.NewID().Hex(), ClientTs: 1,
		},
	}
	appA.onRelay(protocol.RelayEvent{Type: protocol.TypeRelay, Op: unknown}, appA.connGen)
	ansList = queueFollowAnswers(appA)
	foundReject := false
	for _, op := range ansList {
		if op.FollowAnswer.FromID == "丙" && !op.FollowAnswer.Accept {
			foundReject = true
		}
	}
	if !foundReject {
		t.Fatalf("未知目标应 Reject: %+v", ansList)
	}
}

// 双边争议：A 已应用 B 追随但 Answer 未达 B；B 换新 Op.ID 重发 Follow，A 再答 Accept，B 收口。
func TestRelayFollowResendAfterLostAnswer(t *testing.T) {
	line := model.NewID()
	base := document.View{
		Article: model.Article{ID: model.NewID(), Title: "t"},
		Lines:   []model.Line{{ID: line, Content: "底稿"}},
	}
	docA, err := document.Load(base.Article, base.Lines, nil)
	if err != nil {
		t.Fatal(err)
	}
	docB, err := document.Load(base.Article, base.Lines, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := docA.SubmitWith("甲", line, model.ActionEdit, []string{"甲文"}, document.SubmitOpts{}); err != nil {
		t.Fatal(err)
	}
	if err := docB.SubmitWith("乙", line, model.ActionEdit, []string{"乙文"}, document.SubmitOpts{}); err != nil {
		t.Fatal(err)
	}

	appA := NewAppWithStateDir(t.TempDir())
	appA.personID = "甲"
	appA.name = "甲"
	appA.doc = docA
	appA.attentionLine = line.Hex()
	appA.attentionActive = true
	appA.relayReady = true
	appA.relayBooted = true
	appA.serverSnap = &protocol.Snapshot{Type: protocol.TypeSnapshot, View: base}

	appB := NewAppWithStateDir(t.TempDir())
	appB.personID = "乙"
	appB.name = "乙"
	appB.doc = docB
	appB.attentionLine = line.Hex()
	appB.attentionActive = true
	appB.relayReady = true
	appB.relayBooted = true
	appB.serverSnap = &protocol.Snapshot{Type: protocol.TypeSnapshot, View: base}

	editB := protocol.Op{
		ID: model.NewID().Hex(), Kind: protocol.TypeSubmit,
		Submit: &protocol.Submit{
			Type: protocol.TypeSubmit, PersonID: "乙", LineID: line.Hex(),
			Action: model.ActionEdit, Content: []string{"乙文"},
		},
	}
	editA := protocol.Op{
		ID: model.NewID().Hex(), Kind: protocol.TypeSubmit,
		Submit: &protocol.Submit{
			Type: protocol.TypeSubmit, PersonID: "甲", LineID: line.Hex(),
			Action: model.ActionEdit, Content: []string{"甲文"},
		},
	}
	appA.onRelay(protocol.RelayEvent{Type: protocol.TypeRelay, Op: editB}, appA.connGen)
	appB.onRelay(protocol.RelayEvent{Type: protocol.TypeRelay, Op: editA}, appB.connGen)

	var ccA, ccB *protocol.Op
	appA.mu.Lock()
	for i := range appA.queue {
		if appA.queue[i].Kind == protocol.TypeDisputeCC {
			op := appA.queue[i]
			ccA = &op
			break
		}
	}
	appA.mu.Unlock()
	appB.mu.Lock()
	for i := range appB.queue {
		if appB.queue[i].Kind == protocol.TypeDisputeCC {
			op := appB.queue[i]
			ccB = &op
			break
		}
	}
	appB.mu.Unlock()
	if ccA == nil || ccB == nil {
		t.Fatal("双方应入队本人 CC")
	}
	appA.onRelay(protocol.RelayEvent{Type: protocol.TypeRelay, Op: *ccB}, appA.connGen)
	appB.onRelay(protocol.RelayEvent{Type: protocol.TypeRelay, Op: *ccA}, appB.connGen)

	claimA, ok := findDisputeByPerson(appView(t, appB), line, "甲")
	if !ok {
		t.Fatal("B 应有甲候选")
	}
	if err := appB.RequestFollow(claimA.ID.Hex()); err != nil {
		t.Fatal(err)
	}
	var follow1 protocol.Op
	appB.mu.Lock()
	for _, op := range appB.queue {
		if op.Kind == protocol.TypeFollow {
			follow1 = op
			break
		}
	}
	appB.mu.Unlock()
	if follow1.Follow == nil {
		t.Fatal("B 未入队 Follow")
	}

	appA.onRelay(protocol.RelayEvent{Type: protocol.TypeRelay, Op: follow1}, appA.connGen)
	ans1 := queueFollowAnswers(appA)
	if len(ans1) != 1 || !ans1[0].FollowAnswer.Accept {
		t.Fatalf("A 首次应 Accept: %+v", ans1)
	}
	// 故意不把 Answer 交给 B；同 Op 重放不重答
	appA.onRelay(protocol.RelayEvent{Type: protocol.TypeRelay, Op: follow1}, appA.connGen)
	if len(queueFollowAnswers(appA)) != 1 {
		t.Fatalf("同 Op 重放又答复: %d", len(queueFollowAnswers(appA)))
	}

	if err := appB.RequestFollow(claimA.ID.Hex()); err != nil {
		t.Fatal(err)
	}
	var follow2 protocol.Op
	appB.mu.Lock()
	for _, op := range appB.queue {
		if op.Kind == protocol.TypeFollow && op.ID != follow1.ID {
			follow2 = op
			break
		}
	}
	appB.mu.Unlock()
	if follow2.Follow == nil || follow2.ID == follow1.ID {
		t.Fatalf("B 应换新 Op.ID 重发: follow1=%s follow2=%+v", follow1.ID, follow2)
	}

	appA.onRelay(protocol.RelayEvent{Type: protocol.TypeRelay, Op: follow2}, appA.connGen)
	ans2 := queueFollowAnswers(appA)
	if len(ans2) != 2 {
		t.Fatalf("新 Op 应再答 Accept: %d %+v", len(ans2), ans2)
	}
	last := ans2[len(ans2)-1]
	if !last.FollowAnswer.Accept || last.FollowAnswer.FromID != "乙" {
		t.Fatalf("重答内容错: %+v", last.FollowAnswer)
	}

	appB.onRelay(protocol.RelayEvent{Type: protocol.TypeRelay, Op: last}, appB.connGen)
	vB := waitFollowers(t, appB, claimA.ID, "乙", time.Second)
	if len(vB.Lines) != 1 || vB.Lines[0].Content != "甲文" {
		if d, ok := findDisputeByPerson(vB, line, "甲"); !ok || len(d.Followers) == 0 || d.Followers[0] != "乙" {
			t.Fatalf("B 收重答后未收口: lines=%+v disputes=%+v", vB.Lines, vB.Disputes)
		}
	}
}

// 双方同时互追（ClientTs 不同）：较早者决胜，两边一致，不循环 Answer。
func TestRelayFollowMutualEarlierWinsNoAnswerLoop(t *testing.T) {
	line := model.NewID()
	base := document.View{
		Article: model.Article{ID: model.NewID(), Title: "t"},
		Lines:   []model.Line{{ID: line, Content: "底稿"}},
	}
	docA, err := document.Load(base.Article, base.Lines, nil)
	if err != nil {
		t.Fatal(err)
	}
	docB, err := document.Load(base.Article, base.Lines, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := docA.SubmitWith("甲", line, model.ActionEdit, []string{"甲文"}, document.SubmitOpts{}); err != nil {
		t.Fatal(err)
	}
	if err := docB.SubmitWith("乙", line, model.ActionEdit, []string{"乙文"}, document.SubmitOpts{}); err != nil {
		t.Fatal(err)
	}

	appA := NewAppWithStateDir(t.TempDir())
	appA.personID = "甲"
	appA.name = "甲"
	appA.doc = docA
	appA.attentionLine = line.Hex()
	appA.attentionActive = true
	appA.relayReady = true
	appA.relayBooted = true
	appA.serverSnap = &protocol.Snapshot{Type: protocol.TypeSnapshot, View: base}

	appB := NewAppWithStateDir(t.TempDir())
	appB.personID = "乙"
	appB.name = "乙"
	appB.doc = docB
	appB.attentionLine = line.Hex()
	appB.attentionActive = true
	appB.relayReady = true
	appB.relayBooted = true
	appB.serverSnap = &protocol.Snapshot{Type: protocol.TypeSnapshot, View: base}

	editB := protocol.Op{
		ID: model.NewID().Hex(), Kind: protocol.TypeSubmit,
		Submit: &protocol.Submit{
			Type: protocol.TypeSubmit, PersonID: "乙", LineID: line.Hex(),
			Action: model.ActionEdit, Content: []string{"乙文"},
		},
	}
	editA := protocol.Op{
		ID: model.NewID().Hex(), Kind: protocol.TypeSubmit,
		Submit: &protocol.Submit{
			Type: protocol.TypeSubmit, PersonID: "甲", LineID: line.Hex(),
			Action: model.ActionEdit, Content: []string{"甲文"},
		},
	}
	appA.onRelay(protocol.RelayEvent{Type: protocol.TypeRelay, Op: editB}, appA.connGen)
	appB.onRelay(protocol.RelayEvent{Type: protocol.TypeRelay, Op: editA}, appB.connGen)

	var ccA, ccB *protocol.Op
	appA.mu.Lock()
	for i := range appA.queue {
		if appA.queue[i].Kind == protocol.TypeDisputeCC {
			op := appA.queue[i]
			ccA = &op
			break
		}
	}
	appA.mu.Unlock()
	appB.mu.Lock()
	for i := range appB.queue {
		if appB.queue[i].Kind == protocol.TypeDisputeCC {
			op := appB.queue[i]
			ccB = &op
			break
		}
	}
	appB.mu.Unlock()
	if ccA == nil || ccB == nil {
		t.Fatal("双方应入队本人 CC")
	}
	appA.onRelay(protocol.RelayEvent{Type: protocol.TypeRelay, Op: *ccB}, appA.connGen)
	appB.onRelay(protocol.RelayEvent{Type: protocol.TypeRelay, Op: *ccA}, appB.connGen)

	claimA, okA := findDisputeByPerson(appView(t, appB), line, "甲")
	claimB, okB := findDisputeByPerson(appView(t, appA), line, "乙")
	if !okA || !okB {
		t.Fatalf("缺候选 A=%v B=%v", okA, okB)
	}

	// 甲追乙 ts=20；乙追甲 ts=10（较早）。本端先挂 pending，再互投 Follow。
	appA.mu.Lock()
	if _, err := appA.doc.RequestFollow("甲", claimB.ID, 20); err != nil {
		appA.mu.Unlock()
		t.Fatal(err)
	}
	followA := protocol.Op{
		ID: model.NewID().Hex(), Kind: protocol.TypeFollow,
		Follow: &protocol.Follow{
			Type: protocol.TypeFollow, PersonID: "甲",
			DisputeID: claimB.ID.Hex(), ClientTs: 20,
		},
	}
	if err := appA.pushOpLocked(followA); err != nil {
		appA.mu.Unlock()
		t.Fatal(err)
	}
	appA.mu.Unlock()

	appB.mu.Lock()
	if _, err := appB.doc.RequestFollow("乙", claimA.ID, 10); err != nil {
		appB.mu.Unlock()
		t.Fatal(err)
	}
	followB := protocol.Op{
		ID: model.NewID().Hex(), Kind: protocol.TypeFollow,
		Follow: &protocol.Follow{
			Type: protocol.TypeFollow, PersonID: "乙",
			DisputeID: claimA.ID.Hex(), ClientTs: 10,
		},
	}
	if err := appB.pushOpLocked(followB); err != nil {
		appB.mu.Unlock()
		t.Fatal(err)
	}
	appB.mu.Unlock()

	beforeA := len(queueFollowAnswers(appA))
	beforeB := len(queueFollowAnswers(appB))

	// 分步：先把较早的乙→甲 Follow 投给甲，再把甲→乙 Follow 投给乙
	appA.onRelay(protocol.RelayEvent{Type: protocol.TypeRelay, Op: followB}, appA.connGen)
	if n := len(queueFollowAnswers(appA)); n != beforeA {
		t.Fatalf("互追决胜后 A 不应 Answer: before=%d after=%d", beforeA, n)
	}
	appB.onRelay(protocol.RelayEvent{Type: protocol.TypeRelay, Op: followA}, appB.connGen)
	if n := len(queueFollowAnswers(appB)); n != beforeB {
		t.Fatalf("互追决胜后 B 不应 Answer: before=%d after=%d", beforeB, n)
	}

	vA := appView(t, appA)
	vB := appView(t, appB)
	if len(vA.Lines) != 1 || vA.Lines[0].Content != "甲文" {
		t.Fatalf("A 决胜正文应为甲文: %+v disputes=%+v", vA.Lines, vA.Disputes)
	}
	if len(vB.Lines) != 1 || vB.Lines[0].Content != "甲文" {
		t.Fatalf("B 决胜正文应为甲文: %+v disputes=%+v", vB.Lines, vB.Disputes)
	}
	if len(vA.Disputes) != 0 || len(vB.Disputes) != 0 {
		t.Fatalf("互追收口后应无争议 A=%+v B=%+v", vA.Disputes, vB.Disputes)
	}
	// 真正跟随方（乙，较早）失活注意力；胜出领袖甲可保持活跃。
	appB.mu.Lock()
	if appB.attentionActive || appB.attentionLine != line.Hex() {
		appB.mu.Unlock()
		t.Fatalf("互追跟随方应失活且保留行: active=%v line=%q", appB.attentionActive, appB.attentionLine)
	}
	appB.mu.Unlock()
	appA.mu.Lock()
	if !appA.attentionActive {
		appA.mu.Unlock()
		t.Fatal("互追领袖方不应因决胜失活注意力")
	}
	appA.mu.Unlock()
}

// 双边争议 → B 追随 A applied 后注意力失活：A 再普通 Edit，B 直接整合且不发 CC；
// B 主动 MoveCaret 恢复注意力后，再遇异文才 CC。
func TestRelayFollowAppliedDeactivatesAttentionNoReCC(t *testing.T) {
	line := model.NewID()
	base := document.View{
		Article: model.Article{ID: model.NewID(), Title: "t"},
		Lines:   []model.Line{{ID: line, Content: "底稿"}},
	}
	docA, err := document.Load(base.Article, base.Lines, nil)
	if err != nil {
		t.Fatal(err)
	}
	docB, err := document.Load(base.Article, base.Lines, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := docA.SubmitWith("甲", line, model.ActionEdit, []string{"甲文"}, document.SubmitOpts{}); err != nil {
		t.Fatal(err)
	}
	if err := docB.SubmitWith("乙", line, model.ActionEdit, []string{"乙文"}, document.SubmitOpts{}); err != nil {
		t.Fatal(err)
	}

	appA := NewAppWithStateDir(t.TempDir())
	appA.personID = "甲"
	appA.name = "甲"
	appA.doc = docA
	appA.attentionLine = line.Hex()
	appA.attentionActive = true
	appA.relayReady = true
	appA.relayBooted = true
	appA.serverSnap = &protocol.Snapshot{Type: protocol.TypeSnapshot, View: base}

	appB := NewAppWithStateDir(t.TempDir())
	appB.personID = "乙"
	appB.name = "乙"
	appB.doc = docB
	appB.attentionLine = line.Hex()
	appB.attentionActive = true
	appB.relayReady = true
	appB.relayBooted = true
	appB.serverSnap = &protocol.Snapshot{Type: protocol.TypeSnapshot, View: base}

	editB := protocol.Op{
		ID: model.NewID().Hex(), Kind: protocol.TypeSubmit,
		Submit: &protocol.Submit{
			Type: protocol.TypeSubmit, PersonID: "乙", LineID: line.Hex(),
			Action: model.ActionEdit, Content: []string{"乙文"},
		},
	}
	editA := protocol.Op{
		ID: model.NewID().Hex(), Kind: protocol.TypeSubmit,
		Submit: &protocol.Submit{
			Type: protocol.TypeSubmit, PersonID: "甲", LineID: line.Hex(),
			Action: model.ActionEdit, Content: []string{"甲文"},
		},
	}
	appA.onRelay(protocol.RelayEvent{Type: protocol.TypeRelay, Op: editB}, appA.connGen)
	appB.onRelay(protocol.RelayEvent{Type: protocol.TypeRelay, Op: editA}, appB.connGen)

	var ccA, ccB *protocol.Op
	appA.mu.Lock()
	for i := range appA.queue {
		if appA.queue[i].Kind == protocol.TypeDisputeCC {
			op := appA.queue[i]
			ccA = &op
			break
		}
	}
	appA.mu.Unlock()
	appB.mu.Lock()
	for i := range appB.queue {
		if appB.queue[i].Kind == protocol.TypeDisputeCC {
			op := appB.queue[i]
			ccB = &op
			break
		}
	}
	appB.mu.Unlock()
	if ccA == nil || ccB == nil {
		t.Fatal("双方应入队本人 CC")
	}
	appA.onRelay(protocol.RelayEvent{Type: protocol.TypeRelay, Op: *ccB}, appA.connGen)
	appB.onRelay(protocol.RelayEvent{Type: protocol.TypeRelay, Op: *ccA}, appB.connGen)

	claimA, ok := findDisputeByPerson(appView(t, appB), line, "甲")
	if !ok {
		t.Fatal("B 应有甲候选")
	}
	if err := appB.RequestFollow(claimA.ID.Hex()); err != nil {
		t.Fatal(err)
	}
	var followOp protocol.Op
	appB.mu.Lock()
	for _, op := range appB.queue {
		if op.Kind == protocol.TypeFollow {
			followOp = op
			break
		}
	}
	appB.mu.Unlock()
	appA.onRelay(protocol.RelayEvent{Type: protocol.TypeRelay, Op: followOp}, appA.connGen)
	ansList := queueFollowAnswers(appA)
	if len(ansList) != 1 || !ansList[0].FollowAnswer.Accept {
		t.Fatalf("A 应 Accept: %+v", ansList)
	}
	appB.onRelay(protocol.RelayEvent{Type: protocol.TypeRelay, Op: ansList[0]}, appB.connGen)
	_ = waitFollowers(t, appB, claimA.ID, "乙", time.Second)

	appB.mu.Lock()
	if appB.attentionActive {
		appB.mu.Unlock()
		t.Fatal("追随 applied 后 B.attentionActive 应为 false")
	}
	if appB.attentionLine != line.Hex() {
		appB.mu.Unlock()
		t.Fatalf("追随不得清空注意力行: %q", appB.attentionLine)
	}
	ccBefore := 0
	for _, op := range appB.queue {
		if op.Kind == protocol.TypeDisputeCC {
			ccBefore++
		}
	}
	appB.mu.Unlock()

	// A 再普通 Edit；B 失活注意力 → 直接整合，不发 CC、不开争议。
	editA2 := protocol.Op{
		ID: model.NewID().Hex(), Kind: protocol.TypeSubmit,
		Submit: &protocol.Submit{
			Type: protocol.TypeSubmit, PersonID: "甲", LineID: line.Hex(),
			Action: model.ActionEdit, Content: []string{"甲再改"},
		},
	}
	appB.onRelay(protocol.RelayEvent{Type: protocol.TypeRelay, Op: editA2}, appB.connGen)
	vB := appView(t, appB)
	if len(vB.Lines) != 1 || vB.Lines[0].Content != "甲再改" {
		t.Fatalf("B 应直接更新正文: %+v", vB.Lines)
	}
	if countEditDisputes(vB, line) != 0 {
		t.Fatalf("B View 不应新开争议: %+v", vB.Disputes)
	}
	if queueKindCount(appB, protocol.TypeDisputeCC) != ccBefore {
		t.Fatalf("失活后不得新发 DisputeCC: before=%d after=%d", ccBefore, queueKindCount(appB, protocol.TypeDisputeCC))
	}

	// B 主动 MoveCaret 恢复注意力，再遇异文才 CC。
	if err := appB.MoveCaretRange(line.Hex(), "", 0, 0, "", "", 0, 0); err != nil {
		t.Fatal(err)
	}
	appB.mu.Lock()
	if !appB.attentionActive {
		appB.mu.Unlock()
		t.Fatal("MoveCaret 后应恢复 attentionActive")
	}
	appB.mu.Unlock()

	editA3 := protocol.Op{
		ID: model.NewID().Hex(), Kind: protocol.TypeSubmit,
		Submit: &protocol.Submit{
			Type: protocol.TypeSubmit, PersonID: "甲", LineID: line.Hex(),
			Action: model.ActionEdit, Content: []string{"甲三改"},
		},
	}
	appB.onRelay(protocol.RelayEvent{Type: protocol.TypeRelay, Op: editA3}, appB.connGen)
	if queueKindCount(appB, protocol.TypeDisputeCC) != ccBefore+1 {
		t.Fatalf("恢复注意力后异文应发 CC: before=%d after=%d", ccBefore, queueKindCount(appB, protocol.TypeDisputeCC))
	}
	vB2 := appView(t, appB)
	if countEditDisputes(vB2, line) != 0 {
		t.Fatalf("本人 CC 不进本地争议: %+v", vB2.Disputes)
	}
	if len(vB2.Lines) != 1 || vB2.Lines[0].Content != "甲再改" {
		t.Fatalf("发 CC 时本地正文应保持本人主张: %+v", vB2.Lines)
	}
}

func countInsertDisputes(v document.View, line model.ID, action string) int {
	n := 0
	for _, d := range v.Disputes {
		if d.RealLine == line && d.Action == action {
			n++
		}
	}
	return n
}

func waitLineCount(t *testing.T, app *App, want int, d time.Duration) document.View {
	t.Helper()
	deadline := time.Now().Add(d)
	var v document.View
	for time.Now().Before(deadline) {
		v = appView(t, app)
		if len(v.Lines) == want {
			return v
		}
		flushApp(app)
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("行数=%d want=%d lines=%+v", len(v.Lines), want, v.Lines)
	return v
}

// 两端 Join：A 行尾 Enter，B 无注意力 → 同新行 ID、零争议。
func TestRelayLiveTwoAppsInsertAfterNoAttention(t *testing.T) {
	srv, articleID := createRelayArticle(t)

	appA := NewAppWithStateDir(t.TempDir())
	appA.serverURL = srv.URL
	appB := NewAppWithStateDir(t.TempDir())
	appB.serverURL = srv.URL
	if err := appA.Join(articleID, "甲"); err != nil {
		t.Fatal(err)
	}
	if err := appB.Join(articleID, "乙"); err != nil {
		t.Fatal(err)
	}
	waitRelayReady(t, appA, 2*time.Second)
	waitRelayReady(t, appB, 2*time.Second)

	appA.mu.Lock()
	anchorHex := appA.yourLine
	appA.mu.Unlock()
	if anchorHex == "" {
		anchorHex = appView(t, appA).Lines[0].ID.Hex()
	}
	anchor, err := model.ParseID(anchorHex)
	if err != nil {
		t.Fatal(err)
	}
	beforeN := len(appView(t, appA).Lines)
	if beforeN < 2 {
		t.Fatalf("双人 Join 应分到各自初始行: %d", beforeN)
	}
	if err := appA.SubmitInsert(anchor.Hex(), []string{"新行"}); err != nil {
		t.Fatal(err)
	}
	flushApp(appA)

	vA := waitLineCount(t, appA, beforeN+1, 3*time.Second)
	vB := waitLineCount(t, appB, beforeN+1, 3*time.Second)
	idx := -1
	for i, ln := range vA.Lines {
		if ln.ID == anchor {
			idx = i
			break
		}
	}
	if idx < 0 || idx+1 >= len(vA.Lines) || vA.Lines[idx+1].Content != "新行" {
		t.Fatalf("A 行尾插入: anchor=%s lines=%+v", anchor.Hex(), vA.Lines)
	}
	newID := vA.Lines[idx+1].ID
	if idx+1 >= len(vB.Lines) || vB.Lines[idx+1].ID != newID || vB.Lines[idx+1].Content != "新行" {
		t.Fatalf("B 应同新行 ID: A=%+v B=%+v", vA.Lines, vB.Lines)
	}
	if len(vA.Disputes) != 0 || len(vB.Disputes) != 0 {
		t.Fatalf("零争议 A=%+v B=%+v", vA.Disputes, vB.Disputes)
	}
	appB.mu.Lock()
	ccN := 0
	for _, op := range appB.queue {
		if op.Kind == protocol.TypeDisputeCC {
			ccN++
		}
	}
	appB.mu.Unlock()
	if ccN != 0 {
		t.Fatalf("无注意力不应发 CC: %d", ccN)
	}
	stopAppSession(appA)
	stopAppSession(appB)
}

// 行首 Enter：新行在原行上方，Submit.LineID / CC.RealLine 指下行。
func TestRelayLiveTwoAppsInsertBeforeNoAttention(t *testing.T) {
	srv, articleID := createRelayArticle(t)

	appA := NewAppWithStateDir(t.TempDir())
	appA.serverURL = srv.URL
	appB := NewAppWithStateDir(t.TempDir())
	appB.serverURL = srv.URL
	if err := appA.Join(articleID, "甲"); err != nil {
		t.Fatal(err)
	}
	if err := appB.Join(articleID, "乙"); err != nil {
		t.Fatal(err)
	}
	waitRelayReady(t, appA, 2*time.Second)
	waitRelayReady(t, appB, 2*time.Second)

	appA.mu.Lock()
	belowHex := appA.yourLine
	appA.mu.Unlock()
	if belowHex == "" {
		belowHex = appView(t, appA).Lines[0].ID.Hex()
	}
	below, err := model.ParseID(belowHex)
	if err != nil {
		t.Fatal(err)
	}
	beforeN := len(appView(t, appA).Lines)
	if err := appA.SubmitInsertBefore(below.Hex(), []string{"前插"}); err != nil {
		t.Fatal(err)
	}
	appA.mu.Lock()
	var subLine string
	var subAction string
	for _, op := range appA.queue {
		if op.Kind == protocol.TypeSubmit && op.Submit != nil && op.Submit.Action == model.ActionInsertBefore {
			subLine = op.Submit.LineID
			subAction = op.Submit.Action
			break
		}
	}
	appA.mu.Unlock()
	if subLine != below.Hex() || subAction != model.ActionInsertBefore {
		t.Fatalf("前插 RealLine 须指下行: line=%q action=%q", subLine, subAction)
	}
	flushApp(appA)

	vA := waitLineCount(t, appA, beforeN+1, 3*time.Second)
	vB := waitLineCount(t, appB, beforeN+1, 3*time.Second)
	idx := -1
	for i, ln := range vA.Lines {
		if ln.ID == below {
			idx = i
			break
		}
	}
	if idx <= 0 || vA.Lines[idx-1].Content != "前插" {
		t.Fatalf("A 前插应在下行上方: below=%s lines=%+v", below.Hex(), vA.Lines)
	}
	newID := vA.Lines[idx-1].ID
	if idx-1 >= len(vB.Lines) || vB.Lines[idx-1].ID != newID || vB.Lines[idx-1].Content != "前插" || vB.Lines[idx].ID != below {
		t.Fatalf("B 前插: A=%+v B=%+v", vA.Lines, vB.Lines)
	}
	if len(vA.Disputes) != 0 || len(vB.Disputes) != 0 {
		t.Fatalf("零争议 A=%+v B=%+v", vA.Disputes, vB.Disputes)
	}
	stopAppSession(appA)
	stopAppSession(appB)
}

// B 活跃锚点时 A Insert：B 只排本人 CC，正式链不变；foreign Insert CC 入场且幂等。
func TestRelayInsertActiveAttentionQueuesOwnCC(t *testing.T) {
	anchor := model.NewID()
	newID := model.NewID()
	base := document.View{
		Article: model.Article{ID: model.NewID(), Title: "t"},
		Lines:   []model.Line{{ID: anchor, Content: "锚"}},
	}
	docB, err := document.Load(base.Article, base.Lines, nil)
	if err != nil {
		t.Fatal(err)
	}

	appB := NewAppWithStateDir(t.TempDir())
	appB.personID = "乙"
	appB.name = "乙"
	appB.doc = docB
	appB.attentionLine = anchor.Hex()
	appB.attentionActive = true
	appB.relayReady = true
	appB.relayBooted = true
	appB.serverSnap = &protocol.Snapshot{Type: protocol.TypeSnapshot, View: base}

	before := appView(t, appB)
	insA := protocol.Op{
		ID:   model.NewID().Hex(),
		Kind: protocol.TypeSubmit,
		Submit: &protocol.Submit{
			Type:     protocol.TypeSubmit,
			PersonID: "甲",
			LineID:   anchor.Hex(),
			Action:   model.ActionInsert,
			Content:  []string{"对方插"},
			LineIDs:  []string{newID.Hex()},
		},
	}
	appB.onRelay(protocol.RelayEvent{Type: protocol.TypeRelay, Op: insA}, appB.connGen)

	vB := appView(t, appB)
	if len(vB.Lines) != 1 || vB.Lines[0].ID != anchor || vB.Lines[0].Content != "锚" {
		t.Fatalf("正式链应不变: %+v", vB.Lines)
	}
	if countInsertDisputes(vB, anchor, model.ActionInsert) != 0 {
		t.Fatalf("本地不得进插入争议: %+v", vB.Disputes)
	}
	appB.mu.Lock()
	var ccOp *protocol.Op
	for i := range appB.queue {
		if appB.queue[i].Kind == protocol.TypeDisputeCC {
			op := appB.queue[i]
			ccOp = &op
			break
		}
	}
	claimID := appB.stableClaimIDLocked(anchor.Hex(), model.ActionInsert)
	appB.mu.Unlock()
	if ccOp == nil || ccOp.DisputeCC == nil {
		t.Fatal("应入队本人 Insert DisputeCC")
	}
	if ccOp.DisputeCC.Claim.Person != "乙" || ccOp.DisputeCC.Claim.ID != claimID ||
		ccOp.DisputeCC.Claim.Action != model.ActionInsert || ccOp.DisputeCC.Claim.RealLine != anchor {
		t.Fatalf("cc=%+v want claimID=%s", ccOp.DisputeCC, claimID.Hex())
	}

	// 重复普通包幂等：仍只一条 CC。
	appB.onRelay(protocol.RelayEvent{Type: protocol.TypeRelay, Op: insA}, appB.connGen)
	if queueKindCount(appB, protocol.TypeDisputeCC) != 1 {
		t.Fatalf("重复 Insert CC 数=%d", queueKindCount(appB, protocol.TypeDisputeCC))
	}

	// foreign Insert CC 入场；同 ID 重收不增候选；正式链不变。
	foreignID := model.NewID()
	foreignCC := protocol.Op{
		ID:   model.NewID().Hex(),
		Kind: protocol.TypeDisputeCC,
		DisputeCC: &protocol.DisputeCC{
			TargetPersonID: "乙",
			Claim: model.Dispute{
				ID:       foreignID,
				RealLine: anchor,
				Action:   model.ActionInsert,
				Person:   "甲",
				Content:  []string{"对方插"},
			},
		},
	}
	appB.onRelay(protocol.RelayEvent{Type: protocol.TypeRelay, Op: foreignCC}, appB.connGen)
	vB2 := appView(t, appB)
	if len(vB2.Lines) != len(before.Lines) || vB2.Lines[0].ID != anchor {
		t.Fatalf("foreign Insert CC 不得改链: %+v", vB2.Lines)
	}
	if countInsertDisputes(vB2, anchor, model.ActionInsert) != 2 {
		t.Fatalf("外来插入 CC 应连同本人未插入的写法入场: %+v", vB2.Disputes)
	}
	appB.onRelay(protocol.RelayEvent{Type: protocol.TypeRelay, Op: foreignCC}, appB.connGen)
	if countInsertDisputes(appView(t, appB), anchor, model.ActionInsert) != 2 {
		t.Fatalf("同 ID 重收不得增候选: %+v", appView(t, appB).Disputes)
	}
}

// Bootstrap 当前链已含插入；Events 即使非空也不得当权威。无本地 View 直接取链。
func TestRelayBootstrapOwnInsertRestoresNoDispute(t *testing.T) {
	anchor := model.NewID()
	newID := model.NewID()
	art := model.Article{ID: model.NewID(), Title: "t"}

	app := NewAppWithStateDir(t.TempDir())
	app.personID = "甲"
	app.doc = document.New("")
	app.connGen = 1

	boot := protocol.Bootstrap{
		Type: protocol.TypeBootstrap,
		Base: document.View{Article: art, Lines: []model.Line{
			{ID: anchor, Content: "锚", Next: newID},
			{ID: newID, Content: "历史插", Prev: anchor},
		}},
		YourLine: anchor.Hex(),
	}
	app.onBootstrap(boot, 1)
	v := appView(t, app)
	if len(v.Lines) != 2 || v.Lines[1].ID != newID || v.Lines[1].Content != "历史插" {
		t.Fatalf("应以当前链为准: %+v", v.Lines)
	}
	if len(v.Disputes) != 0 {
		t.Fatalf("不得造争议: %+v", v.Disputes)
	}
}

func TestOnBootstrapUnackedEditKeepsCandidateAndQueue(t *testing.T) {
	line := model.NewID()
	art := model.Article{ID: model.NewID(), Title: "t"}
	doc, err := document.Load(art, []model.Line{{ID: line, Content: "底稿"}}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := doc.SubmitWith("甲", line, model.ActionEdit, []string{"我未ACK"}, document.SubmitOpts{}); err != nil {
		t.Fatal(err)
	}
	ownID := model.NewID()
	app := NewAppWithStateDir(t.TempDir())
	app.personID = "甲"
	app.doc = doc
	app.connGen = 2
	app.attentionActive = true
	app.claimIDs = map[string]model.ID{line.Hex() + "\x00" + model.ActionEdit: ownID}
	app.queue = []protocol.Op{{
		ID:   "op-unacked",
		Kind: protocol.TypeSubmit,
		Submit: &protocol.Submit{
			Type: protocol.TypeSubmit, PersonID: "甲", LineID: line.Hex(),
			Action: model.ActionEdit, Content: []string{"我未ACK"},
		},
	}}
	app.rejected = []rejectedOp{{ID: "rej1", Op: app.queue[0], Message: "旧拒"}}

	boot := protocol.Bootstrap{
		Type: protocol.TypeBootstrap,
		Base: document.View{Article: art, Lines: []model.Line{{ID: line, Content: "服务器异文"}}},
		Disputes: []model.Dispute{{
			ID: model.NewID(), RealLine: line, Action: model.ActionEdit,
			Person: "乙", Content: []string{"乙CC"},
		}},
		YourLine: line.Hex(),
	}
	app.onBootstrap(boot, 2)
	v := appView(t, app)
	if countEditDisputes(v, line) != 2 {
		t.Fatalf("应保留双边候选: %+v", v.Disputes)
	}
	got := map[string]string{}
	for _, d := range v.Disputes {
		if d.RealLine == line && d.Action == model.ActionEdit {
			got[d.Person] = d.Content[0]
		}
	}
	if got["甲"] != "我未ACK" || got["乙"] != "乙CC" {
		t.Fatalf("候选=%v", got)
	}
	app.mu.Lock()
	defer app.mu.Unlock()
	if len(app.queue) != 1 || app.queue[0].ID != "op-unacked" {
		t.Fatalf("须保留未 ACK queue: %+v", app.queue)
	}
	if len(app.rejected) != 1 || app.rejected[0].ID != "rej1" {
		t.Fatalf("须保留 rejected: %+v", app.rejected)
	}
	if app.attentionActive {
		t.Fatal("attentionActive 须失活")
	}
	if app.claimIDs[line.Hex()+"\x00"+model.ActionEdit] != ownID {
		t.Fatalf("claimID 漂移")
	}
}

func TestOnBootstrapOwnOnlyCCNoSelfDispute(t *testing.T) {
	line := model.NewID()
	art := model.Article{ID: model.NewID(), Title: "t"}
	app := NewAppWithStateDir(t.TempDir())
	app.personID = "甲"
	app.doc = document.New("")
	app.connGen = 1
	boot := protocol.Bootstrap{
		Type: protocol.TypeBootstrap,
		Base: document.View{Article: art, Lines: []model.Line{{ID: line, Content: "链"}}},
		Disputes: []model.Dispute{{
			ID: model.NewID(), RealLine: line, Action: model.ActionEdit,
			Person: "甲", Content: []string{"仅本人CC"},
		}},
		YourLine: line.Hex(),
	}
	app.onBootstrap(boot, 1)
	v := appView(t, app)
	if countEditDisputes(v, line) != 0 {
		t.Fatalf("仅本人 CC 不得自争议: %+v", v.Disputes)
	}
	if len(v.Lines) != 1 || v.Lines[0].Content != "链" {
		t.Fatalf("正文=%+v", v.Lines)
	}
}

func TestOnBootstrapRebuildFailKeepsLocal(t *testing.T) {
	line := model.NewID()
	art := model.Article{ID: model.NewID(), Title: "t"}
	doc, err := document.Load(art, []model.Line{{ID: line, Content: "本机原文"}}, nil)
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	app := NewAppWithStateDir(dir)
	app.personID = "甲"
	app.articleID = art.ID.Hex()
	app.doc = doc
	app.connGen = 1
	app.yourLine = "local-your"
	app.people = []protocol.Person{{ID: "甲", Name: "甲名"}}
	app.cursors = []protocol.Cursor{{PersonID: "甲", LineID: "local-your"}}
	app.attentionActive = true
	app.queue = []protocol.Op{{
		ID:   "op-keep",
		Kind: protocol.TypeSubmit,
		Submit: &protocol.Submit{
			Type: protocol.TypeSubmit, PersonID: "甲", LineID: line.Hex(),
			Action: model.ActionEdit, Content: []string{"未发编辑"},
		},
	}}
	app.preboot = []protocol.RelayEvent{{
		Type: protocol.TypeRelay,
		Op:   protocol.Op{ID: "pre-1", Kind: protocol.TypeSubmit},
	}}
	oldSnap := app.serverSnap

	up := websocket.Upgrader{CheckOrigin: func(*http.Request) bool { return true }}
	gotBatch := make(chan struct{}, 1)
	wsSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c, err := up.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		for {
			_, data, err := c.ReadMessage()
			if err != nil {
				return
			}
			var head struct {
				Type string `json:"type"`
			}
			if json.Unmarshal(data, &head) == nil && head.Type == protocol.TypeBatch {
				select {
				case gotBatch <- struct{}{}:
				default:
				}
			}
		}
	}))
	t.Cleanup(wsSrv.Close)
	dialWS := func() *websocket.Conn {
		t.Helper()
		c, _, err := websocket.DefaultDialer.Dial("ws"+strings.TrimPrefix(wsSrv.URL, "http"), nil)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = c.Close() })
		return c
	}
	app.conn = dialWS()

	bad := protocol.Bootstrap{
		Type: protocol.TypeBootstrap,
		Base: document.View{Article: art, Lines: []model.Line{{ID: line, Content: "服务器"}}},
		Disputes: []model.Dispute{{
			ID: model.NewID(), RealLine: line, Action: model.ActionDelete,
			Person: "乙", Content: []string{""},
		}},
		YourLine: "boot-your",
		People:   []protocol.Person{{ID: "乙", Name: "乙名"}},
		Cursors:  []protocol.Cursor{{PersonID: "乙", LineID: "boot-your"}},
	}
	app.onBootstrap(bad, 1)

	v := appView(t, app)
	if len(v.Lines) != 1 || v.Lines[0].Content != "本机原文" {
		t.Fatalf("失败不得覆盖本地: %+v", v.Lines)
	}
	app.mu.Lock()
	if app.relayReady || app.relayBooted {
		app.mu.Unlock()
		t.Fatalf("失败不得 ready/booted: ready=%v booted=%v", app.relayReady, app.relayBooted)
	}
	if len(app.queue) != 1 || app.queue[0].ID != "op-keep" {
		app.mu.Unlock()
		t.Fatalf("须保留 queue: %+v", app.queue)
	}
	if len(app.preboot) != 1 || app.preboot[0].Op.ID != "pre-1" {
		app.mu.Unlock()
		t.Fatalf("须保留 preboot: %+v", app.preboot)
	}
	if app.yourLine != "local-your" || len(app.people) != 1 || app.people[0].ID != "甲" {
		app.mu.Unlock()
		t.Fatalf("不得改可见身份态 your=%q people=%+v", app.yourLine, app.people)
	}
	if len(app.cursors) != 1 || app.cursors[0].PersonID != "甲" || !app.attentionActive {
		app.mu.Unlock()
		t.Fatalf("不得改 cursors/attention: %+v active=%v", app.cursors, app.attentionActive)
	}
	if app.serverSnap != oldSnap {
		app.mu.Unlock()
		t.Fatal("失败不得写 serverSnap")
	}
	if app.sentCount != 0 {
		app.mu.Unlock()
		t.Fatalf("失败不得 flush sentCount=%d", app.sentCount)
	}
	if sess := app.sessions[app.articleID]; sess != nil && sess.Snapshot != nil {
		app.mu.Unlock()
		t.Fatal("失败不得落盘 serverSnap")
	}
	app.mu.Unlock()

	select {
	case <-gotBatch:
		t.Fatal("失败不得向服务器发 Batch")
	case <-time.After(80 * time.Millisecond):
	}

	logPath := filepath.Join(dir, clientLogName)
	logData, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatalf("应写 client.log: %v", err)
	}
	logText := string(logData)
	if strings.Contains(logText, "bootstrap-rebuild") {
		t.Fatalf("日志 ID 不得固定 bootstrap-rebuild: %q", logText)
	}
	idRe := regexp.MustCompile(`\[(E\d{8}-\d{6}-[0-9a-z]{3})\]`)
	if idRe.FindStringSubmatch(logText) == nil {
		t.Fatalf("须有时间戳日志 ID: %q", logText)
	}

	// 失败分支异步关旧 conn；换新连接再验好 Bootstrap flush。
	fresh := dialWS()
	app.mu.Lock()
	app.conn = fresh
	app.sentCount = 0
	app.mu.Unlock()

	good := protocol.Bootstrap{
		Type:     protocol.TypeBootstrap,
		Base:     document.View{Article: art, Lines: []model.Line{{ID: line, Content: "服务器正文"}}},
		YourLine: line.Hex(),
	}
	app.onBootstrap(good, 1)
	v = appView(t, app)
	// 重建成功后 replay 未 ACK 普通 Edit，正文应回到本机队列内容。
	if len(v.Lines) != 1 || v.Lines[0].Content != "未发编辑" {
		t.Fatalf("好 Bootstrap 应重建并 replay: %+v", v.Lines)
	}
	app.mu.Lock()
	ready := app.relayReady && app.relayBooted
	sent := app.sentCount
	app.mu.Unlock()
	if !ready {
		t.Fatal("好 Bootstrap 应 ready/booted")
	}
	if sent == 0 {
		t.Fatal("好 Bootstrap 应 flush 队列")
	}
	select {
	case <-gotBatch:
	case <-time.After(2 * time.Second):
		t.Fatal("好 Bootstrap 应发出 Batch")
	}
}

func TestFormalWSThirdJoinerSeesDispute(t *testing.T) {
	srv, articleID := createRelayArticle(t)

	appA := NewAppWithStateDir(t.TempDir())
	appA.serverURL = srv.URL
	appB := NewAppWithStateDir(t.TempDir())
	appB.serverURL = srv.URL
	if err := appA.Join(articleID, "甲"); err != nil {
		t.Fatal(err)
	}
	if err := appB.Join(articleID, "乙"); err != nil {
		t.Fatal(err)
	}
	waitRelayReady(t, appA, 2*time.Second)
	waitRelayReady(t, appB, 2*time.Second)

	appA.mu.Lock()
	line := appA.yourLine
	appA.mu.Unlock()
	if line == "" {
		line = appView(t, appA).Lines[0].ID.Hex()
	}
	lid, err := model.ParseID(line)
	if err != nil {
		t.Fatal(err)
	}
	if err := appA.MoveCaret(line, "", 0, 0); err != nil {
		t.Fatal(err)
	}
	if err := appB.MoveCaret(line, "", 0, 0); err != nil {
		t.Fatal(err)
	}
	if err := appA.SubmitEdit(line, "甲方"); err != nil {
		t.Fatal(err)
	}
	if err := appB.SubmitEdit(line, "乙方"); err != nil {
		t.Fatal(err)
	}
	flushApp(appA)
	flushApp(appB)
	waitEditDisputes(t, appA, lid, 2, 3*time.Second)
	waitEditDisputes(t, appB, lid, 2, 3*time.Second)
	waitQueueEmpty(t, appA, 3*time.Second)
	waitQueueEmpty(t, appB, 3*time.Second)

	appC := NewAppWithStateDir(t.TempDir())
	appC.serverURL = srv.URL
	if err := appC.Join(articleID, "丙"); err != nil {
		t.Fatal(err)
	}
	waitRelayReady(t, appC, 2*time.Second)
	deadline := time.Now().Add(3 * time.Second)
	var vC document.View
	for time.Now().Before(deadline) {
		vC = appView(t, appC)
		if countEditDisputes(vC, lid) >= 2 {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	got := map[string]bool{}
	for _, d := range vC.Disputes {
		if d.RealLine == lid && d.Action == model.ActionEdit && len(d.Content) > 0 {
			got[d.Content[0]] = true
		}
	}
	if !got["甲方"] || !got["乙方"] {
		t.Fatalf("第三人应收到甲/乙争议: %+v", vC.Disputes)
	}
	stopAppSession(appA)
	stopAppSession(appB)
	stopAppSession(appC)
}

func TestFormalWSReconnectAdoptsChainNoNewCC(t *testing.T) {
	srv, articleID := createRelayArticle(t)
	dirA := t.TempDir()

	appA := NewAppWithStateDir(dirA)
	appA.serverURL = srv.URL
	appB := NewAppWithStateDir(t.TempDir())
	appB.serverURL = srv.URL
	if err := appA.Join(articleID, "甲"); err != nil {
		t.Fatal(err)
	}
	if err := appB.Join(articleID, "乙"); err != nil {
		t.Fatal(err)
	}
	waitRelayReady(t, appA, 2*time.Second)
	waitRelayReady(t, appB, 2*time.Second)

	appA.mu.Lock()
	line := appA.yourLine
	appA.mu.Unlock()
	if line == "" {
		line = appView(t, appA).Lines[0].ID.Hex()
	}
	if err := appA.MoveCaret(line, "", 0, 0); err != nil {
		t.Fatal(err)
	}
	if err := appA.SubmitEdit(line, "甲先写"); err != nil {
		t.Fatal(err)
	}
	flushApp(appA)
	waitQueueEmpty(t, appA, 3*time.Second)
	stopAppSession(appA)

	if err := appB.MoveCaret(line, "", 0, 0); err != nil {
		t.Fatal(err)
	}
	appB.mu.Lock()
	appB.attentionActive = false
	appB.mu.Unlock()
	if err := appB.SubmitEdit(line, "乙离线改"); err != nil {
		t.Fatal(err)
	}
	flushApp(appB)
	waitQueueEmpty(t, appB, 3*time.Second)

	appA2 := NewAppWithStateDir(dirA)
	appA2.serverURL = srv.URL
	if err := appA2.Join(articleID, "甲"); err != nil {
		t.Fatal(err)
	}
	waitRelayReady(t, appA2, 2*time.Second)
	lid, _ := model.ParseID(line)
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		v := appView(t, appA2)
		content := ""
		for _, ln := range v.Lines {
			if ln.ID == lid {
				content = ln.Content
				break
			}
		}
		if content == "乙离线改" && countEditDisputes(v, lid) == 0 {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	v := appView(t, appA2)
	got := ""
	for _, ln := range v.Lines {
		if ln.ID == lid {
			got = ln.Content
			break
		}
	}
	if got != "乙离线改" {
		t.Fatalf("应采纳当前链行 %s: got=%q lines=%+v", line, got, v.Lines)
	}
	if countEditDisputes(v, lid) != 0 {
		t.Fatalf("无注意力重连不得新争议: %+v", v.Disputes)
	}
	appA2.mu.Lock()
	active := appA2.attentionActive
	ccN := 0
	for _, op := range appA2.queue {
		if op.Kind == protocol.TypeDisputeCC {
			ccN++
		}
	}
	appA2.mu.Unlock()
	if active {
		t.Fatal("重连 attentionActive 须 false")
	}
	if ccN != 0 {
		t.Fatalf("零新 CC，got %d", ccN)
	}
	stopAppSession(appA2)
	stopAppSession(appB)
}

func TestFormalWSReconnectKeepsUnackedWithForeign(t *testing.T) {
	srv, articleID := createRelayArticle(t)
	dirA := t.TempDir()

	appA := NewAppWithStateDir(dirA)
	appA.serverURL = srv.URL
	appB := NewAppWithStateDir(t.TempDir())
	appB.serverURL = srv.URL
	if err := appA.Join(articleID, "甲"); err != nil {
		t.Fatal(err)
	}
	if err := appB.Join(articleID, "乙"); err != nil {
		t.Fatal(err)
	}
	waitRelayReady(t, appA, 2*time.Second)
	waitRelayReady(t, appB, 2*time.Second)

	appA.mu.Lock()
	line := appA.yourLine
	appA.mu.Unlock()
	if line == "" {
		line = appView(t, appA).Lines[0].ID.Hex()
	}
	lid, _ := model.ParseID(line)
	if err := appA.MoveCaret(line, "", 0, 0); err != nil {
		t.Fatal(err)
	}
	if err := appB.MoveCaret(line, "", 0, 0); err != nil {
		t.Fatal(err)
	}
	if err := appA.SubmitEdit(line, "甲方正文"); err != nil {
		t.Fatal(err)
	}
	if err := appB.SubmitEdit(line, "乙方正文"); err != nil {
		t.Fatal(err)
	}
	flushApp(appA)
	flushApp(appB)
	waitEditDisputes(t, appA, lid, 2, 3*time.Second)
	waitEditDisputes(t, appB, lid, 2, 3*time.Second)
	waitQueueEmpty(t, appA, 3*time.Second)

	// A 本地再改一笔但不发（模拟未 ACK）：直接塞 queue + 乐观改 doc。
	appA.mu.Lock()
	if err := appA.doc.SubmitWith(appA.personID, lid, model.ActionEdit, []string{"甲未ACK"}, document.SubmitOpts{WholeClaim: true}); err != nil {
		appA.mu.Unlock()
		t.Fatal(err)
	}
	unacked := protocol.Op{
		ID:   model.NewID().Hex(),
		Kind: protocol.TypeSubmit,
		Submit: &protocol.Submit{
			Type: protocol.TypeSubmit, PersonID: appA.personID, LineID: line,
			Action: model.ActionEdit, Content: []string{"甲未ACK"}, WholeClaim: true,
		},
	}
	appA.queue = append(appA.queue, unacked)
	ownBefore := appA.claimIDs[line+"\x00"+model.ActionEdit]
	if err := appA.savePersistLocked(); err != nil {
		appA.mu.Unlock()
		t.Fatal(err)
	}
	appA.mu.Unlock()
	stopAppSession(appA)

	appA2 := NewAppWithStateDir(dirA)
	appA2.serverURL = srv.URL
	if err := appA2.Join(articleID, "甲"); err != nil {
		t.Fatal(err)
	}
	waitRelayReady(t, appA2, 2*time.Second)
	v := waitEditDisputes(t, appA2, lid, 2, 3*time.Second)
	got := map[string]string{}
	gotID := map[string]model.ID{}
	for _, d := range v.Disputes {
		if d.RealLine == lid && d.Action == model.ActionEdit {
			got[d.Person] = d.Content[0]
			gotID[d.Person] = d.ID
		}
	}
	idA := appA2.PersonID()
	if got[idA] != "甲未ACK" {
		t.Fatalf("应保住未 ACK 本人候选: %v", got)
	}
	if gotID[idA] != ownBefore && !ownBefore.IsZero() {
		t.Fatalf("claimID 漂移 before=%v after=%v", ownBefore, gotID[idA])
	}
	// 重连瞬间 queue 仍在（单测已验）；真 WS 可能随后 flush+ACK 清空，属送达成功。
	appA2.mu.Lock()
	ownAfter := appA2.claimIDs[line+"\x00"+model.ActionEdit]
	appA2.mu.Unlock()
	if !ownBefore.IsZero() && ownAfter != ownBefore {
		t.Fatalf("claimIDs 漂移 before=%v after=%v", ownBefore, ownAfter)
	}
	stopAppSession(appA2)
	stopAppSession(appB)
}

func countBodyDisputes(v document.View, line model.ID) int {
	n := 0
	for _, d := range v.Disputes {
		if d.RealLine == line && (d.Action == model.ActionEdit || d.Action == model.ActionDelete) {
			n++
		}
	}
	return n
}

// 外来 Delete CC：本端保留行（建本人 Edit）；本人 Delete CC 只记稳定 ID、不进争议；Edit/Delete 同槽 ID。
func TestRelayDisputeCCDeleteAndBodySlotID(t *testing.T) {
	line := model.NewID()
	base := document.View{
		Article: model.Article{ID: model.NewID(), Title: "t"},
		Lines:   []model.Line{{ID: line, Content: "正文"}},
	}
	doc, err := document.Load(base.Article, base.Lines, nil)
	if err != nil {
		t.Fatal(err)
	}
	app := NewAppWithStateDir(t.TempDir())
	app.personID = "甲"
	app.doc = doc
	app.relayReady = true
	app.relayBooted = true
	app.serverSnap = &protocol.Snapshot{Type: protocol.TypeSnapshot, View: base}

	app.mu.Lock()
	oldDelID := model.NewID()
	app.claimIDs = map[string]model.ID{line.Hex() + "\x00" + model.ActionDelete: oldDelID}
	editID := app.stableClaimIDLocked(line.Hex(), model.ActionEdit)
	delID := app.stableClaimIDLocked(line.Hex(), model.ActionDelete)
	app.mu.Unlock()
	if editID != oldDelID || delID != oldDelID {
		t.Fatalf("Edit/Delete 须复用旧 Delete 键 ID: edit=%v del=%v old=%v", editID, delID, oldDelID)
	}
	app.mu.Lock()
	if _, ok := app.claimIDs[line.Hex()+"\x00"+model.ActionDelete]; ok {
		t.Fatal("旧 Delete 键应迁到 ActionEdit")
	}
	if app.claimIDs[line.Hex()+"\x00"+model.ActionEdit] != oldDelID {
		t.Fatal("规范键须为 ActionEdit")
	}
	app.mu.Unlock()

	foreignDel := protocol.Op{
		ID:   model.NewID().Hex(),
		Kind: protocol.TypeDisputeCC,
		DisputeCC: &protocol.DisputeCC{
			TargetPersonID: "甲",
			Claim: model.Dispute{
				ID:       model.NewID(),
				RealLine: line,
				Action:   model.ActionDelete,
				Person:   "乙",
				Content:  nil,
			},
		},
	}
	app.onRelay(protocol.RelayEvent{Type: protocol.TypeRelay, Op: foreignDel}, app.connGen)
	v := appView(t, app)
	if len(v.Lines) != 1 || v.Lines[0].ID != line || v.Lines[0].Content != "正文" {
		t.Fatalf("外来 Delete 须保留行: %+v", v.Lines)
	}
	if countBodyDisputes(v, line) != 2 {
		t.Fatalf("外来 Delete 应登记双方 body 候选: %+v", v.Disputes)
	}
	got := map[string]string{}
	for _, d := range v.Disputes {
		if d.RealLine == line {
			got[d.Person] = d.Action
		}
	}
	if got["甲"] != model.ActionEdit || got["乙"] != model.ActionDelete {
		t.Fatalf("本人保留行 Edit、对方 Delete: %v", got)
	}

	ownDelID := model.NewID()
	ownDel := protocol.Op{
		ID:   model.NewID().Hex(),
		Kind: protocol.TypeDisputeCC,
		DisputeCC: &protocol.DisputeCC{
			TargetPersonID: "乙",
			Claim: model.Dispute{
				ID:       ownDelID,
				RealLine: line,
				Action:   model.ActionDelete,
				Person:   "甲",
				Content:  nil,
			},
		},
	}
	// 新 App：仅本人 Delete CC，不得自争议。
	doc2, err := document.Load(base.Article, base.Lines, nil)
	if err != nil {
		t.Fatal(err)
	}
	app2 := NewAppWithStateDir(t.TempDir())
	app2.personID = "甲"
	app2.doc = doc2
	app2.relayReady = true
	app2.relayBooted = true
	app2.serverSnap = &protocol.Snapshot{Type: protocol.TypeSnapshot, View: base}
	app2.onRelay(protocol.RelayEvent{Type: protocol.TypeRelay, Op: ownDel}, app2.connGen)
	v2 := appView(t, app2)
	if countBodyDisputes(v2, line) != 0 {
		t.Fatalf("本人 Delete CC 不得进争议: %+v", v2.Disputes)
	}
	app2.mu.Lock()
	remembered := app2.claimIDs[line.Hex()+"\x00"+model.ActionEdit]
	app2.mu.Unlock()
	if remembered != ownDelID {
		t.Fatalf("本人 Delete CC 须记稳定 ID: got=%v want=%v", remembered, ownDelID)
	}
	app2.mu.Lock()
	viaEdit := app2.stableClaimIDLocked(line.Hex(), model.ActionEdit)
	viaDel := app2.stableClaimIDLocked(line.Hex(), model.ActionDelete)
	app2.mu.Unlock()
	if viaEdit != ownDelID || viaDel != ownDelID {
		t.Fatalf("Edit/Delete 同槽: edit=%v del=%v want=%v", viaEdit, viaDel, ownDelID)
	}
}
