//go:build fixture

package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"time"
)

type archive struct {
	f   *os.File
	enc *json.Encoder
}

func openArchive() (*archive, error) {
	dir := "archive"
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, err
	}
	name := time.Now().Format("2006-01-02T15-04-05.000") + ".jsonl"
	f, err := os.Create(filepath.Join(dir, name))
	if err != nil {
		return nil, err
	}
	enc := json.NewEncoder(f)
	enc.SetEscapeHTML(false)
	a := &archive{f: f, enc: enc}
	a.rec(map[string]any{"kind": "run"})
	return a, nil
}

func (a *archive) rec(fields map[string]any) {
	if _, ok := fields["ts"]; !ok {
		fields["ts"] = time.Now().Format(time.RFC3339Nano)
	}
	_ = a.enc.Encode(fields)
	_ = a.f.Sync()
}

func (a *archive) close() {
	_ = a.f.Close()
}

func (a *archive) path() string {
	return a.f.Name()
}
