package main

import (
	"encoding/json"
	"log"
	"net/http"
	"os"
)

func main() {
	addr := env("ADDR", ":8787")
	uri := env("MONGO_URI", "mongodb://127.0.0.1:27017")
	dbName := env("MONGO_DB", "editor")

	store := connectMongo(uri, dbName)
	hub := NewHub(store)
	stop := make(chan struct{})
	go hub.StartFlush(stop)

	mux := http.NewServeMux()
	mux.HandleFunc("POST /api/articles", hub.handleCreate)
	mux.HandleFunc("GET /api/articles", hub.handleList)
	mux.HandleFunc("GET /api/articles/{id}", hub.handleGet)
	mux.HandleFunc("GET /ws", hub.handleWS)
	mux.HandleFunc("OPTIONS /api/articles", handleOptions)
	mux.HandleFunc("OPTIONS /api/articles/{id}", handleOptions)
	mux.HandleFunc("OPTIONS /ws", handleOptions)

	log.Printf("listen %s", addr)
	if err := http.ListenAndServe(addr, cors(mux)); err != nil {
		log.Fatal(err)
	}
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
	_ = json.NewDecoder(r.Body).Decode(&body)
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
		http.Error(w, err.Error(), http.StatusNotFound)
		return
	}
	writeJSON(w, http.StatusOK, v)
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}
