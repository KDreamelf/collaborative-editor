//go:build fixture

package main

import (
	"net/http/httptest"
	"testing"
	"time"

	"github.com/KDreamelf/collaborative-editor/internal/protocol"
	"github.com/KDreamelf/collaborative-editor/internal/server"
	"github.com/gorilla/websocket"
)

// 三个夹具就是三个专属客户端，连正式 Hub。
// 甲乙同行走主张包；丙只旁观落盘记录，不回主张。
func TestFixtureClientsDisputeTopology(t *testing.T) {
	srv := httptest.NewServer(server.NewHub(nil).Handler())
	t.Cleanup(srv.Close)

	articleID, err := createArticle(srv.URL, "夹具拓扑")
	if err != nil {
		t.Fatal(err)
	}

	jia := startFixture(t, srv.URL, articleID, "甲", "")
	t.Cleanup(jia.stop)
	line, err := jia.call(func(p *peer) (string, error) { return p.yourLine, nil })
	if err != nil || line == "" {
		t.Fatalf("甲没有分到行: %q %v", line, err)
	}

	yi := startFixture(t, srv.URL, articleID, "乙", line)
	t.Cleanup(yi.stop)
	if _, err := yi.call(func(p *peer) (string, error) {
		if p.yourLine != line {
			return "", errString("乙没有站到甲的行")
		}
		return "", p.sendEdit("乙文")
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := jia.call(func(p *peer) (string, error) {
		return "", p.sendEdit("甲文")
	}); err != nil {
		t.Fatal(err)
	}

	waitFixture(t, func() bool {
		jf, _ := jia.call(func(p *peer) (string, error) {
			if len(p.foreign) == 0 {
				return "", errString("甲未进争议")
			}
			return "1", nil
		})
		yf, _ := yi.call(func(p *peer) (string, error) {
			if len(p.foreign) == 0 {
				return "", errString("乙未进争议")
			}
			return "1", nil
		})
		return jf == "1" && yf == "1"
	})

	bing := startFixture(t, srv.URL, articleID, "丙", "")
	t.Cleanup(bing.stop)
	if _, err := bing.call(func(p *peer) (string, error) {
		if len(p.foreign) != 0 {
			return "", errString("旁观者进了争议")
		}
		if len(p.observed) == 0 {
			return "", errString("旁观者没看到落盘主张")
		}
		for _, op := range p.outQ {
			if op.Kind == protocol.TypeDisputeCC {
				return "", errString("旁观者回了主张包")
			}
		}
		return "", nil
	}); err != nil {
		t.Fatal(err)
	}
}

type errString string

func (e errString) Error() string { return string(e) }

type fixtureReq struct {
	fn   func(*peer) error
	done chan error
}

type fixtureClient struct {
	p    *peer
	req  chan fixtureReq
	done chan struct{}
}

func startFixture(t *testing.T, base, articleID, person, focus string) *fixtureClient {
	t.Helper()
	p := freshPeer(person)
	p.base = base
	p.articleID = articleID
	p.focusLine = focus
	p.rng = randSrc(1)
	fc := &fixtureClient{p: p, req: make(chan fixtureReq), done: make(chan struct{})}
	go fc.loop()
	deadline := time.Now().Add(3 * time.Second)
	for {
		_, err := fc.call(func(p *peer) (string, error) {
			if !p.booted {
				return "", errString("尚未入场")
			}
			return "", nil
		})
		if err == nil {
			return fc
		}
		if time.Now().After(deadline) {
			t.Fatalf("%s 入场超时: %v", person, err)
		}
		time.Sleep(15 * time.Millisecond)
	}
}

func (f *fixtureClient) stop() { close(f.done) }

func (f *fixtureClient) call(fn func(*peer) (string, error)) (string, error) {
	out := ""
	done := make(chan error, 1)
	select {
	case f.req <- fixtureReq{fn: func(p *peer) error {
		s, err := fn(p)
		out = s
		return err
	}, done: done}:
	case <-time.After(3 * time.Second):
		return "", errString("夹具无响应")
	}
	select {
	case err := <-done:
		return out, err
	case <-time.After(3 * time.Second):
		return "", errString("夹具动作超时")
	}
}

func (f *fixtureClient) loop() {
	wsURL, err := toWS(f.p.base)
	if err != nil {
		return
	}
	conn, _, err := websocket.DefaultDialer.Dial(wsURL, nil)
	if err != nil {
		return
	}
	defer conn.Close()
	f.p.conn = conn
	if err := f.p.writeJSON(protocol.Join{
		Type: protocol.TypeJoin, ArticleID: f.p.articleID, PersonID: f.p.personID, Name: displayName,
	}); err != nil {
		return
	}
	incoming := make(chan []byte, 64)
	go func() {
		for {
			_, data, err := conn.ReadMessage()
			if err != nil {
				close(incoming)
				return
			}
			incoming <- data
		}
	}()
	var disputeC <-chan time.Time
	pump := func() error {
		timer := time.NewTimer(150 * time.Millisecond)
		defer timer.Stop()
		for {
			select {
			case data, ok := <-incoming:
				if !ok {
					return errString("连接断开")
				}
				if err := f.p.onMsg(data, &disputeC); err != nil {
					return err
				}
				if err := f.p.flushOut(); err != nil {
					return err
				}
				if !timer.Stop() {
					select {
					case <-timer.C:
					default:
					}
				}
				timer.Reset(150 * time.Millisecond)
			case <-timer.C:
				return nil
			case <-f.done:
				return nil
			}
		}
	}
	if err := pump(); err != nil {
		return
	}
	for {
		select {
		case <-f.done:
			return
		case req := <-f.req:
			err := pump()
			if err == nil {
				err = req.fn(f.p)
			}
			if err == nil {
				err = f.p.flushOut()
			}
			if err == nil {
				err = pump()
			}
			req.done <- err
		}
	}
}

func waitFixture(t *testing.T, ready func() bool) {
	t.Helper()
	deadline := time.Now().Add(4 * time.Second)
	for time.Now().Before(deadline) {
		if ready() {
			return
		}
		time.Sleep(30 * time.Millisecond)
	}
	t.Fatal("甲乙未互相进入争议")
}
