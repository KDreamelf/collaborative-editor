package main

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
