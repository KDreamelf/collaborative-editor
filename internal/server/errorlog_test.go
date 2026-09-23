package server

import (
	"bytes"
	"errors"
	"log"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestHTTPGetMissingArticleChinese(t *testing.T) {
	h := NewHub(nil)
	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/articles/{id}", h.handleGet)
	req := httptest.NewRequest(http.MethodGet, "/api/articles/deadbeefdeadbeefdeadbeef", nil)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("应 404，得到 %d %s", rec.Code, rec.Body.String())
	}
	body := strings.TrimSpace(rec.Body.String())
	if body != "文章不存在" {
		t.Fatalf("预期中文，得到 %q", body)
	}
	if strings.Contains(body, "日志编号") {
		t.Fatalf("预期错误不应带日志编号: %q", body)
	}
}

func TestWriteHTTPErrorUnexpectedHidesDetail(t *testing.T) {
	dir := t.TempDir()
	SetErrorLogDir(dir)
	t.Cleanup(func() { SetErrorLogDir("") })

	rec := httptest.NewRecorder()
	writeHTTPError(rec, errors.New("mongo dial tcp 10.0.0.1:27017: connect refused\nsecret"))
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("应 500，得到 %d", rec.Code)
	}
	body := strings.TrimSpace(rec.Body.String())
	if strings.Contains(body, "mongo") || strings.Contains(body, "secret") || strings.Contains(body, "10.0.0.1") {
		t.Fatalf("响应不得暴露原文: %q", body)
	}
	if !strings.HasPrefix(body, "请求失败，日志编号 E") {
		t.Fatalf("应只回编号文案: %q", body)
	}
	id := strings.TrimPrefix(body, "请求失败，日志编号 ")
	data, err := os.ReadFile(filepath.Join(dir, serverLogName))
	if err != nil {
		t.Fatal(err)
	}
	text := string(data)
	if !strings.Contains(text, "["+id+"]") {
		t.Fatalf("日志应按编号可查: id=%s log=%q", id, text)
	}
	if !strings.Contains(text, "mongo dial tcp") {
		t.Fatalf("日志应保留原文: %q", text)
	}
	if strings.Count(text, "\n") != 1 || strings.Contains(text, "\nsecret") {
		t.Fatalf("换行须压成单行: %q", text)
	}
}

func TestDefaultErrorLogDirNotCwd(t *testing.T) {
	dir := defaultErrorLogDir()
	if dir == "" || dir == "." {
		t.Fatalf("默认不得写仓库 cwd: %q", dir)
	}
	if filepath.Base(dir) != serverLogAppDir {
		t.Fatalf("应落在 %s 下: %q", serverLogAppDir, dir)
	}
}

func TestAppendServerErrorLogFallsBackToStdLog(t *testing.T) {
	// 把「目录」指到普通文件上，MkdirAll 必失败。
	bad := filepath.Join(t.TempDir(), "not-a-dir")
	if err := os.WriteFile(bad, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	SetErrorLogDir(bad)
	t.Cleanup(func() { SetErrorLogDir("") })

	var buf bytes.Buffer
	prev := log.Writer()
	log.SetOutput(&buf)
	t.Cleanup(func() { log.SetOutput(prev) })

	id := "E20260324-143052-zzz"
	appendServerErrorLog(id, "boom detail")
	out := buf.String()
	if !strings.Contains(out, "["+id+"]") || !strings.Contains(out, "boom detail") {
		t.Fatalf("写盘失败须 log.Printf 同编号原文: %q", out)
	}
}
