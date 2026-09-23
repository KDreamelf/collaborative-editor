//go:build fixture

package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/KDreamelf/collaborative-editor/internal/document"
	"github.com/KDreamelf/collaborative-editor/internal/model"
)

func TestCases(t *testing.T) {
	paths, err := filepath.Glob(filepath.Join("..", "cases", "*.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	if len(paths) == 0 {
		t.Fatal("cases 里没有用例")
	}
	slices.Sort(paths)
	arc, err := openArchive()
	if err != nil {
		t.Fatal(err)
	}
	defer arc.close()
	t.Log(arc.path())

	failed := 0
	for _, path := range paths {
		name := strings.TrimSuffix(filepath.Base(path), ".jsonl")
		if err := runCase(t, arc, name, path); err != nil {
			failed++
			t.Errorf("%s: %v", name, err)
			arc.rec(map[string]any{"kind": "result", "case": name, "ok": false, "error": err.Error()})
			continue
		}
		arc.rec(map[string]any{"kind": "result", "case": name, "ok": true})
	}
	arc.rec(map[string]any{"kind": "done", "cases": len(paths), "failed": failed})
}

func runCase(t *testing.T, arc *archive, name, path string) error {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	turns := splitTurns(string(raw))
	var in bytes.Buffer
	writeRPC(&in, 1, "initialize", map[string]any{
		"protocolVersion": 2,
		"capabilities":    map[string]any{},
		"info":            map[string]any{"name": "case-runner", "version": "0"},
	})
	writeRPC(&in, 2, "session/new", map[string]any{"cwd": ".", "mcpServers": []any{}, "sessionId": "s1"})
	id := 3
	var prompts []string
	var expects [][]string
	for _, turn := range turns {
		var ops []string
		var exp []string
		for _, line := range strings.Split(turn, "\n") {
			line = strings.TrimSpace(line)
			if line == "" {
				continue
			}
			if isExpect(line) {
				exp = append(exp, line)
			} else {
				ops = append(ops, line)
			}
		}
		expects = append(expects, exp)
		if len(ops) == 0 {
			prompts = append(prompts, "")
			continue
		}
		text := strings.Join(ops, "\n")
		prompts = append(prompts, text)
		writeRPC(&in, id, "session/prompt", map[string]any{
			"sessionId": "s1",
			"prompt":    []any{map[string]any{"type": "text", "text": text}},
		})
		id++
		for _, op := range ops {
			arc.rec(map[string]any{"kind": "op", "case": name, "op": json.RawMessage(op)})
		}
	}

	var out bytes.Buffer
	if err := run(&in, &out); err != nil {
		return err
	}
	views, err := collectViews(out.Bytes())
	if err != nil {
		return err
	}
	for _, v := range views {
		arc.rec(map[string]any{"kind": "view", "case": name, "view": v})
	}

	vi := 0
	for i, exp := range expects {
		if prompts[i] != "" {
			n := strings.Count(prompts[i], "\n") + 1
			if vi+n > len(views) {
				return fmt.Errorf("第 %d 段视图不够: 要 %d 条，剩下 %d", i+1, n, len(views)-vi)
			}
			vi += n
		}
		if len(exp) == 0 {
			continue
		}
		if vi == 0 {
			return fmt.Errorf("第 %d 段还没有视图可核对", i+1)
		}
		view := views[vi-1]
		for _, line := range exp {
			if err := checkExpect(view, line); err != nil {
				arc.rec(map[string]any{"kind": "expect", "case": name, "ok": false, "expect": json.RawMessage(line), "error": err.Error()})
				return fmt.Errorf("第 %d 段: %w", i+1, err)
			}
			arc.rec(map[string]any{"kind": "expect", "case": name, "ok": true, "expect": json.RawMessage(line)})
		}
	}
	return nil
}

func isExpect(line string) bool {
	var m map[string]json.RawMessage
	if json.Unmarshal([]byte(line), &m) != nil {
		return false
	}
	_, ok := m["expect"]
	return ok
}

type expect struct {
	Expect string   `json:"expect"`
	Line   int      `json:"line"`
	Text   string   `json:"text"`
	N      *int     `json:"n"`
	Person string   `json:"person"`
	Action string   `json:"action"`
	Lines  []string `json:"lines"`
	From   int      `json:"from"`
	To     int      `json:"to"`
	Who    string   `json:"who"`
}

func checkExpect(view document.View, raw string) error {
	var e expect
	if err := json.Unmarshal([]byte(raw), &e); err != nil {
		return err
	}
	switch e.Expect {
	case "line":
		if e.Line < 0 || e.Line >= len(view.Lines) {
			return fmt.Errorf("没有第 %d 行", e.Line)
		}
		if view.Lines[e.Line].Content != e.Text {
			return fmt.Errorf("第 %d 行是 %q，要 %q", e.Line, view.Lines[e.Line].Content, e.Text)
		}
	case "lines":
		if e.N == nil || len(view.Lines) != *e.N {
			return fmt.Errorf("正式行 %d 条，要 %d", len(view.Lines), nOr(e.N))
		}
	case "disputes":
		if e.N == nil || len(view.Disputes) != *e.N {
			return fmt.Errorf("争议 %d 份，要 %d", len(view.Disputes), nOr(e.N))
		}
	case "suspended":
		if e.N == nil || len(view.Suspended) != *e.N {
			return fmt.Errorf("挂起 %d 份，要 %d", len(view.Suspended), nOr(e.N))
		}
	case "linked":
		if e.From < 0 || e.To < 0 || e.From >= len(view.Lines) || e.To >= len(view.Lines) {
			return fmt.Errorf("链接下标超出正文")
		}
		if view.Lines[e.From].Next != view.Lines[e.To].ID || view.Lines[e.To].Prev != view.Lines[e.From].ID {
			return fmt.Errorf("第 %d 行的后一行不是第 %d 行", e.From, e.To)
		}
	case "claim":
		d, ok := findClaim(view, e.Person, e.Action)
		if !ok {
			return fmt.Errorf("没有 %s 的主张", e.Person)
		}
		if e.Lines != nil {
			if !slices.Equal(d.Content, e.Lines) {
				return fmt.Errorf("%s 的主张是 %q，要 %q", e.Person, d.Content, e.Lines)
			}
		} else if strings.Join(d.Content, "\n") != e.Text {
			return fmt.Errorf("%s 的主张是 %q，要 %q", e.Person, strings.Join(d.Content, "\n"), e.Text)
		}
	case "followers":
		d, ok := findClaim(view, e.Person, e.Action)
		if !ok {
			return fmt.Errorf("没有 %s 的主张", e.Person)
		}
		if e.N == nil || len(d.Followers) != *e.N {
			return fmt.Errorf("%s 的追随者 %d 人，要 %d", e.Person, len(d.Followers), nOr(e.N))
		}
		if e.Who != "" && !slices.Contains(d.Followers, e.Who) {
			return fmt.Errorf("%s 的追随者里没有 %s", e.Person, e.Who)
		}
	default:
		return fmt.Errorf("未知断言 %s", e.Expect)
	}
	return nil
}

func findClaim(view document.View, person, action string) (model.Dispute, bool) {
	if action == "" {
		action = model.ActionEdit
	}
	for _, d := range view.Disputes {
		if d.Person == person && d.Action == action {
			return d, true
		}
	}
	return model.Dispute{}, false
}

func nOr(n *int) int {
	if n == nil {
		return -1
	}
	return *n
}

func splitTurns(s string) []string {
	parts := strings.Split(strings.ReplaceAll(s, "\r\n", "\n"), "\n\n")
	var out []string
	for _, p := range parts {
		p = strings.TrimSpace(p)
		if p != "" {
			out = append(out, p)
		}
	}
	return out
}

func writeRPC(buf *bytes.Buffer, id int, method string, params any) {
	msg := map[string]any{"jsonrpc": "2.0", "id": id, "method": method, "params": params}
	b, err := json.Marshal(msg)
	if err != nil {
		panic(err)
	}
	buf.Write(b)
	buf.WriteByte('\n')
}

func collectViews(raw []byte) ([]document.View, error) {
	var views []document.View
	for _, line := range bytes.Split(raw, []byte{'\n'}) {
		line = bytes.TrimSpace(line)
		if len(line) == 0 {
			continue
		}
		var msg struct {
			Method string `json:"method"`
			Error  *struct {
				Message string `json:"message"`
			} `json:"error"`
			Params json.RawMessage `json:"params"`
		}
		if err := json.Unmarshal(line, &msg); err != nil {
			return nil, err
		}
		if msg.Error != nil {
			return nil, fmt.Errorf("夹具返回错误: %s", msg.Error.Message)
		}
		if msg.Method != "session/update" {
			continue
		}
		var params struct {
			Update struct {
				Content struct {
					Text string `json:"text"`
				} `json:"content"`
			} `json:"update"`
		}
		if err := json.Unmarshal(msg.Params, &params); err != nil {
			return nil, err
		}
		var view document.View
		if err := json.Unmarshal([]byte(params.Update.Content.Text), &view); err != nil {
			return nil, err
		}
		views = append(views, view)
	}
	return views, nil
}
