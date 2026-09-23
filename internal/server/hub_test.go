package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/KDreamelf/collaborative-editor/internal/document"
	"github.com/KDreamelf/collaborative-editor/internal/model"
	"github.com/KDreamelf/collaborative-editor/internal/protocol"
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

func TestTwoJoinOwnLines(t *testing.T) {
	h := NewHub(nil)
	meta := h.CreateArticle("t")
	r := h.getRoom(meta.ID)
	h.join(r, &wsClient{}, "p1", "甲")
	h.join(r, &wsClient{}, "p2", "乙")
	v, err := h.GetView(meta.ID)
	if err != nil || len(v.Lines) < 2 {
		t.Fatalf("两人加入后应至少两行: %+v %v", v, err)
	}
	r.mu.Lock()
	cur2, ok := r.cursors["p2"]
	n := len(r.joinOrder)
	r.mu.Unlock()
	if !ok || cur2.LineID != v.Lines[1].ID.Hex() {
		t.Fatalf("第二人光标应在自己的行: %+v lines=%+v", cur2, v.Lines)
	}
	h.join(r, &wsClient{}, "p1", "甲")
	r.mu.Lock()
	n2 := len(r.joinOrder)
	r.mu.Unlock()
	if n != 2 || n2 != 2 {
		t.Fatalf("重复 join 不该再占位: %d %d", n, n2)
	}
}

func TestSubmitConflictKeepsFormalAndTwoDisputes(t *testing.T) {
	h := NewHub(nil)
	meta := h.CreateArticle("t")
	r := h.getRoom(meta.ID)
	h.join(r, &wsClient{}, "p1", "甲")
	h.join(r, &wsClient{}, "p2", "乙")
	v, _ := h.GetView(meta.ID)
	line := v.Lines[0].ID.Hex()
	h.applyBatch(r, nil, protocol.Batch{
		Seq: 1,
		Ops: []protocol.Op{{
			Kind: protocol.TypeSubmit,
			Submit: &protocol.Submit{
				PersonID: "p1",
				LineID:   line,
				Action:   model.ActionEdit,
				Content:  []string{"mine"},
			},
		}},
	})
	h.applyBatch(r, nil, protocol.Batch{
		Seq: 2,
		Ops: []protocol.Op{{
			Kind: protocol.TypeSubmit,
			Submit: &protocol.Submit{
				PersonID: "p2",
				LineID:   line,
				Action:   model.ActionEdit,
				Content:  []string{"yours"},
			},
		}},
	})
	after, err := h.GetView(meta.ID)
	if err != nil {
		t.Fatal(err)
	}
	if after.Lines[0].Content != "" {
		t.Fatalf("正式行不该被后到的盖掉: %+v", after.Lines[0])
	}
	if len(after.Disputes) != 2 {
		t.Fatalf("应有两份争议: %+v", after.Disputes)
	}
}

func TestFollowNeedsNodThenApplies(t *testing.T) {
	h := NewHub(nil)
	meta := h.CreateArticle("t")
	r := h.getRoom(meta.ID)
	h.join(r, &wsClient{}, "p1", "甲")
	h.join(r, &wsClient{}, "p2", "乙")
	v, _ := h.GetView(meta.ID)
	line := v.Lines[0].ID.Hex()
	h.applyBatch(r, nil, protocol.Batch{Seq: 1, Ops: []protocol.Op{{
		Kind:   protocol.TypeSubmit,
		Submit: &protocol.Submit{PersonID: "p1", LineID: line, Action: model.ActionEdit, Content: []string{"a"}},
	}}})
	h.applyBatch(r, nil, protocol.Batch{Seq: 2, Ops: []protocol.Op{{
		Kind:   protocol.TypeSubmit,
		Submit: &protocol.Submit{PersonID: "p2", LineID: line, Action: model.ActionEdit, Content: []string{"b"}},
	}}})
	after, _ := h.GetView(meta.ID)
	var p1Dispute model.ID
	for _, d := range after.Disputes {
		if d.Person == "p1" {
			p1Dispute = d.ID
		}
	}
	if p1Dispute.IsZero() {
		t.Fatal("找不到 p1 争议")
	}
	msgs := h.follow(r, &wsClient{personID: "p2", name: "乙"}, protocol.Follow{
		PersonID:  "p2",
		DisputeID: p1Dispute.Hex(),
		ClientTs:  1,
	})
	pending := false
	for _, m := range msgs {
		var head struct {
			Type   string `json:"type"`
			Status string `json:"status"`
		}
		if err := json.Unmarshal(m.data, &head); err != nil {
			t.Fatal(err)
		}
		if head.Type == protocol.TypeFollowResult && head.Status == document.FollowPending {
			pending = true
		}
	}
	if !pending {
		t.Fatalf("点头前应 pending，msgs=%d", len(msgs))
	}
	mid, _ := h.GetView(meta.ID)
	if len(mid.Disputes) != 2 || mid.Lines[0].Content != "" {
		t.Fatalf("点头前争议还在、正文未定: %+v", mid)
	}
	h.followAnswer(r, protocol.FollowAnswer{
		PersonID:  "p1",
		FromID:    "p2",
		DisputeID: p1Dispute.Hex(),
		Accept:    true,
	})
	done, _ := h.GetView(meta.ID)
	if len(done.Disputes) != 0 || done.Lines[0].Content != "a" {
		t.Fatalf("点头后正文应是被追随的那份: %+v", done)
	}
}

func twoClaims(t *testing.T) (*Hub, *room, string, string) {
	t.Helper()
	h := NewHub(nil)
	meta := h.CreateArticle("t")
	r := h.getRoom(meta.ID)
	h.join(r, &wsClient{}, "p1", "甲")
	h.join(r, &wsClient{}, "p2", "乙")
	v, _ := h.GetView(meta.ID)
	line := v.Lines[0].ID.Hex()
	h.applyBatch(r, nil, protocol.Batch{Seq: 1, Ops: []protocol.Op{{
		Kind:   protocol.TypeSubmit,
		Submit: &protocol.Submit{PersonID: "p1", LineID: line, Action: model.ActionEdit, Content: []string{"a"}},
	}}})
	h.applyBatch(r, nil, protocol.Batch{Seq: 2, Ops: []protocol.Op{{
		Kind:   protocol.TypeSubmit,
		Submit: &protocol.Submit{PersonID: "p2", LineID: line, Action: model.ActionEdit, Content: []string{"b"}},
	}}})
	after, _ := h.GetView(meta.ID)
	var p1Dispute string
	for _, d := range after.Disputes {
		if d.Person == "p1" {
			p1Dispute = d.ID.Hex()
		}
	}
	if p1Dispute == "" {
		t.Fatal("找不到 p1 争议")
	}
	return h, r, line, p1Dispute
}

func TestOnlineFollowAskAfterSnapshotAllowsAnswer(t *testing.T) {
	h := NewHub(nil)
	meta := h.CreateArticle("t")
	r := h.getRoom(meta.ID)
	p1 := &wsClient{personID: "p1", name: "甲"}
	p2 := &wsClient{personID: "p2", name: "乙"}
	h.join(r, p1, "p1", "甲")
	h.join(r, p2, "p2", "乙")
	v, _ := h.GetView(meta.ID)
	line := v.Lines[0].ID.Hex()
	h.applyBatch(r, nil, protocol.Batch{Seq: 1, Ops: []protocol.Op{{
		Kind:   protocol.TypeSubmit,
		Submit: &protocol.Submit{PersonID: "p1", LineID: line, Action: model.ActionEdit, Content: []string{"a"}},
	}}})
	h.applyBatch(r, nil, protocol.Batch{Seq: 2, Ops: []protocol.Op{{
		Kind:   protocol.TypeSubmit,
		Submit: &protocol.Submit{PersonID: "p2", LineID: line, Action: model.ActionEdit, Content: []string{"b"}},
	}}})
	after, _ := h.GetView(meta.ID)
	var p1Dispute string
	for _, d := range after.Disputes {
		if d.Person == "p1" {
			p1Dispute = d.ID.Hex()
		}
	}
	if p1Dispute == "" {
		t.Fatal("找不到 p1 争议")
	}

	assertTargetCanAnswer := func(t *testing.T, msgs []outbound) {
		t.Helper()
		var types []string
		var snap protocol.Snapshot
		var ask protocol.FollowAsk
		for _, m := range msgs {
			if m.client != p1 {
				continue
			}
			var head struct {
				Type string `json:"type"`
			}
			if err := json.Unmarshal(m.data, &head); err != nil {
				t.Fatal(err)
			}
			types = append(types, head.Type)
			switch head.Type {
			case protocol.TypeSnapshot:
				if err := json.Unmarshal(m.data, &snap); err != nil {
					t.Fatal(err)
				}
			case protocol.TypeFollowAsk:
				if err := json.Unmarshal(m.data, &ask); err != nil {
					t.Fatal(err)
				}
			}
		}
		snapAt, askAt := -1, -1
		for i, typ := range types {
			if typ == protocol.TypeSnapshot && snapAt < 0 {
				snapAt = i
			}
			if typ == protocol.TypeFollowAsk && askAt < 0 {
				askAt = i
			}
		}
		if snapAt < 0 || askAt < 0 {
			t.Fatalf("目标应同时收到 Snapshot 与 FollowAsk: %v", types)
		}
		if snapAt > askAt {
			t.Fatalf("目标连接须先 Snapshot 再 FollowAsk: %v", types)
		}
		var pendingOK bool
		for _, d := range snap.Disputes {
			if d.ID.Hex() == p1Dispute && len(d.Pending) == 1 && d.Pending[0].From == "p2" {
				pendingOK = true
			}
		}
		if !pendingOK {
			t.Fatalf("Snapshot 应含待确认: %+v", snap.Disputes)
		}
		loaded, err := document.Load(snap.Article, snap.Lines, snap.Disputes)
		if err != nil {
			t.Fatal(err)
		}
		disputeID, err := model.ParseID(ask.DisputeID)
		if err != nil {
			t.Fatal(err)
		}
		got, err := loaded.AnswerFollow("p1", ask.FromID, disputeID, true)
		if err != nil || got.Status != document.FollowApplied {
			t.Fatalf("Load(snapshot) 后应能点头: %+v %v", got, err)
		}
	}

	batchMsgs := h.applyBatch(r, p2, protocol.Batch{Seq: 3, Ops: []protocol.Op{{
		Kind:   protocol.TypeFollow,
		Follow: &protocol.Follow{PersonID: "p2", DisputeID: p1Dispute, ClientTs: 11},
	}}})
	assertTargetCanAnswer(t, batchMsgs)

	// 重建一场争议，测 legacy 直发 follow
	h2 := NewHub(nil)
	meta2 := h2.CreateArticle("t2")
	r2 := h2.getRoom(meta2.ID)
	p1b := &wsClient{personID: "p1", name: "甲"}
	p2b := &wsClient{personID: "p2", name: "乙"}
	h2.join(r2, p1b, "p1", "甲")
	h2.join(r2, p2b, "p2", "乙")
	v2, _ := h2.GetView(meta2.ID)
	line2 := v2.Lines[0].ID.Hex()
	h2.applyBatch(r2, nil, protocol.Batch{Seq: 1, Ops: []protocol.Op{{
		Kind:   protocol.TypeSubmit,
		Submit: &protocol.Submit{PersonID: "p1", LineID: line2, Action: model.ActionEdit, Content: []string{"a"}},
	}}})
	h2.applyBatch(r2, nil, protocol.Batch{Seq: 2, Ops: []protocol.Op{{
		Kind:   protocol.TypeSubmit,
		Submit: &protocol.Submit{PersonID: "p2", LineID: line2, Action: model.ActionEdit, Content: []string{"b"}},
	}}})
	after2, _ := h2.GetView(meta2.ID)
	var dispute2 string
	for _, d := range after2.Disputes {
		if d.Person == "p1" {
			dispute2 = d.ID.Hex()
		}
	}
	legacyMsgs := h2.follow(r2, p2b, protocol.Follow{PersonID: "p2", DisputeID: dispute2, ClientTs: 12})
	// 复用同一断言逻辑，换目标 client 与争议 ID
	p1, p1Dispute = p1b, dispute2
	assertTargetCanAnswer(t, legacyMsgs)
}

func TestBatchFollowKeepsOrderAndDedups(t *testing.T) {
	h, r, line, p1Dispute := twoClaims(t)
	opID := "follow-1"
	follow := protocol.Op{
		ID:   opID,
		Kind: protocol.TypeFollow,
		Follow: &protocol.Follow{
			PersonID:  "p2",
			DisputeID: p1Dispute,
			ClientTs:  1,
		},
	}
	edit := protocol.Op{
		ID:     "edit-after",
		Kind:   protocol.TypeSubmit,
		Submit: &protocol.Submit{PersonID: "p2", LineID: line, Action: model.ActionEdit, Content: []string{"b2"}},
	}
	ackMsgs := h.applyBatch(r, &wsClient{personID: "p2", name: "乙"}, protocol.Batch{
		Seq: 3,
		Ops: []protocol.Op{follow, edit},
	})
	gotAck := false
	gotPending := false
	for _, m := range ackMsgs {
		var head struct {
			Type    string `json:"type"`
			Status  string `json:"status"`
			Applied int    `json:"applied"`
			Message string `json:"message"`
		}
		if err := json.Unmarshal(m.data, &head); err != nil {
			t.Fatal(err)
		}
		if head.Type == protocol.TypeAck {
			gotAck = true
			if head.Applied != 2 || head.Message != "" {
				t.Fatalf("应按序执行 follow 再 edit: %+v", head)
			}
		}
		if head.Type == protocol.TypeFollowResult && head.Status == document.FollowPending {
			gotPending = true
		}
	}
	if !gotAck || !gotPending {
		t.Fatalf("应有 ack 和 pending，msgs=%d ack=%v pending=%v", len(ackMsgs), gotAck, gotPending)
	}

	dup := h.applyBatch(r, &wsClient{personID: "p2", name: "乙"}, protocol.Batch{
		Seq: 4,
		Ops: []protocol.Op{follow},
	})
	for _, m := range dup {
		var head struct {
			Type    string `json:"type"`
			Applied int    `json:"applied"`
			Message string `json:"message"`
			Status  string `json:"status"`
		}
		_ = json.Unmarshal(m.data, &head)
		if head.Type == protocol.TypeAck && (head.Applied != 1 || head.Message != "") {
			t.Fatalf("同 ID 重发应算已应用、不再报错: %+v", head)
		}
		if head.Type == protocol.TypeFollowResult {
			t.Fatalf("去重后不该再发 FollowResult: %s", m.data)
		}
	}
	mid, _ := h.GetView(tArticle(r))
	var p1 model.Dispute
	for _, d := range mid.Disputes {
		if d.Person == "p1" {
			p1 = d
		}
	}
	if len(p1.Followers) != 0 {
		t.Fatalf("pending 追随不该已经点头: %+v", p1)
	}
}

func tArticle(r *room) string {
	return r.doc.Article().ID.Hex()
}

func TestOfflineFollowAskDeliveredOnJoin(t *testing.T) {
	h := NewHub(nil)
	meta := h.CreateArticle("t")
	r := h.getRoom(meta.ID)
	p2 := &wsClient{personID: "p2", name: "乙"}
	h.join(r, p2, "p2", "乙")
	v, _ := h.GetView(meta.ID)
	line := v.Lines[0].ID.Hex()
	h.applyBatch(r, nil, protocol.Batch{Seq: 1, Ops: []protocol.Op{{
		Kind:   protocol.TypeSubmit,
		Submit: &protocol.Submit{PersonID: "p1", LineID: line, Action: model.ActionEdit, Content: []string{"a"}},
	}}})
	h.applyBatch(r, nil, protocol.Batch{Seq: 2, Ops: []protocol.Op{{
		Kind:   protocol.TypeSubmit,
		Submit: &protocol.Submit{PersonID: "p2", LineID: line, Action: model.ActionEdit, Content: []string{"b"}},
	}}})
	after, _ := h.GetView(meta.ID)
	var p1Dispute string
	for _, d := range after.Disputes {
		if d.Person == "p1" {
			p1Dispute = d.ID.Hex()
		}
	}
	if p1Dispute == "" {
		t.Fatal("找不到 p1 争议")
	}
	r.mu.Lock()
	r.dirty = false
	r.mu.Unlock()

	msgs := h.follow(r, p2, protocol.Follow{PersonID: "p2", DisputeID: p1Dispute, ClientTs: 7})
	for _, m := range msgs {
		var head struct {
			Type string `json:"type"`
		}
		_ = json.Unmarshal(m.data, &head)
		if head.Type == protocol.TypeFollowAsk {
			t.Fatal("目标离线时不应发出 FollowAsk")
		}
	}
	mid, _ := h.GetView(meta.ID)
	var pendingN int
	for _, d := range mid.Disputes {
		if d.Person == "p1" {
			pendingN = len(d.Pending)
			if pendingN != 1 || d.Pending[0].From != "p2" || d.Pending[0].ClientTs != 7 {
				t.Fatalf("离线请求应写入待确认: %+v", d)
			}
		}
	}
	if pendingN != 1 {
		t.Fatal("应有一条待确认")
	}
	r.mu.Lock()
	if !r.dirty {
		r.mu.Unlock()
		t.Fatal("pending 追随应标 dirty 待刷 Mongo")
	}
	r.mu.Unlock()

	p1 := &wsClient{}
	joinMsgs := h.join(r, p1, "p1", "甲")
	var ask protocol.FollowAsk
	gotAsk := false
	for _, m := range joinMsgs {
		if m.client != p1 {
			continue
		}
		var head struct {
			Type string `json:"type"`
		}
		if err := json.Unmarshal(m.data, &head); err != nil {
			t.Fatal(err)
		}
		if head.Type == protocol.TypeFollowAsk {
			if err := json.Unmarshal(m.data, &ask); err != nil {
				t.Fatal(err)
			}
			gotAsk = true
		}
	}
	if !gotAsk {
		t.Fatalf("目标 join 应补发 FollowAsk，msgs=%d", len(joinMsgs))
	}
	if ask.FromID != "p2" || ask.FromName != "乙" || ask.DisputeID != p1Dispute || ask.ClientTs != 7 {
		t.Fatalf("补发 FollowAsk 字段不对: %+v", ask)
	}

	// 再 join 一次只重发询问，不增加待确认
	p1b := &wsClient{}
	h.join(r, p1b, "p1", "甲")
	still, _ := h.GetView(meta.ID)
	for _, d := range still.Disputes {
		if d.Person == "p1" && len(d.Pending) != 1 {
			t.Fatalf("重连不得复制待确认: %+v", d)
		}
	}

	h.followAnswer(r, protocol.FollowAnswer{
		PersonID: "p1", FromID: "p2", DisputeID: p1Dispute, Accept: true,
	})
	done, _ := h.GetView(meta.ID)
	if len(done.Disputes) != 0 || done.Lines[0].Content != "a" {
		t.Fatalf("点头后应收束: %+v", done)
	}
}

func TestFollowAskFromNameFallback(t *testing.T) {
	h := NewHub(nil)
	meta := h.CreateArticle("t")
	r := h.getRoom(meta.ID)
	h.join(r, &wsClient{}, "p1", "甲")
	v, _ := h.GetView(meta.ID)
	line := v.Lines[0].ID.Hex()
	h.applyBatch(r, nil, protocol.Batch{Seq: 1, Ops: []protocol.Op{{
		Kind:   protocol.TypeSubmit,
		Submit: &protocol.Submit{PersonID: "p1", LineID: line, Action: model.ActionEdit, Content: []string{"a"}},
	}}})
	h.applyBatch(r, nil, protocol.Batch{Seq: 2, Ops: []protocol.Op{{
		Kind:   protocol.TypeSubmit,
		Submit: &protocol.Submit{PersonID: "ghost", LineID: line, Action: model.ActionEdit, Content: []string{"b"}},
	}}})
	after, _ := h.GetView(meta.ID)
	var p1Dispute string
	for _, d := range after.Disputes {
		if d.Person == "p1" {
			p1Dispute = d.ID.Hex()
		}
	}
	// ghost 从未 join，names 里没有；follow 时 client.name 也空
	h.follow(r, &wsClient{personID: "ghost"}, protocol.Follow{
		PersonID: "ghost", DisputeID: p1Dispute, ClientTs: 1,
	})
	joinMsgs := h.join(r, &wsClient{}, "p1", "甲")
	for _, m := range joinMsgs {
		var ask protocol.FollowAsk
		if err := json.Unmarshal(m.data, &ask); err != nil {
			continue
		}
		if ask.Type == protocol.TypeFollowAsk {
			if ask.FromName != "有人" || ask.FromID != "ghost" {
				t.Fatalf("离线无名发起人 FromName 应为「有人」: %+v", ask)
			}
			return
		}
	}
	t.Fatal("应收到 FollowAsk")
}

func TestBatchAnswerFollowDedupDoesNotRerun(t *testing.T) {
	h, r, _, p1Dispute := twoClaims(t)
	h.applyBatch(r, nil, protocol.Batch{Seq: 3, Ops: []protocol.Op{{
		ID:     "f1",
		Kind:   protocol.TypeFollow,
		Follow: &protocol.Follow{PersonID: "p2", DisputeID: p1Dispute, ClientTs: 1},
	}}})
	ans := protocol.Op{
		ID:   "ans-1",
		Kind: protocol.TypeFollowAnswer,
		FollowAnswer: &protocol.FollowAnswer{
			PersonID:  "p1",
			FromID:    "p2",
			DisputeID: p1Dispute,
			Accept:    true,
		},
	}
	h.applyBatch(r, nil, protocol.Batch{Seq: 4, Ops: []protocol.Op{ans}})
	done, _ := h.GetView(tArticle(r))
	if len(done.Disputes) != 0 || done.Lines[0].Content != "a" {
		t.Fatalf("点头后正文应是被追随的那份: %+v", done)
	}
	again := h.applyBatch(r, nil, protocol.Batch{Seq: 5, Ops: []protocol.Op{ans}})
	for _, m := range again {
		var head struct {
			Type    string `json:"type"`
			Applied int    `json:"applied"`
			Message string `json:"message"`
		}
		_ = json.Unmarshal(m.data, &head)
		if head.Type == protocol.TypeAck && (head.Applied != 1 || head.Message != "") {
			t.Fatalf("AnswerFollow 重发不该再执行或打断后续: %+v", head)
		}
	}
	still, _ := h.GetView(tArticle(r))
	if len(still.Disputes) != 0 || still.Lines[0].Content != "a" {
		t.Fatalf("去重后文档不该变: %+v", still)
	}
}

func TestSpanEditBatchOneOp(t *testing.T) {
	h := NewHub(nil)
	meta := h.CreateArticle("t")
	r := h.getRoom(meta.ID)
	h.join(r, &wsClient{}, "p1", "甲")
	v, _ := h.GetView(meta.ID)
	line := v.Lines[0].ID.Hex()
	id1, id2 := model.NewID(), model.NewID()
	h.applyBatch(r, nil, protocol.Batch{Seq: 1, Ops: []protocol.Op{{
		ID:   "paste-1",
		Kind: protocol.TypeSubmit,
		Submit: &protocol.Submit{
			PersonID: "p1",
			LineID:   line,
			Action:   model.ActionEdit,
			Content:  []string{"A", "B", "C"},
			LineIDs:  []string{id1.Hex(), id2.Hex()},
		},
	}}})
	mid, err := h.GetView(meta.ID)
	if err != nil || len(mid.Lines) != 3 {
		t.Fatalf("粘贴三行: %+v %v", mid, err)
	}
	spanID := model.NewID()
	client := &wsClient{}
	msgs := h.applyBatch(r, client, protocol.Batch{Seq: 2, Ops: []protocol.Op{{
		ID:   "span-1",
		Kind: protocol.TypeSpanEdit,
		SpanEdit: &protocol.SpanEdit{
			Type:        protocol.TypeSpanEdit,
			PersonID:    "p1",
			BaseIDs:     []string{mid.Lines[0].ID.Hex(), mid.Lines[1].ID.Hex()},
			BaseTexts:   []string{"A", "B"},
			AfterSeen:   mid.Lines[2].ID.Hex(),
			Replacement: []string{"X", "Y"},
			LineIDs:     []string{spanID.Hex()},
		},
	}}})
	acked := false
	for _, m := range msgs {
		var head struct {
			Type    string `json:"type"`
			Applied int    `json:"applied"`
			Message string `json:"message"`
		}
		if err := json.Unmarshal(m.data, &head); err != nil {
			continue
		}
		if head.Type == protocol.TypeAck {
			acked = true
			if head.Applied != 1 || head.Message != "" {
				t.Fatalf("spanEdit 应整包成功: %+v", head)
			}
		}
	}
	if !acked {
		t.Fatal("应有 Ack")
	}
	after, _ := h.GetView(meta.ID)
	if len(after.Lines) != 3 || after.Lines[0].Content != "X" || after.Lines[1].Content != "Y" || after.Lines[2].Content != "C" {
		t.Fatalf("服务端正文: %+v", after.Lines)
	}
	if after.Lines[0].ID != mid.Lines[0].ID || after.Lines[1].ID != spanID || after.Lines[2].ID != mid.Lines[2].ID {
		t.Fatalf("ID 稳定/预生: %+v", after.Lines)
	}
}

func TestInsertBeforeDisputeAnchorIsOriginalLine(t *testing.T) {
	h := NewHub(nil)
	meta := h.CreateArticle("t")
	r := h.getRoom(meta.ID)
	h.join(r, &wsClient{}, "p1", "甲")
	h.join(r, &wsClient{}, "p2", "乙")
	v, _ := h.GetView(meta.ID)
	if len(v.Lines) < 2 {
		_ = r.doc.EnsureLines(2)
		v, _ = h.GetView(meta.ID)
	}
	l1, l2 := v.Lines[0].ID, v.Lines[1].ID
	idA, idB := model.NewID(), model.NewID()
	before := l1.Hex()
	h.applyBatch(r, nil, protocol.Batch{Seq: 1, Ops: []protocol.Op{{
		ID:   "before-a",
		Kind: protocol.TypeSubmit,
		Submit: &protocol.Submit{
			PersonID:   "p1",
			LineID:     l2.Hex(),
			Action:     model.ActionInsertBefore,
			Content:    []string{"甲前"},
			BeforeSeen: &before,
			LineIDs:    []string{idA.Hex()},
		},
	}}})
	h.applyBatch(r, nil, protocol.Batch{Seq: 2, Ops: []protocol.Op{{
		ID:   "before-b",
		Kind: protocol.TypeSubmit,
		Submit: &protocol.Submit{
			PersonID:   "p2",
			LineID:     l2.Hex(),
			Action:     model.ActionInsertBefore,
			Content:    []string{"乙前"},
			BeforeSeen: &before,
			LineIDs:    []string{idB.Hex()},
		},
	}}})
	after, err := h.GetView(meta.ID)
	if err != nil {
		t.Fatal(err)
	}
	found := 0
	for _, d := range after.Disputes {
		if d.Action != model.ActionInsertBefore {
			continue
		}
		found++
		if d.RealLine != l2 {
			t.Fatalf("before 争议 anchor 须为下方原行: got %s want %s action=%s content=%v",
				d.RealLine.Hex(), l2.Hex(), d.Action, d.Content)
		}
		if len(d.Content) != 1 || (d.Content[0] != "甲前" && d.Content[0] != "乙前") {
			t.Fatalf("Content 仅插入段: %+v", d)
		}
	}
	if found < 2 {
		t.Fatalf("应有两份插在前面争议: disputes=%+v lines=%+v", after.Disputes, after.Lines)
	}
}

func TestInsertAfterStillWorksWithBeforeSeenField(t *testing.T) {
	h := NewHub(nil)
	meta := h.CreateArticle("t")
	r := h.getRoom(meta.ID)
	h.join(r, &wsClient{}, "p1", "甲")
	v, _ := h.GetView(meta.ID)
	if len(v.Lines) < 2 {
		_ = r.doc.EnsureLines(2)
		v, _ = h.GetView(meta.ID)
	}
	anchor, tail := v.Lines[0].ID, v.Lines[1].ID
	newID := model.NewID()
	afterSeen := tail.Hex()
	msgs := h.applyBatch(r, &wsClient{}, protocol.Batch{Seq: 1, Ops: []protocol.Op{{
		ID:   "after-1",
		Kind: protocol.TypeSubmit,
		Submit: &protocol.Submit{
			PersonID:  "p1",
			LineID:    anchor.Hex(),
			Action:    model.ActionInsert,
			Content:   []string{"后插"},
			AfterSeen: &afterSeen,
			LineIDs:   []string{newID.Hex()},
		},
	}}})
	acked := false
	for _, m := range msgs {
		var head struct {
			Type    string `json:"type"`
			Applied int    `json:"applied"`
			Message string `json:"message"`
		}
		if err := json.Unmarshal(m.data, &head); err != nil {
			continue
		}
		if head.Type == protocol.TypeAck {
			acked = true
			if head.Applied != 1 || head.Message != "" {
				t.Fatalf("旧 after 应成功: %+v", head)
			}
		}
	}
	if !acked {
		t.Fatal("应有 Ack")
	}
	after, _ := h.GetView(meta.ID)
	if len(after.Lines) < 3 || after.Lines[1].ID != newID || after.Lines[1].Content != "后插" {
		t.Fatalf("旧 after 入链: %+v", after.Lines)
	}
}

func TestSpanEditUnknownKindRejected(t *testing.T) {
	h := NewHub(nil)
	meta := h.CreateArticle("t")
	r := h.getRoom(meta.ID)
	h.join(r, &wsClient{}, "p1", "甲")
	before, _ := h.GetView(meta.ID)
	client := &wsClient{}
	msgs := h.applyBatch(r, client, protocol.Batch{Seq: 1, Ops: []protocol.Op{{
		ID:   "bad-kind",
		Kind: "spanEditTypo",
		SpanEdit: &protocol.SpanEdit{
			PersonID:    "p1",
			BaseIDs:     []string{before.Lines[0].ID.Hex(), model.NewID().Hex()},
			BaseTexts:   []string{"", "x"},
			Replacement: []string{"Z"},
		},
	}}})
	gotReject := false
	for _, m := range msgs {
		var head struct {
			Type    string `json:"type"`
			Applied int    `json:"applied"`
			Message string `json:"message"`
		}
		if err := json.Unmarshal(m.data, &head); err != nil {
			continue
		}
		if head.Type == protocol.TypeAck {
			gotReject = true
			if head.Applied != 0 || !strings.Contains(head.Message, "未知操作") {
				t.Fatalf("未知 kind 须拒绝: %+v", head)
			}
		}
	}
	if !gotReject {
		t.Fatal("应有拒绝 Ack")
	}
	after, _ := h.GetView(meta.ID)
	if after.Lines[0].Content != before.Lines[0].Content {
		t.Fatalf("拒绝不得改正文: %+v", after.Lines)
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
