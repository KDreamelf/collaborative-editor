//go:build fixture

// 测试夹具。正式编译不加 -tags fixture，桌面端和 cmd/server 都不会编进这个程序。
// 标准输入输出走 ACP（Agent Client Protocol）：每行一条 JSON-RPC。
// session/prompt 里的文本是操作 JSONL，一行一个操作。
package main

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/KDreamelf/collaborative-editor/internal/server"
)

func main() {
	if err := run(os.Stdin, os.Stdout); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

type incoming struct {
	Method string          `json:"method"`
	ID     json.RawMessage `json:"id"`
	Params json.RawMessage `json:"params"`
}

func run(in io.Reader, out io.Writer) error {
	script := server.NewScript()
	sessions := map[string]bool{}
	sc := bufio.NewScanner(in)
	sc.Buffer(make([]byte, 0, 64*1024), 2*1024*1024)
	enc := json.NewEncoder(out)
	enc.SetEscapeHTML(false)
	for sc.Scan() {
		line := bytes.TrimSpace(sc.Bytes())
		if len(line) == 0 {
			continue
		}
		var msg incoming
		if err := json.Unmarshal(line, &msg); err != nil {
			writeMsg(enc, nil, nil, &rpcError{-32700, err.Error()})
			continue
		}
		switch msg.Method {
		case "initialize":
			writeMsg(enc, msg.ID, map[string]any{
				"protocolVersion": 2,
				"capabilities":    map[string]any{"session": map[string]any{}},
				"info": map[string]any{
					"name":    "collab-fixture",
					"title":   "协同编辑器测试夹具",
					"version": "0",
				},
				"authMethods": []any{},
			}, nil)
		case "session/new":
			var p struct {
				SessionID string `json:"sessionId"`
			}
			_ = json.Unmarshal(msg.Params, &p)
			if p.SessionID == "" {
				p.SessionID = "s1"
			}
			sessions[p.SessionID] = true
			writeMsg(enc, msg.ID, map[string]any{"sessionId": p.SessionID}, nil)
		case "session/cancel":
			writeMsg(enc, msg.ID, map[string]any{}, nil)
		case "session/prompt":
			if err := prompt(script, sessions, enc, msg); err != nil {
				return err
			}
		default:
			writeMsg(enc, msg.ID, nil, &rpcError{-32601, "方法不存在"})
		}
	}
	return sc.Err()
}

type rpcError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

func prompt(script *server.Script, sessions map[string]bool, enc *json.Encoder, msg incoming) error {
	var p struct {
		SessionID string `json:"sessionId"`
		Prompt    []struct {
			Type string `json:"type"`
			Text string `json:"text"`
		} `json:"prompt"`
	}
	if err := json.Unmarshal(msg.Params, &p); err != nil {
		writeMsg(enc, msg.ID, nil, &rpcError{-32602, err.Error()})
		return nil
	}
	if !sessions[p.SessionID] {
		writeMsg(enc, msg.ID, nil, &rpcError{-32602, "没有这场会话"})
		return nil
	}
	var text strings.Builder
	for _, block := range p.Prompt {
		if block.Type == "" || block.Type == "text" {
			text.WriteString(block.Text)
			text.WriteByte('\n')
		}
	}
	for _, line := range strings.Split(text.String(), "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		view, err := script.Exec([]byte(line))
		if err != nil {
			writeMsg(enc, msg.ID, nil, &rpcError{-32603, err.Error()})
			return nil
		}
		body, err := json.Marshal(view)
		if err != nil {
			return err
		}
		if err := enc.Encode(map[string]any{
			"jsonrpc": "2.0",
			"method":  "session/update",
			"params": map[string]any{
				"sessionId": p.SessionID,
				"update": map[string]any{
					"sessionUpdate": "agent_message_chunk",
					"content":       map[string]any{"type": "text", "text": string(body)},
				},
			},
		}); err != nil {
			return err
		}
	}
	writeMsg(enc, msg.ID, map[string]any{"stopReason": "end_turn"}, nil)
	return nil
}

func writeMsg(enc *json.Encoder, id json.RawMessage, result any, err *rpcError) {
	msg := map[string]any{"jsonrpc": "2.0"}
	if len(id) > 0 && string(id) != "null" {
		msg["id"] = id
	}
	if err != nil {
		msg["error"] = err
	} else {
		msg["result"] = result
	}
	_ = enc.Encode(msg)
}
