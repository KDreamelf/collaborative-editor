package server

import (
	"crypto/rand"
	"fmt"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

const serverLogName = "server.log"
const serverLogAppDir = "collaborative-editor"

var (
	errorLogMu   sync.Mutex
	errorLogDir  string // 空则用用户缓存/配置目录；测试注入临时目录
	errorLogName = serverLogName
)

// SetErrorLogDir 测试用：错误日志写到指定目录。
func SetErrorLogDir(dir string) {
	errorLogMu.Lock()
	defer errorLogMu.Unlock()
	errorLogDir = dir
}

func defaultErrorLogDir() string {
	if base, err := os.UserCacheDir(); err == nil && base != "" {
		return filepath.Join(base, serverLogAppDir)
	}
	if base, err := os.UserConfigDir(); err == nil && base != "" {
		return filepath.Join(base, serverLogAppDir)
	}
	return filepath.Join(os.TempDir(), serverLogAppDir)
}

func newServerLogID() string {
	now := time.Now()
	stamp := now.Format("20060102-150405")
	var b [2]byte
	_, _ = rand.Read(b[:])
	v := int(b[0])<<8 | int(b[1])
	const digits = "0123456789abcdefghijklmnopqrstuvwxyz"
	r := [3]byte{}
	for i := 2; i >= 0; i-- {
		r[i] = digits[v%36]
		v /= 36
	}
	return fmt.Sprintf("E%s-%s", stamp, string(r[:]))
}

func sanitizeLogLine(s string) string {
	s = strings.ReplaceAll(s, "\r", " ")
	s = strings.ReplaceAll(s, "\n", " ")
	return s
}

func appendServerErrorLog(id, message string) {
	message = sanitizeLogLine(message)
	errorLogMu.Lock()
	dir := errorLogDir
	name := errorLogName
	errorLogMu.Unlock()
	if dir == "" {
		dir = defaultErrorLogDir()
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		log.Printf("[%s] %s (mkdir log: %v)", id, message, err)
		return
	}
	path := filepath.Join(dir, name)
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		log.Printf("[%s] %s (open log: %v)", id, message, err)
		return
	}
	defer f.Close()
	if _, err := fmt.Fprintf(f, "[%s] %s\n", id, message); err != nil {
		log.Printf("[%s] %s (write log: %v)", id, message, err)
	}
}

// writeHTTPError 预期错误（如文章不存在）原样中文；其余记编号日志，只回「请求失败，日志编号 E…」。
func writeHTTPError(w http.ResponseWriter, err error) {
	if err == nil {
		return
	}
	if err == errNotFound || err.Error() == "文章不存在" {
		http.Error(w, "文章不存在", http.StatusNotFound)
		return
	}
	id := newServerLogID()
	appendServerErrorLog(id, err.Error())
	http.Error(w, "请求失败，日志编号 "+id, http.StatusInternalServerError)
}
