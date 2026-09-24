// 专属编辑客户端，走 ACP（Agent Client Protocol，智能体客户端协议）的 stdio。
// 一行一个 JSON-RPC。stdout 只出协议，日志在 stderr。
//
// session/prompt 的文本是一条命令：
//
//	join <http-base> <articleId> <personId>
//	edit <行号> <正文>
//	say <正文>
//	accept <行号> <人>
//	reject <行号> <人>
//	view
package main

import (
	"bufio"
	"encoding/json"
	"fmt"
	"log"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/KDreamelf/collaborative-editor/internal/model"
	"github.com/KDreamelf/collaborative-editor/internal/protocol"
	"github.com/gorilla/websocket"
)

func main() {
	log.SetOutput(os.Stderr)
	log.SetFlags(0)
	in := bufio.NewReader(os.Stdin)
	out := bufio.NewWriter(os.Stdout)
	var ed *editor
	var sessionID string
	for {
		line, err := in.ReadBytes('\n')
		if err != nil {
			return
		}
		line = []byte(strings.TrimSpace(string(line)))
		if len(line) == 0 {
			continue
		}
		var msg struct {
			ID     json.RawMessage `json:"id"`
			Method string          `json:"method"`
			Params json.RawMessage `json:"params"`
		}
		if err := json.Unmarshal(line, &msg); err != nil || len(msg.ID) == 0 {
			continue
		}
		switch msg.Method {
		case "initialize":
			writeResult(out, msg.ID, map[string]any{"protocolVersion": "1"})
		case "session/new":
			sessionID = model.NewID().Hex()
			ed = newEditor()
			writeResult(out, msg.ID, map[string]any{"sessionId": sessionID})
		case "session/prompt":
			if ed == nil {
				writeErr(out, msg.ID, "没有会话")
				continue
			}
			reply, err := ed.command(promptText(msg.Params))
			if err != nil {
				reply = "错误 " + err.Error()
			}
			note(out, sessionID, reply)
			writeResult(out, msg.ID, map[string]any{"stopReason": "end_turn"})
		default:
			writeErr(out, msg.ID, "不支持 "+msg.Method)
		}
	}
}

func promptText(params json.RawMessage) string {
	var body struct {
		Prompt []struct {
			Type string `json:"type"`
			Text string `json:"text"`
		} `json:"prompt"`
	}
	if json.Unmarshal(params, &body) != nil {
		return ""
	}
	var b strings.Builder
	for _, p := range body.Prompt {
		if p.Text != "" {
			b.WriteString(p.Text)
		}
	}
	return strings.TrimSpace(b.String())
}

func writeResult(w *bufio.Writer, id json.RawMessage, result any) {
	raw, _ := json.Marshal(result)
	fmt.Fprintf(w, `{"jsonrpc":"2.0","id":%s,"result":%s}`+"\n", id, raw)
	_ = w.Flush()
}

func writeErr(w *bufio.Writer, id json.RawMessage, message string) {
	raw, _ := json.Marshal(message)
	fmt.Fprintf(w, `{"jsonrpc":"2.0","id":%s,"error":{"code":-32000,"message":%s}}`+"\n", id, raw)
	_ = w.Flush()
}

func note(w *bufio.Writer, sessionID, text string) {
	payload := map[string]any{
		"sessionId": sessionID,
		"update": map[string]any{
			"sessionUpdate": "agent_message_chunk",
			"content":       map[string]any{"type": "text", "text": text},
		},
	}
	raw, _ := json.Marshal(payload)
	fmt.Fprintf(w, `{"jsonrpc":"2.0","method":"session/update","params":%s}`+"\n", raw)
	_ = w.Flush()
}

type editor struct {
	conn      *websocket.Conn
	incoming  chan []byte
	person    string
	order     []string
	formal    map[string]string
	local     map[string]string
	attending map[string]bool
	ownID     map[string]model.ID
	claims    map[string]model.Dispute // line\x00person → 他人主张
	sentBody  map[string]string        // line\x00target → 已抄送正文
	decision  map[string]string        // line\x00person → accept|reject
	sentCC    int
	seq       int64
	waitSeq   int64
	ackOK     bool
	ackErr    string
}

func newEditor() *editor {
	return &editor{
		formal:    map[string]string{},
		local:     map[string]string{},
		attending: map[string]bool{},
		ownID:     map[string]model.ID{},
		claims:    map[string]model.Dispute{},
		sentBody:  map[string]string{},
		decision:  map[string]string{},
	}
}

func (e *editor) command(line string) (string, error) {
	fields := strings.Fields(line)
	if len(fields) == 0 {
		return "", fmt.Errorf("空命令")
	}
	switch fields[0] {
	case "join":
		return e.join(fields[1:])
	case "say":
		text := strings.TrimSpace(strings.TrimPrefix(line, "say"))
		if text == "" {
			return "", fmt.Errorf("say <正文>")
		}
		if err := e.say(text); err != nil {
			return "", err
		}
		e.drain(350 * time.Millisecond)
		return e.view(), nil
	case "edit":
		if len(fields) < 3 {
			return "", fmt.Errorf("edit <行号> <正文>")
		}
		n, err := strconv.Atoi(fields[1])
		if err != nil {
			return "", fmt.Errorf("edit <行号> <正文>")
		}
		text := strings.TrimSpace(strings.TrimPrefix(strings.TrimPrefix(line, "edit"), " "+fields[1]))
		if err := e.edit(n, text); err != nil {
			return "", err
		}
		e.drain(350 * time.Millisecond)
		return e.view(), nil
	case "accept", "reject":
		if len(fields) != 3 {
			return "", fmt.Errorf("%s <行号> <人>", fields[0])
		}
		n, err := strconv.Atoi(fields[1])
		if err != nil {
			return "", err
		}
		if fields[0] == "accept" {
			err = e.accept(n, fields[2])
		} else {
			err = e.reject(n, fields[2])
		}
		if err != nil {
			return "", err
		}
		e.drain(350 * time.Millisecond)
		return e.view(), nil
	case "view":
		e.drain(350 * time.Millisecond)
		return e.view(), nil
	default:
		return "", fmt.Errorf("未知命令")
	}
}

func (e *editor) join(args []string) (string, error) {
	if len(args) != 3 {
		return "", fmt.Errorf("join <http> <articleId> <personId>")
	}
	if e.conn != nil {
		return "", fmt.Errorf("已经加入")
	}
	base, article, person := args[0], args[1], args[2]
	u, err := url.Parse(base)
	if err != nil {
		return "", err
	}
	if u.Scheme == "https" {
		u.Scheme = "wss"
	} else {
		u.Scheme = "ws"
	}
	u.Path = "/ws"
	conn, _, err := websocket.DefaultDialer.Dial(u.String(), nil)
	if err != nil {
		return "", err
	}
	e.conn = conn
	e.person = person
	e.incoming = make(chan []byte, 256)
	go func() {
		for {
			_, data, err := conn.ReadMessage()
			if err != nil {
				return
			}
			e.incoming <- data
		}
	}()
	if err := e.writeJSON(protocol.Join{
		Type: protocol.TypeJoin, ArticleID: article, PersonID: person, Name: person,
	}); err != nil {
		return "", err
	}
	if err := e.until(2*time.Second, func() bool { return len(e.order) > 0 }); err != nil {
		return "", fmt.Errorf("没有入场")
	}
	return e.view(), nil
}

func (e *editor) edit(n int, text string) error {
	id, err := e.lineID(n)
	if err != nil {
		return err
	}
	e.local[id] = text
	e.attending[id] = true
	if err := e.sendOp(protocol.Op{
		ID:   model.NewID().Hex(),
		Kind: protocol.TypeSubmit,
		Submit: &protocol.Submit{
			Type: protocol.TypeSubmit, PersonID: e.person, LineID: id,
			Action: model.ActionEdit, Content: []string{text},
		},
	}); err != nil {
		return err
	}
	e.formal[id] = text
	return nil
}

func (e *editor) say(text string) error {
	if len(e.order) == 0 {
		return fmt.Errorf("尚未加入")
	}
	anchor := e.order[len(e.order)-1]
	nid := model.NewID()
	if err := e.sendOp(protocol.Op{
		ID:   model.NewID().Hex(),
		Kind: protocol.TypeSubmit,
		Submit: &protocol.Submit{
			Type: protocol.TypeSubmit, PersonID: e.person, LineID: anchor,
			Action: model.ActionInsert, Content: []string{text},
			LineIDs: []string{nid.Hex()},
		},
	}); err != nil {
		return err
	}
	e.insertAfter(anchor, nid.Hex(), text)
	return nil
}

func (e *editor) insertAfter(anchor, newID, text string) {
	for _, id := range e.order {
		if id == newID {
			e.formal[newID] = text
			if _, ok := e.local[newID]; !ok {
				e.local[newID] = text
			}
			return
		}
	}
	at := len(e.order)
	for i, id := range e.order {
		if id == anchor {
			at = i + 1
			break
		}
	}
	e.order = append(e.order, "")
	copy(e.order[at+1:], e.order[at:])
	e.order[at] = newID
	e.formal[newID] = text
	e.local[newID] = text
}

func (e *editor) accept(n int, person string) error {
	id, err := e.lineID(n)
	if err != nil {
		return err
	}
	claim, ok := e.claims[id+"\x00"+person]
	if !ok {
		return fmt.Errorf("没有 %s 在第 %d 行的主张", person, n)
	}
	body := ""
	if len(claim.Content) > 0 {
		body = claim.Content[0]
	}
	if err := e.sendOp(protocol.Op{
		ID:   model.NewID().Hex(),
		Kind: protocol.TypeFollow,
		Follow: &protocol.Follow{
			Type: protocol.TypeFollow, PersonID: e.person,
			DisputeID: claim.ID.Hex(), ClientTs: time.Now().UnixMilli(),
		},
	}); err != nil {
		return err
	}
	e.local[id] = body
	e.attending[id] = false
	e.decision[id+"\x00"+person] = "accept"
	return nil
}

func (e *editor) reject(n int, person string) error {
	id, err := e.lineID(n)
	if err != nil {
		return err
	}
	if _, ok := e.claims[id+"\x00"+person]; !ok {
		return fmt.Errorf("没有 %s 在第 %d 行的主张", person, n)
	}
	e.decision[id+"\x00"+person] = "reject"
	e.attending[id] = true
	if e.local[id] != "" {
		_ = e.emitCC(id, person)
	}
	return nil
}

func (e *editor) lineID(n int) (string, error) {
	if n < 1 || n > len(e.order) {
		return "", fmt.Errorf("没有第 %d 行", n)
	}
	return e.order[n-1], nil
}

func (e *editor) sendOp(op protocol.Op) error {
	e.seq++
	e.waitSeq = e.seq
	e.ackOK = false
	e.ackErr = ""
	if err := e.writeJSON(protocol.Batch{Type: protocol.TypeBatch, Seq: e.seq, Ops: []protocol.Op{op}}); err != nil {
		return err
	}
	if err := e.until(3*time.Second, func() bool { return e.ackOK || e.ackErr != "" }); err != nil {
		return err
	}
	if e.ackErr != "" {
		return fmt.Errorf("%s", e.ackErr)
	}
	return nil
}

func (e *editor) until(limit time.Duration, done func() bool) error {
	deadline := time.Now().Add(limit)
	quiet := 80 * time.Millisecond
	timer := time.NewTimer(quiet)
	defer timer.Stop()
	for {
		if done() {
			return nil
		}
		select {
		case data, ok := <-e.incoming:
			if !ok {
				return fmt.Errorf("连接断开")
			}
			e.handle(data)
			if done() {
				return nil
			}
			if !timer.Stop() {
				select {
				case <-timer.C:
				default:
				}
			}
			timer.Reset(quiet)
		case <-timer.C:
			if done() || time.Now().After(deadline) {
				if done() {
					return nil
				}
				return fmt.Errorf("等待超时")
			}
			timer.Reset(quiet)
		}
	}
}

func (e *editor) drain(quiet time.Duration) {
	timer := time.NewTimer(quiet)
	defer timer.Stop()
	for {
		select {
		case data := <-e.incoming:
			e.handle(data)
			if !timer.Stop() {
				select {
				case <-timer.C:
				default:
				}
			}
			timer.Reset(quiet)
		case <-timer.C:
			return
		}
	}
}

func (e *editor) handle(data []byte) {
	var head struct {
		Type string `json:"type"`
	}
	if json.Unmarshal(data, &head) != nil {
		return
	}
	switch head.Type {
	case protocol.TypeBootstrap:
		var boot protocol.Bootstrap
		if json.Unmarshal(data, &boot) != nil {
			return
		}
		e.setLines(boot.Base.Lines)
		e.replaceClaims(boot.Disputes)
	case protocol.TypeSnapshot:
		var snap protocol.Snapshot
		if json.Unmarshal(data, &snap) != nil {
			return
		}
		if len(snap.Lines) > 0 {
			e.setLines(snap.Lines)
		}
	case protocol.TypeDisputeRecord:
		var rec protocol.DisputeRecord
		if json.Unmarshal(data, &rec) != nil {
			return
		}
		e.replaceClaims(rec.Disputes)
	case protocol.TypeRelay:
		var ev protocol.RelayEvent
		if json.Unmarshal(data, &ev) != nil {
			return
		}
		e.onRelay(ev.Op)
	case protocol.TypeAck:
		var ack protocol.Ack
		if json.Unmarshal(data, &ack) != nil || ack.Seq != e.waitSeq {
			return
		}
		if ack.Applied < 1 {
			msg := ack.Message
			if msg == "" {
				msg = "未应用"
			}
			e.ackErr = msg
			return
		}
		e.ackOK = true
	}
}

func (e *editor) setLines(lines []model.Line) {
	e.order = e.order[:0]
	for _, ln := range lines {
		id := ln.ID.Hex()
		e.order = append(e.order, id)
		e.formal[id] = ln.Content
		if _, ok := e.local[id]; !ok {
			e.local[id] = ln.Content
		}
	}
}

func (e *editor) replaceClaims(list []model.Dispute) {
	next := map[string]model.Dispute{}
	for _, d := range list {
		if d.ID.IsZero() || d.Person == "" {
			continue
		}
		if d.Person == e.person {
			e.ownID[d.RealLine.Hex()] = d.ID
			continue
		}
		next[d.RealLine.Hex()+"\x00"+d.Person] = d
	}
	e.claims = next
}

func (e *editor) onRelay(op protocol.Op) {
	switch {
	case op.Kind == protocol.TypeSubmit && op.Submit != nil && model.IsInsertAction(op.Submit.Action):
		sub := op.Submit
		if sub.PersonID == e.person || len(sub.Content) == 0 || len(sub.LineIDs) == 0 {
			return
		}
		e.insertAfter(sub.LineID, sub.LineIDs[0], sub.Content[0])
	case op.Kind == protocol.TypeSubmit && op.Submit != nil && op.Submit.Action == model.ActionEdit:
		sub := op.Submit
		if sub.PersonID == e.person || len(sub.Content) == 0 {
			return
		}
		if e.attending[sub.LineID] && sub.Content[0] != e.local[sub.LineID] {
			_ = e.emitCC(sub.LineID, sub.PersonID)
			return
		}
		if !e.attending[sub.LineID] {
			e.local[sub.LineID] = sub.Content[0]
			e.formal[sub.LineID] = sub.Content[0]
		}
	case op.Kind == protocol.TypeDisputeCC && op.DisputeCC != nil:
		cc := op.DisputeCC
		claim := cc.Claim
		if claim.Person == e.person {
			if !claim.ID.IsZero() {
				e.ownID[claim.RealLine.Hex()] = claim.ID
			}
			return
		}
		if cc.TargetPersonID != e.person {
			return
		}
		e.claims[claim.RealLine.Hex()+"\x00"+claim.Person] = claim
	case op.Kind == protocol.TypeFollow && op.Follow != nil:
		f := op.Follow
		if f.PersonID == e.person {
			return
		}
		for _, id := range e.ownID {
			if id.Hex() != f.DisputeID {
				continue
			}
			e.replyFollow(f.PersonID, f.DisputeID, true)
			return
		}
	}
}

func (e *editor) replyFollow(fromID, disputeID string, accept bool) {
	e.seq++
	_ = e.writeJSON(protocol.Batch{
		Type: protocol.TypeBatch,
		Seq:  e.seq,
		Ops: []protocol.Op{{
			ID:   model.NewID().Hex(),
			Kind: protocol.TypeFollowAnswer,
			FollowAnswer: &protocol.FollowAnswer{
				Type: protocol.TypeFollowAnswer, PersonID: e.person,
				FromID: fromID, DisputeID: disputeID, Accept: accept,
			},
		}},
	})
}

func (e *editor) emitCC(lineID, target string) error {
	if target == "" || target == e.person {
		return nil
	}
	body := e.local[lineID]
	key := lineID + "\x00" + target
	if e.sentBody[key] == body {
		return nil
	}
	id := e.ownID[lineID]
	if id.IsZero() {
		id = model.NewID()
		e.ownID[lineID] = id
	}
	line, err := model.ParseID(lineID)
	if err != nil {
		return err
	}
	e.sentBody[key] = body
	e.sentCC++
	e.seq++
	return e.writeJSON(protocol.Batch{
		Type: protocol.TypeBatch,
		Seq:  e.seq,
		Ops: []protocol.Op{{
			ID:   model.NewID().Hex(),
			Kind: protocol.TypeDisputeCC,
			DisputeCC: &protocol.DisputeCC{
				TargetPersonID: target,
				Claim: model.Dispute{
					ID: id, RealLine: line, Action: model.ActionEdit,
					Person: e.person, Content: []string{body}, Followers: []string{},
				},
			},
		}},
	})
}

func (e *editor) view() string {
	var b strings.Builder
	fmt.Fprintf(&b, "person=%s lines=%d sentCC=%d\n", e.person, len(e.order), e.sentCC)
	for i, id := range e.order {
		fmt.Fprintf(&b, "L%d id=%s local=%s formal=%s attending=%t\n", i+1, id, e.local[id], e.formal[id], e.attending[id])
		for _, claim := range e.claims {
			if claim.RealLine.Hex() != id {
				continue
			}
			body := ""
			if len(claim.Content) > 0 {
				body = claim.Content[0]
			}
			mark := e.decision[id+"\x00"+claim.Person]
			fmt.Fprintf(&b, "  claim %s %s decision=%s\n", claim.Person, body, mark)
		}
	}
	return strings.TrimRight(b.String(), "\n")
}

func (e *editor) writeJSON(v any) error {
	return e.conn.WriteJSON(v)
}
