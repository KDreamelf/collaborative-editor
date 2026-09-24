package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/KDreamelf/collaborative-editor/internal/document"
	"github.com/KDreamelf/collaborative-editor/internal/model"
	"github.com/KDreamelf/collaborative-editor/internal/protocol"
	"github.com/gorilla/websocket"
)

func TestHTTPCreateAndList(t *testing.T) {
	h := NewHub(nil)
	mux := http.NewServeMux()
	mux.HandleFunc("POST /api/articles", h.handleCreate)
	mux.HandleFunc("GET /api/articles", h.handleList)
	req := httptest.NewRequest(http.MethodPost, "/api/articles", strings.NewReader(`{"title":"面试"}`))
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("创建应 200，得到 %d %s", rec.Code, rec.Body.String())
	}
	var meta articleMeta
	if err := json.Unmarshal(rec.Body.Bytes(), &meta); err != nil || meta.ID == "" || meta.Title != "面试" {
		t.Fatalf("创建响应不对: %s %v", rec.Body.String(), err)
	}
	req = httptest.NewRequest(http.MethodGet, "/api/articles", nil)
	rec = httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), meta.ID) {
		t.Fatalf("列表应包含新文章: %d %s", rec.Code, rec.Body.String())
	}
}

func TestCreateArticle(t *testing.T) {
	h := NewHub(nil)
	meta := h.CreateArticle("")
	if meta.Title != "未命名" || meta.ID == "" {
		t.Fatalf("空标题应写成未命名: %+v", meta)
	}
	list := h.ListArticles()
	if len(list) != 1 || list[0].ID != meta.ID {
		t.Fatalf("列表应有新建文章: %+v", list)
	}
	v, err := h.GetView(meta.ID)
	if err != nil || v.Article.Title != "未命名" || len(v.Lines) != 1 {
		t.Fatalf("应能取到视图: %+v %v", v, err)
	}
}

func TestFormalWSNeutralPath(t *testing.T) {
	h := NewHub(nil)
	meta := h.CreateArticle("t")
	srv := httptest.NewServer(http.HandlerFunc(h.handleWS))
	defer srv.Close()
	wsURL := "ws" + strings.TrimPrefix(srv.URL, "http")

	dial := func(t *testing.T) *websocket.Conn {
		t.Helper()
		c, _, err := websocket.DefaultDialer.Dial(wsURL, nil)
		if err != nil {
			t.Fatal(err)
		}
		return c
	}
	writeJSON := func(t *testing.T, c *websocket.Conn, v any) {
		t.Helper()
		if err := c.WriteJSON(v); err != nil {
			t.Fatal(err)
		}
	}
	readType := func(t *testing.T, c *websocket.Conn, want string) []byte {
		t.Helper()
		_ = c.SetReadDeadline(time.Now().Add(2 * time.Second))
		for {
			_, data, err := c.ReadMessage()
			if err != nil {
				t.Fatalf("读 %s: %v", want, err)
			}
			var head struct {
				Type string `json:"type"`
			}
			if err := json.Unmarshal(data, &head); err != nil {
				t.Fatal(err)
			}
			if head.Type == want {
				return data
			}
			// 丢弃他人光标/结构 Snapshot 等旁路包，继续等目标类型。
		}
	}
	joinBoot := func(t *testing.T, c *websocket.Conn, personID, name string) protocol.Bootstrap {
		t.Helper()
		writeJSON(t, c, protocol.Join{
			Type: protocol.TypeJoin, ArticleID: meta.ID, PersonID: personID, Name: name,
		})
		raw := readType(t, c, protocol.TypeBootstrap)
		var boot protocol.Bootstrap
		if err := json.Unmarshal(raw, &boot); err != nil {
			t.Fatal(err)
		}
		return boot
	}

	a := dial(t)
	defer a.Close()
	b := dial(t)
	defer b.Close()

	bootA := joinBoot(t, a, "A", "甲")
	if bootA.YourLine == "" || len(bootA.Base.Lines) < 1 {
		t.Fatalf("A Bootstrap: %+v", bootA)
	}
	bootB := joinBoot(t, b, "B", "乙")
	if bootB.YourLine == bootA.YourLine || len(bootB.Base.Lines) != 2 {
		t.Fatalf("B 应分到第二条初始行: %+v", bootB)
	}
	line := bootA.Base.Lines[0].ID.Hex()

	// 普通 Edit：Relay，不造争议，不广播全局 Snapshot。
	writeJSON(t, a, protocol.Batch{
		Type: protocol.TypeBatch, Seq: 1,
		Ops: []protocol.Op{{
			ID: "e1", Kind: protocol.TypeSubmit,
			Submit: &protocol.Submit{PersonID: "A", LineID: line, Action: model.ActionEdit, Content: []string{"甲"}},
		}},
	})
	ackRaw := readType(t, a, protocol.TypeAck)
	var ack protocol.Ack
	if err := json.Unmarshal(ackRaw, &ack); err != nil || ack.Applied != 1 || ack.Message != "" {
		t.Fatalf("Edit ACK: %s err=%v", ackRaw, err)
	}
	relayRaw := readType(t, b, protocol.TypeRelay)
	var editEv protocol.RelayEvent
	if err := json.Unmarshal(relayRaw, &editEv); err != nil || editEv.Op.ID != "e1" {
		t.Fatalf("B 应收 Edit Relay: %s", relayRaw)
	}
	if n := len(mustView(t, h.getRoom(meta.ID)).Disputes); n != 0 {
		t.Fatalf("普通 Edit 无争议: %d", n)
	}

	claimID := model.NewID()
	writeJSON(t, a, protocol.Batch{
		Type: protocol.TypeBatch, Seq: 2,
		Ops: []protocol.Op{{
			ID: "ccA", Kind: protocol.TypeDisputeCC,
			DisputeCC: &protocol.DisputeCC{
				TargetPersonID: "B",
				Claim: model.Dispute{
					ID: claimID, RealLine: bootA.Base.Lines[0].ID, Action: model.ActionEdit, Person: "A",
					Content: []string{"甲主张"}, Followers: []string{},
				},
			},
		}},
	})
	_ = readType(t, a, protocol.TypeAck)
	_ = readType(t, b, protocol.TypeRelay)

	// late Join：Bootstrap.Disputes 含主张。
	c := dial(t)
	defer c.Close()
	bootC := joinBoot(t, c, "C", "丙")
	if len(bootC.Disputes) != 1 || bootC.Disputes[0].ID != claimID {
		t.Fatalf("late Join Disputes: %+v", bootC.Disputes)
	}
	for _, d := range bootC.Disputes {
		if len(d.Pending) != 0 && d.Pending[0].From == "" {
			t.Fatalf("Pending 形状异常: %+v", d)
		}
	}
	_ = c.Close()

	// 身份伪造 Cursor 被覆盖（跳过 C leave 等旁路 Cursors）。
	writeJSON(t, a, protocol.CursorMsg{
		Type: protocol.TypeCursor,
		Cursor: protocol.Cursor{
			PersonID: "forged", Name: "冒充", LineID: line, Offset: 3, SelEnd: 3,
		},
	})
	cursorOK := false
	_ = b.SetReadDeadline(time.Now().Add(2 * time.Second))
	for !cursorOK {
		_, curRaw, err := b.ReadMessage()
		if err != nil {
			t.Fatalf("读 Cursors: %v", err)
		}
		if outboundType(t, curRaw) != protocol.TypeCursors {
			continue
		}
		var curs cursorsMsg
		if err := json.Unmarshal(curRaw, &curs); err != nil {
			t.Fatal(err)
		}
		for _, cur := range curs.Cursors {
			if cur.PersonID == "forged" || cur.Name == "冒充" {
				t.Fatalf("光标身份未覆盖: %+v", curs.Cursors)
			}
			if cur.PersonID == "A" && cur.Name == "甲" && cur.Offset == 3 {
				cursorOK = true
			}
		}
	}

	// ACK 失败 Message 自然且非空。
	writeJSON(t, a, protocol.Batch{
		Type: protocol.TypeBatch, Seq: 3,
		Ops: []protocol.Op{{
			ID: "bad", Kind: protocol.TypeSubmit,
			Submit: &protocol.Submit{PersonID: "A", LineID: "not-a-line-id", Action: model.ActionEdit, Content: []string{"拒"}},
		}},
	})
	failRaw := readType(t, a, protocol.TypeAck)
	var failAck protocol.Ack
	if err := json.Unmarshal(failRaw, &failAck); err != nil {
		t.Fatal(err)
	}
	if failAck.Applied != 0 || failAck.Message == "" || ackHasProtocolLeak(failAck.Message) {
		t.Fatalf("失败 ACK 应自然非空: %+v", failAck)
	}

	// 目标离线 Follow → 重连收 raw Follow → Answer。
	claimB := model.NewID()
	writeJSON(t, b, protocol.Batch{
		Type: protocol.TypeBatch, Seq: 1,
		Ops: []protocol.Op{{
			ID: "ccB", Kind: protocol.TypeDisputeCC,
			DisputeCC: &protocol.DisputeCC{
				TargetPersonID: "A",
				Claim: model.Dispute{
					ID: claimB, RealLine: bootA.Base.Lines[0].ID, Action: model.ActionEdit, Person: "B",
					Content: []string{"乙主张"}, Followers: []string{},
				},
			},
		}},
	})
	_ = readType(t, b, protocol.TypeAck)
	_ = readType(t, a, protocol.TypeRelay)

	_ = b.Close()
	// leave 异步；稍等房间摘掉 B。
	deadline := time.Now().Add(2 * time.Second)
	for {
		r := h.getRoom(meta.ID)
		r.mu.Lock()
		n := 0
		for c := range r.clients {
			if c.personID == "B" {
				n++
			}
		}
		r.mu.Unlock()
		if n == 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("B 离开超时")
		}
		time.Sleep(20 * time.Millisecond)
	}

	writeJSON(t, a, protocol.Batch{
		Type: protocol.TypeBatch, Seq: 4,
		Ops: []protocol.Op{{
			ID: "f1", Kind: protocol.TypeFollow,
			Follow: &protocol.Follow{PersonID: "A", DisputeID: claimB.Hex(), ClientTs: 9},
		}},
	})
	_ = readType(t, a, protocol.TypeAck)

	b2 := dial(t)
	defer b2.Close()
	writeJSON(t, b2, protocol.Join{
		Type: protocol.TypeJoin, ArticleID: meta.ID, PersonID: "B", Name: "乙",
	})
	bootB2Raw := readType(t, b2, protocol.TypeBootstrap)
	var bootB2 protocol.Bootstrap
	if err := json.Unmarshal(bootB2Raw, &bootB2); err != nil {
		t.Fatal(err)
	}
	pendingOK := false
	for _, d := range bootB2.Disputes {
		if d.ID == claimB {
			for _, p := range d.Pending {
				if p.From == "A" && p.To == "B" && p.ClientTs == 9 {
					pendingOK = true
				}
			}
		}
	}
	if !pendingOK {
		t.Fatalf("重连 Bootstrap 应含 Pending: %+v", bootB2.Disputes)
	}
	followRaw := readType(t, b2, protocol.TypeRelay)
	var followEv protocol.RelayEvent
	if err := json.Unmarshal(followRaw, &followEv); err != nil {
		t.Fatal(err)
	}
	if followEv.Op.Kind != protocol.TypeFollow || followEv.Op.Follow == nil ||
		followEv.Op.Follow.PersonID != "A" || followEv.Op.Follow.DisputeID != claimB.Hex() ||
		followEv.Op.Follow.ClientTs != 9 {
		t.Fatalf("重连应 raw Follow: %+v", followEv)
	}
	wantID := pendingFollowOpID(claimB.Hex(), "A", "B", 9)
	if followEv.Op.ID != wantID {
		t.Fatalf("pending Op.ID: got %q want %q", followEv.Op.ID, wantID)
	}
	if strings.Contains(string(followRaw), protocol.TypeFollowAsk) {
		t.Fatal("不得发旧 FollowAsk")
	}

	writeJSON(t, b2, protocol.Batch{
		Type: protocol.TypeBatch, Seq: 2,
		Ops: []protocol.Op{{
			ID: "ans1", Kind: protocol.TypeFollowAnswer,
			FollowAnswer: &protocol.FollowAnswer{
				PersonID: "B", FromID: "A", DisputeID: claimB.Hex(), Accept: true,
			},
		}},
	})
	ansAckRaw := readType(t, b2, protocol.TypeAck)
	var ansAck protocol.Ack
	if err := json.Unmarshal(ansAckRaw, &ansAck); err != nil || ansAck.Applied != 1 || ansAck.Message != "" {
		t.Fatalf("Answer ACK: %s", ansAckRaw)
	}
	done := mustView(t, h.getRoom(meta.ID))
	if len(done.Disputes) != 0 || done.Lines[0].Content != "乙主张" {
		t.Fatalf("Answer 后应收束到乙主张: %+v", done)
	}
}

func TestParseSpanEditOptsStrict(t *testing.T) {
	_, err := protocol.ParseSpanEditOpts(nil)
	if err == nil {
		t.Fatal("nil 应失败")
	}
	_, err = protocol.ParseSpanEditOpts(&protocol.SpanEdit{
		PersonID: "p", BaseIDs: []string{model.NewID().Hex()}, BaseTexts: []string{"a"}, Replacement: []string{"x"},
	})
	if err != document.ErrSpanBase {
		t.Fatalf("少于 2 行: %v", err)
	}
	id1, id2 := model.NewID(), model.NewID()
	_, err = protocol.ParseSpanEditOpts(&protocol.SpanEdit{
		PersonID: "p", BaseIDs: []string{id1.Hex(), id2.Hex()}, BaseTexts: []string{"a", "b"},
		Replacement: []string{"x", "y"}, LineIDs: nil,
	})
	if err != document.ErrLineIDs {
		t.Fatalf("缺 LineIDs: %v", err)
	}
	_, err = protocol.ParseSpanEditOpts(&protocol.SpanEdit{
		PersonID: "p", BaseIDs: []string{id1.Hex(), "not-hex"}, BaseTexts: []string{"a", "b"},
		Replacement: []string{"x"},
	})
	if err != document.ErrSpanBase {
		t.Fatalf("非法 ID: %v", err)
	}
}
