package server

import (
	"encoding/json"
	"errors"
	"log"
	"net/http"
	"os"
)

func Main() {
	addr := env("ADDR", ":8787")
	hub, err := openHub(os.Getenv("MONGO_URI"), os.Getenv("MONGO_DB"))
	if err != nil {
		log.Fatal(err)
	}
	stop := make(chan struct{})
	go hub.StartFlush(stop)

	log.Printf("listen %s", addr)
	if err := http.ListenAndServe(addr, hub.Handler()); err != nil {
		log.Fatal(err)
	}
}

// openHub：MONGO_URI 空=纯内存；非空则连 Mongo，失败直接返回（不降级）。
func openHub(mongoURI, mongoDB string) (*Hub, error) {
	if mongoURI == "" {
		return NewHub(nil), nil
	}
	if mongoDB == "" {
		mongoDB = "editor"
	}
	s := connectMongo(mongoURI, mongoDB)
	if s == nil {
		return nil, errors.New("mongo 不可用")
	}
	return NewHub(s), nil
}

// Handler 正式 HTTP/WS 入口（建文 + Join）。集成测试与 Main 共用。
func (h *Hub) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /api/articles", h.handleCreate)
	mux.HandleFunc("GET /api/articles", h.handleList)
	mux.HandleFunc("GET /api/articles/{id}", h.handleGet)
	mux.HandleFunc("GET /ws", h.handleWS)
	mux.HandleFunc("OPTIONS /api/articles", handleOptions)
	mux.HandleFunc("OPTIONS /api/articles/{id}", handleOptions)
	mux.HandleFunc("OPTIONS /ws", handleOptions)
	return cors(mux)
}

func env(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}

func cors(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Access-Control-Allow-Origin", "*")
		w.Header().Set("Access-Control-Allow-Methods", "GET, POST, OPTIONS")
		w.Header().Set("Access-Control-Allow-Headers", "Content-Type")
		if r.Method == http.MethodOptions {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		next.ServeHTTP(w, r)
	})
}

func handleOptions(w http.ResponseWriter, r *http.Request) {
	w.WriteHeader(http.StatusNoContent)
}

func (h *Hub) handleCreate(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Title string `json:"title"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		http.Error(w, "坏请求", http.StatusBadRequest)
		return
	}
	meta := h.CreateArticle(body.Title)
	writeJSON(w, http.StatusOK, meta)
}

func (h *Hub) handleList(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, h.ListArticles())
}

func (h *Hub) handleGet(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	v, err := h.GetView(id)
	if err != nil {
		writeHTTPError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, v)
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}
