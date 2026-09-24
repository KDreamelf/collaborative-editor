package server

import (
	"strings"
	"testing"
	"time"
)

func TestOpenHubMemoryAndMongoFail(t *testing.T) {
	h, err := openHub("", "")
	if err != nil || h == nil {
		t.Fatalf("纯内存: hub=%v err=%v", h, err)
	}
	if h.mongo != nil {
		t.Fatal("空 MONGO_URI 应为纯内存")
	}

	const badURI = "mongodb://user:secret@127.0.0.1:1"
	done := make(chan error, 1)
	go func() {
		_, err := openHub(badURI, "editor")
		done <- err
	}()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("坏 MONGO_URI 应失败，不得降级内存")
		}
		if strings.Contains(err.Error(), badURI) || strings.Contains(err.Error(), "secret") {
			t.Fatalf("错误不得含 URI/密码: %v", err)
		}
	case <-time.After(15 * time.Second):
		t.Fatal("openHub 失败超时")
	}
}

func TestOpenHubDefaultDBName(t *testing.T) {
	_, err := openHub("mongodb://127.0.0.1:1", "")
	if err == nil {
		t.Fatal("默认 db=editor 仍应因 Ping 失败退出")
	}
}
