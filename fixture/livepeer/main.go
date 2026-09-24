//go:build fixture

// 正式中立入场夹具：Bootstrap(当前行链+主张)/Relay + 单切面争议。
// Edit/CC/Follow/FollowAnswer 同一串行 outQ；ACK Applied 才出队；断线原 Op.ID 重发。
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"log"
	"math/rand"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"strings"
	"time"
	"unicode/utf16"

	"github.com/KDreamelf/collaborative-editor/internal/model"
	"github.com/KDreamelf/collaborative-editor/internal/protocol"
	"github.com/gorilla/websocket"
)

const (
	displayName, articleTitle = "测试脚本", "测试"
	writeEvery, disputeWait   = 5 * time.Second, 10 * time.Second
	lineMaxRunes              = 100
	reconnectWait             = 2 * time.Second
)

var phrases = []string{"春风拂面", "远山如黛", "灯火可亲", "细雨敲窗", "长路未央", "杯茶温热", "书页翻动", "城南旧事", "晚霞满天", "清泉石上"}

func main() {
	log.SetFlags(log.LstdFlags | log.Lmsgprefix)
	log.SetPrefix("livepeer ")
	server := flag.String("server", "http://127.0.0.1:8787", "本机协同服务")
	article := flag.String("article", "", "已有文章 id")
	person := flag.String("person", "", "复用 personId")
	line := flag.String("line", "", "联测时把注意力移到已有行 id")
	flag.Parse()
	if err := run(*server, *article, *person, *line); err != nil {
		log.Println("退出:", err)
		os.Exit(1)
	}
}

func run(serverURL, articleID, personID, focusLine string) error {
	base, err := normalizeLocalServer(serverURL)
	if err != nil {
		return err
	}
	articleID, personID = strings.TrimSpace(articleID), strings.TrimSpace(personID)
	focusLine = strings.TrimSpace(focusLine)
	if focusLine != "" {
		id, err := model.ParseID(focusLine)
		if err != nil || id.IsZero() {
			return fmt.Errorf("--line 需要已有行 ID")
		}
	}
	if articleID != "" && personID == "" {
		return fmt.Errorf("--article 须同时给 --person")
	}
	if articleID == "" {
		if articleID, err = createArticle(base, articleTitle); err != nil {
			return err
		}
		if personID == "" {
			personID = model.NewID().Hex()
		}
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()
	p := &peer{
		base:        base,
		articleID:   articleID,
		personID:    personID,
		focusLine:   focusLine,
		rng:         rand.New(rand.NewSource(time.Now().UnixNano())),
		formal:      map[string]string{},
		seen:        map[string]bool{},
		foreign:     map[string]model.Dispute{},
		handled:     map[string]bool{},
		answered:    map[string]bool{},
		answerOpID:  map[string]string{},
		confirmedCC: map[string]string{},
	}
	log.Printf("文章 %s  服务 %s  人 %s(%s)  ← 请用桌面客户端进入测试文档", p.articleID, p.base, displayName, p.personID)
	for {
		if err := p.session(ctx); err != nil {
			if ctx.Err() != nil {
				return nil
			}
			log.Printf("会话中断: %v；%s 后重连", err, reconnectWait)
		}
		select {
		case <-ctx.Done():
			return nil
		case <-time.After(reconnectWait):
		}
	}
}

type peer struct {
	base, articleID, personID string
	focusLine                 string
	conn                      *websocket.Conn
	rng                       *rand.Rand

	yourLine string
	ownText  string
	formal   map[string]string
	claimID  model.ID

	seen        map[string]bool
	foreign     map[string]model.Dispute // claimID hex → 他人主张
	handled     map[string]bool          // 争议决策 key（每场一次）
	answered    map[string]bool          // followKey → ACK 后才 true
	answerOpID  map[string]string        // followKey → 稳定 Answer Op.ID
	confirmedCC map[string]string        // targetPerson → 已 ACK 的本人 CC 正文

	outQ        []protocol.Op // 串行待发；下标 0 为在途或下一发
	seq         int64
	inflightSeq int64 // 当前已写出、等 ACK 的 Batch.Seq；0=无在途发送

	seeded       bool
	booted       bool   // 已吃过至少一次 Bootstrap；重连保留主观态
	attending    bool   // 本端已主动编辑；Bootstrap/断线/追随成功后为假
	followTarget string // FollowAnswer 采纳后暂时跟随此人；下次 sendEdit 清空
	lastCursor   string
	awaitFollow  bool // 已选 RequestFollow，等对方 FollowAnswer；其间不写

	disputeTimer *time.Timer
	disputeKey   string
}

func (p *peer) session(ctx context.Context) error {
	wsURL, err := toWS(p.base)
	if err != nil {
		return err
	}
	conn, _, err := websocket.DefaultDialer.DialContext(ctx, wsURL, nil)
	if err != nil {
		return fmt.Errorf("拨号失败: %w", err)
	}
	p.conn = conn
	defer func() {
		_ = conn.Close()
		p.conn = nil
		p.inflightSeq = 0   // 队列保留，重连用原 Op.ID 重发队头
		p.attending = false // 断线注意力消失
	}()

	// 重连保留 ownText/outQ/claimID/follow 等主观态；Bootstrap 再按 Base/Disputes 对齐。
	p.attending = false
	p.lastCursor = ""
	p.stopDisputeTimer()

	if err := p.writeJSON(protocol.Join{
		Type: protocol.TypeJoin, ArticleID: p.articleID, PersonID: p.personID, Name: displayName,
	}); err != nil {
		return err
	}
	log.Printf("已加入 articleId=%s personId=%s outQ=%d", p.articleID, p.personID, len(p.outQ))

	incoming := make(chan []byte, 64)
	readErr := make(chan error, 1)
	go func() {
		for {
			_, data, err := conn.ReadMessage()
			if err != nil {
				readErr <- err
				return
			}
			incoming <- data
		}
	}()

	tick := time.NewTicker(writeEvery)
	defer tick.Stop()
	var disputeC <-chan time.Time

	for {
		if err := p.flushOut(); err != nil {
			return err
		}
		select {
		case <-ctx.Done():
			return nil
		case err := <-readErr:
			return fmt.Errorf("连接断开: %w", err)
		case data := <-incoming:
			if err := p.onMsg(data, &disputeC); err != nil {
				return err
			}
		case <-tick.C:
			if err := p.maybeWrite(); err != nil {
				return err
			}
		case <-disputeC:
			disputeC = nil
			if err := p.onDisputeTimer(); err != nil {
				return err
			}
		}
	}
}

func (p *peer) onMsg(data []byte, disputeC *<-chan time.Time) error {
	var head struct {
		Type string `json:"type"`
	}
	if json.Unmarshal(data, &head) != nil {
		return nil
	}
	switch head.Type {
	case protocol.TypeBootstrap:
		var boot protocol.Bootstrap
		if json.Unmarshal(data, &boot) != nil {
			return nil
		}
		return p.applyBootstrap(boot, disputeC)
	case protocol.TypeRelay:
		var ev protocol.RelayEvent
		if json.Unmarshal(data, &ev) != nil {
			return nil
		}
		return p.applyRelay(ev, disputeC)
	case protocol.TypeAck:
		var ack protocol.Ack
		if json.Unmarshal(data, &ack) != nil {
			return nil
		}
		return p.onAck(ack)
	case protocol.TypeCursor:
	case protocol.TypeError:
		var em protocol.ErrMsg
		if json.Unmarshal(data, &em) == nil {
			return fmt.Errorf("服务端错误: %s", em.Message)
		}
	}
	return nil
}

func (p *peer) applyBootstrap(boot protocol.Bootstrap, disputeC *<-chan time.Time) error {
	// 入场只吃 Base 当前链 + Disputes 显式主张。注意力失活，不误发 CC。
	p.attending = false
	first := !p.booted
	p.booted = true

	p.formal = map[string]string{}
	for _, ln := range boot.Base.Lines {
		p.formal[ln.ID.Hex()] = ln.Content
	}
	if boot.YourLine != "" {
		p.yourLine = boot.YourLine
	} else if p.yourLine == "" && len(boot.Base.Lines) > 0 {
		p.yourLine = boot.Base.Lines[0].ID.Hex()
	}
	if p.focusLine != "" {
		if _, ok := p.formal[p.focusLine]; !ok {
			return fmt.Errorf("要编辑的行已不存在")
		}
		p.yourLine = p.focusLine
	}
	baseText := ""
	if p.yourLine != "" {
		baseText = p.formal[p.yourLine]
	}

	pendingLocal := p.hasUnackedLocalEditOrCC()
	p.stopDisputeTimer()
	if disputeC != nil {
		*disputeC = nil
	}

	if first {
		p.ownText = baseText
		p.foreign = map[string]model.Dispute{}
		p.seen = map[string]bool{}
		p.followTarget = ""
		p.seeded = p.ownText != ""
	} else if !pendingLocal {
		// 重连无未确认 Edit/CC：采纳服务器当前正文。
		p.ownText = baseText
		if baseText != "" {
			p.seeded = true
		}
	}
	// 有 outQ Edit/CC：保留 ownText，下方按 Base/Disputes 判定落地，未确认则原 Op.ID 重发。

	p.pruneOutQFromBootstrap(boot)
	p.inflightSeq = 0
	p.importBootstrapDisputes(boot.Disputes, disputeC)

	log.Printf("Bootstrap yourLine=%s own=%q foreign=%d disputes=%d outQ=%d attending=%v follow=%q",
		p.yourLine, p.ownText, len(p.foreign), len(boot.Disputes), len(p.outQ), p.attending, p.followTarget)
	if !p.seeded && p.yourLine != "" && len(p.outQ) == 0 {
		if err := p.seedWrite(); err != nil {
			return err
		}
	}
	return p.sendCursor(true)
}

// hasUnackedLocalEditOrCC：outQ 里还有本机普通 Edit 或本人 DisputeCC。
func (p *peer) hasUnackedLocalEditOrCC() bool {
	for _, op := range p.outQ {
		switch op.Kind {
		case protocol.TypeSubmit:
			if op.Submit != nil && op.Submit.Action == model.ActionEdit {
				return true
			}
		case protocol.TypeDisputeCC:
			return true
		}
	}
	return false
}

// pruneOutQFromBootstrap：用 Base 正文 / Disputes 同 Claim.ID+Content 判定已落地。
// 无法确认则原 Op.ID 留队重发；不清队列、不造新 ID。
func (p *peer) pruneOutQFromBootstrap(boot protocol.Bootstrap) {
	if len(p.outQ) == 0 {
		return
	}
	byClaim := map[string]model.Dispute{}
	for _, d := range boot.Disputes {
		byClaim[d.ID.Hex()] = d
	}
	kept := p.outQ[:0]
	for _, op := range p.outQ {
		if p.bootstrapOpLanded(op, boot, byClaim) {
			p.onOpAcked(op)
			continue
		}
		kept = append(kept, op)
	}
	p.outQ = kept
}

func (p *peer) bootstrapOpLanded(op protocol.Op, boot protocol.Bootstrap, byClaim map[string]model.Dispute) bool {
	switch op.Kind {
	case protocol.TypeSubmit:
		if op.Submit == nil || len(op.Submit.Content) == 0 {
			return false
		}
		want, lineID := op.Submit.Content[0], op.Submit.LineID
		for _, ln := range boot.Base.Lines {
			if ln.ID.Hex() == lineID && ln.Content == want {
				return true
			}
		}
		return false
	case protocol.TypeDisputeCC:
		if op.DisputeCC == nil || op.DisputeCC.Claim.ID.IsZero() {
			return false
		}
		d, ok := byClaim[op.DisputeCC.Claim.ID.Hex()]
		if !ok {
			return false
		}
		return disputeContent(d) == disputeContent(op.DisputeCC.Claim)
	default:
		return false
	}
}

// importBootstrapDisputes：先扫本人主张定 claimID，再导入他人，最后才 enqueueOwnCC。
// 单次遍历若 foreign 排在 own 前，会在读到本人旧 ID 前 NewID，与服务器同槽冲突（ErrBroken）。
func (p *peer) importBootstrapDisputes(disputes []model.Dispute, disputeC *<-chan time.Time) {
	p.foreign = map[string]model.Dispute{}
	for _, d := range disputes {
		if d.Person == p.personID && !d.ID.IsZero() {
			p.claimID = d.ID
		}
	}
	for _, d := range disputes {
		if d.Person == p.personID || d.ID.IsZero() || !p.ownLineEdit(d) {
			continue
		}
		p.foreign[d.ID.Hex()] = cloneDispute(d)
	}
	for _, d := range disputes {
		if d.Person == p.personID || d.ID.IsZero() || !p.ownLineEdit(d) {
			continue
		}
		p.enqueueOwnCC(d.Person)
	}
	p.syncDisputeTimer(disputeC)
}

// ownLineEdit：夹具只处理本人 yourLine 上的 ActionEdit 主张。
func (p *peer) ownLineEdit(d model.Dispute) bool {
	return p.yourLine != "" && d.Action == model.ActionEdit && d.RealLine.Hex() == p.yourLine
}

func disputeContent(d model.Dispute) string {
	if len(d.Content) == 0 {
		return ""
	}
	return d.Content[0]
}

func (p *peer) applyRelay(ev protocol.RelayEvent, disputeC *<-chan time.Time) error {
	op := ev.Op
	if op.ID != "" && p.seen[op.ID] {
		return nil
	}
	switch {
	case op.Kind == protocol.TypeSubmit && op.Submit != nil && op.Submit.Action == model.ActionEdit:
		return p.onOrdinaryEdit(op, disputeC)
	case op.Kind == protocol.TypeDisputeCC && op.DisputeCC != nil:
		return p.onDisputeCC(op, disputeC)
	case op.Kind == protocol.TypeFollow && op.Follow != nil:
		return p.onFollow(op)
	case op.Kind == protocol.TypeFollowAnswer && op.FollowAnswer != nil:
		return p.onFollowAnswer(op, disputeC)
	default:
		p.markSeen(op.ID)
		return nil
	}
}

// onOrdinaryEdit：在线且本端有注意力时，他人改活跃行异文 → 只发 DisputeCC，本端不进争议。
// Bootstrap/断线补洞/追随被动期：无注意力，直接整合正文。
func (p *peer) onOrdinaryEdit(op protocol.Op, disputeC *<-chan time.Time) error {
	sub := op.Submit
	p.markSeen(op.ID)
	if sub.PersonID == p.personID {
		if len(sub.Content) > 0 {
			p.ownText = sub.Content[0]
			p.formal[sub.LineID] = p.ownText
			p.seeded = true
		}
		return p.sendCursor(false)
	}
	text := ""
	if len(sub.Content) > 0 {
		text = sub.Content[0]
	}
	// 追随成功后：目标人后续普通 Edit 直接采纳，不发 CC。
	if p.followTarget != "" && sub.PersonID == p.followTarget && sub.LineID == p.yourLine {
		p.formal[sub.LineID] = text
		p.ownText = text
		p.seeded = true
		log.Printf("追随中采纳普通 Edit from=%s own=%q", sub.PersonID, p.ownText)
		return p.sendCursor(true)
	}
	if sub.LineID == p.yourLine && p.attentionActive() && text != p.ownText {
		p.enqueueOwnCC(sub.PersonID)
		log.Printf("普通 Edit 触发本人 DisputeCC → %s（本端不进争议）", sub.PersonID)
		return nil
	}
	p.formal[sub.LineID] = text
	if sub.LineID == p.yourLine && !p.attentionActive() {
		p.ownText = text
		if text != "" {
			p.seeded = true
		}
	}
	return p.sendCursor(false)
}

func (p *peer) onDisputeCC(op protocol.Op, disputeC *<-chan time.Time) error {
	cc := op.DisputeCC
	p.markSeen(op.ID)
	claim := cc.Claim
	if claim.Person == p.personID {
		if !claim.ID.IsZero() {
			p.claimID = claim.ID
		}
		content := ""
		if len(claim.Content) > 0 {
			content = claim.Content[0]
		}
		p.confirmedCC[cc.TargetPersonID] = content
		return nil
	}
	// 追随中：仅本行目标人 CC 更新直接采纳，不新开 10s 决策。
	if p.followTarget != "" && claim.Person == p.followTarget && p.ownLineEdit(claim) {
		if len(claim.Content) > 0 {
			p.ownText = claim.Content[0]
			p.formal[p.yourLine] = p.ownText
			p.seeded = true
		}
		key := claim.ID.Hex()
		delete(p.foreign, key)
		if p.disputeKey == disputeKeyOf(claim) {
			p.stopDisputeTimer()
			if disputeC != nil {
				*disputeC = nil
			}
		}
		log.Printf("追随中采纳 DisputeCC from=%s own=%q", claim.Person, p.ownText)
		return p.sendCursor(true)
	}
	if !p.ownLineEdit(claim) {
		return nil
	}
	key := claim.ID.Hex()
	p.foreign[key] = cloneDispute(claim)
	p.enqueueOwnCC(claim.Person)
	log.Printf("收到他人 DisputeCC person=%s claim=%s", claim.Person, key)
	p.syncDisputeTimer(disputeC)
	return p.sendCursor(true)
}

func (p *peer) onFollow(op protocol.Op) error {
	p.markSeen(op.ID)
	f := op.Follow
	if f == nil || f.PersonID == p.personID {
		return nil
	}
	if p.claimID.IsZero() || f.DisputeID != p.claimID.Hex() {
		return nil
	}
	key := followKey(f)
	if p.answered[key] {
		return nil
	}
	opID := p.answerOpID[key]
	if opID == "" {
		opID = model.NewID().Hex()
		p.answerOpID[key] = opID
	}
	if p.hasOutOp(opID) {
		p.stopDisputeTimer()
		return nil
	}
	log.Printf("收到 TypeFollow → 入队 FollowAnswer Accept=true from=%s dispute=%s", f.PersonID, f.DisputeID)
	p.enqueue(protocol.Op{
		ID:   opID,
		Kind: protocol.TypeFollowAnswer,
		FollowAnswer: &protocol.FollowAnswer{
			Type: protocol.TypeFollowAnswer, PersonID: p.personID,
			FromID: f.PersonID, DisputeID: f.DisputeID, Accept: true,
		},
	})
	// 对方已选择追随本人，等待自动答复送达；取消本轮随机跟随决定。
	if p.disputeKey != "" {
		p.handled[p.disputeKey] = true
	}
	p.stopDisputeTimer()
	return nil
}

func (p *peer) onFollowAnswer(op protocol.Op, disputeC *<-chan time.Time) error {
	p.markSeen(op.ID)
	ans := op.FollowAnswer
	if ans == nil || !p.awaitFollow || ans.FromID != p.personID {
		return nil
	}
	p.awaitFollow = false
	if !ans.Accept {
		log.Printf("FollowAnswer 拒绝，保留本人候选")
		return nil
	}
	d, ok := p.foreign[ans.DisputeID]
	if !ok {
		log.Printf("FollowAnswer accept 但本地无该候选 %s", ans.DisputeID)
		return nil
	}
	if !p.ownLineEdit(d) {
		delete(p.foreign, ans.DisputeID)
		log.Printf("FollowAnswer 非本行主张，忽略 dispute=%s", ans.DisputeID)
		return nil
	}
	if len(d.Content) > 0 {
		p.ownText = d.Content[0]
		p.formal[p.yourLine] = p.ownText
	}
	delete(p.foreign, ans.DisputeID)
	p.handled[disputeKeyOf(d)] = true
	p.stopDisputeTimer()
	if disputeC != nil {
		*disputeC = nil
	}
	// 追随成功：暂时放下本行注意力，跟随对方后续普通 Edit/CC。
	p.followTarget = ans.PersonID
	p.attending = false
	log.Printf("追随生效 own=%q follow=%s", p.ownText, p.followTarget)
	return p.sendCursor(true)
}

func (p *peer) buildOwnCC(target string) protocol.Op {
	lineID, _ := model.ParseID(p.yourLine)
	if p.claimID.IsZero() {
		p.claimID = model.NewID()
	}
	return protocol.Op{
		ID:   model.NewID().Hex(), // 每次内容更新新 Op.ID；重试复用队内同一条
		Kind: protocol.TypeDisputeCC,
		DisputeCC: &protocol.DisputeCC{
			TargetPersonID: target,
			Claim: model.Dispute{
				ID:       p.claimID,
				RealLine: lineID,
				Action:   model.ActionEdit,
				Person:   p.personID,
				Content:  []string{p.ownText},
			},
		},
	}
}

// enqueueOwnCC 同 Claim.ID；正文相对已确认有变才入队（新 Op.ID）。Bootstrap 同文已确认不重复入队。
func (p *peer) enqueueOwnCC(target string) {
	if target == "" || target == p.personID {
		return
	}
	if p.confirmedCC[target] == p.ownText {
		return
	}
	if p.hasPendingCC(target, p.ownText) {
		return
	}
	p.enqueue(p.buildOwnCC(target))
}

func (p *peer) hasPendingCC(target, content string) bool {
	for _, op := range p.outQ {
		if op.Kind != protocol.TypeDisputeCC || op.DisputeCC == nil {
			continue
		}
		cc := op.DisputeCC
		if cc.TargetPersonID != target {
			continue
		}
		if len(cc.Claim.Content) > 0 && cc.Claim.Content[0] == content {
			return true
		}
	}
	return false
}

func (p *peer) attentionActive() bool {
	// 须本端已主动编辑，且未处在追随被动期。
	return p.attending && p.yourLine != "" && p.followTarget == ""
}

func (p *peer) seedWrite() error {
	text := randPhrase(p.rng)
	log.Printf("首写 line=%s: %s", p.yourLine, text)
	return p.sendEdit(text)
}

func (p *peer) maybeWrite() error {
	if !p.seeded || p.yourLine == "" || p.awaitFollow {
		return nil
	}
	if len(p.outQ) > 0 {
		return nil
	}
	if p.hasUndecidedForeign() {
		return nil // 10s 决策前不写；保留本人后 handled，可继续写
	}
	next := rotateLine(p.ownText, randPhrase(p.rng), lineMaxRunes)
	return p.sendEdit(next)
}

func (p *peer) hasUndecidedForeign() bool {
	for _, d := range p.foreign {
		if p.ownLineEdit(d) && !p.handled[disputeKeyOf(d)] {
			return true
		}
	}
	return false
}

func (p *peer) sendEdit(content string) error {
	// 自己再写：结束追随被动期，恢复注意力。
	p.followTarget = ""
	p.attending = true
	// 乐观保留本端正文；ACK 拒绝也不回滚。
	p.ownText = content
	p.seeded = true
	// 已收到别人同处主张后，只更新本人候选，不再改共享正式行。
	conflicted := false
	for _, d := range p.foreign {
		if d.RealLine.Hex() == p.yourLine && d.Action == model.ActionEdit {
			conflicted = true
			p.enqueueOwnCC(d.Person)
		}
	}
	if conflicted {
		return nil
	}
	base := p.formal[p.yourLine]
	baseCp := base
	p.enqueue(protocol.Op{
		ID:   model.NewID().Hex(),
		Kind: protocol.TypeSubmit,
		Submit: &protocol.Submit{
			Type:        protocol.TypeSubmit,
			PersonID:    p.personID,
			LineID:      p.yourLine,
			Action:      model.ActionEdit,
			Content:     []string{content},
			ClientTs:    time.Now().UnixMilli(),
			BaseContent: &baseCp,
		},
	})
	return nil
}

func (p *peer) enqueue(op protocol.Op) {
	if op.ID == "" {
		op.ID = model.NewID().Hex()
	}
	if p.hasOutOp(op.ID) {
		return
	}
	p.outQ = append(p.outQ, op)
}

func (p *peer) hasOutOp(id string) bool {
	if id == "" {
		return false
	}
	for _, op := range p.outQ {
		if op.ID == id {
			return true
		}
	}
	return false
}

// flushOut 仅发送队头一条；等 ACK 前不发下一条。
func (p *peer) flushOut() error {
	if p.conn == nil || p.inflightSeq != 0 || len(p.outQ) == 0 {
		return nil
	}
	p.seq++
	p.inflightSeq = p.seq
	op := p.outQ[0]
	log.Printf("发送 kind=%s op=%s seq=%d q=%d", op.Kind, op.ID, p.seq, len(p.outQ))
	return p.writeJSON(protocol.Batch{Type: protocol.TypeBatch, Seq: p.seq, Ops: []protocol.Op{op}})
}

func (p *peer) onAck(ack protocol.Ack) error {
	if p.inflightSeq == 0 || ack.Seq != p.inflightSeq || len(p.outQ) == 0 {
		return nil
	}
	op := p.outQ[0]
	p.inflightSeq = 0
	if ack.Applied < 1 {
		msg := ack.Message
		if msg == "" {
			msg = "未应用"
		}
		// 明确拒绝：出队避免死循环重发；本端正文不回滚；报错。
		p.outQ = p.outQ[1:]
		return fmt.Errorf("ACK 拒绝 seq=%d kind=%s: %s（本端内容已保留）", ack.Seq, op.Kind, msg)
	}
	p.outQ = p.outQ[1:]
	p.onOpAcked(op)
	log.Printf("ACK ok seq=%d kind=%s", ack.Seq, op.Kind)
	if err := p.sendCursor(true); err != nil {
		return err
	}
	return p.flushOut()
}

func (p *peer) onOpAcked(op protocol.Op) {
	p.markSeen(op.ID)
	switch op.Kind {
	case protocol.TypeSubmit:
		if op.Submit != nil && len(op.Submit.Content) > 0 {
			p.ownText = op.Submit.Content[0]
			if op.Submit.LineID != "" {
				p.formal[op.Submit.LineID] = p.ownText
			}
			p.seeded = true
		}
	case protocol.TypeDisputeCC:
		if op.DisputeCC != nil {
			content := ""
			if len(op.DisputeCC.Claim.Content) > 0 {
				content = op.DisputeCC.Claim.Content[0]
			}
			p.confirmedCC[op.DisputeCC.TargetPersonID] = content
			if !op.DisputeCC.Claim.ID.IsZero() {
				p.claimID = op.DisputeCC.Claim.ID
			}
		}
	case protocol.TypeFollowAnswer:
		if op.FollowAnswer != nil {
			key := op.FollowAnswer.FromID + "|" + op.FollowAnswer.DisputeID
			// ClientTs 不在 Answer 里；用 answerOpID 反查。
			for k, id := range p.answerOpID {
				if id == op.ID {
					key = k
					break
				}
			}
			p.answered[key] = true
			if op.FollowAnswer.Accept {
				for id, d := range p.foreign {
					if d.Person == op.FollowAnswer.FromID {
						delete(p.foreign, id)
						delete(p.handled, disputeKeyOf(d))
					}
				}
				if len(p.foreign) == 0 {
					p.stopDisputeTimer()
				}
			}
		}
	case protocol.TypeFollow:
		// RequestFollow 已在入队时设 awaitFollow
	}
}

func (p *peer) sendCursor(force bool) error {
	text, lineID, disputeID := p.cursorTarget()
	off := utf16Len(text)
	key := fmt.Sprintf("%s|%s|%d", lineID, disputeID, off)
	if !force && key == p.lastCursor {
		return nil
	}
	p.lastCursor = key
	if p.conn == nil {
		return nil
	}
	err := p.writeJSON(protocol.CursorMsg{
		Type: protocol.TypeCursor,
		Cursor: protocol.Cursor{
			PersonID: p.personID, Name: displayName,
			LineID: lineID, DisputeID: disputeID,
			Offset: off, SelEnd: off,
		},
	})
	if err == nil {
		log.Printf("光标 dispute=%q utf16=%d", disputeID, off)
	}
	return err
}

func (p *peer) cursorTarget() (text, lineID, disputeID string) {
	lineID, text = p.yourLine, p.ownText
	if len(p.foreign) > 0 && !p.claimID.IsZero() {
		disputeID = p.claimID.Hex()
	}
	return
}

func (p *peer) syncDisputeTimer(disputeC *<-chan time.Time) {
	if disputeC == nil {
		return
	}
	active := map[string]bool{}
	for _, d := range p.foreign {
		if p.ownLineEdit(d) {
			active[disputeKeyOf(d)] = true
		}
	}
	for k := range p.handled {
		if !active[k] {
			delete(p.handled, k)
		}
	}
	if p.disputeKey != "" && !active[p.disputeKey] {
		p.stopDisputeTimer()
		*disputeC = nil
	}
	if p.disputeTimer != nil || p.awaitFollow {
		return
	}
	for _, d := range p.foreign {
		if !p.ownLineEdit(d) {
			continue
		}
		k := disputeKeyOf(d)
		if p.handled[k] {
			continue
		}
		p.disputeKey = k
		log.Printf("争议 10s 后决定 key=%s", k)
		p.disputeTimer = time.NewTimer(disputeWait)
		*disputeC = p.disputeTimer.C
		return
	}
}

func (p *peer) onDisputeTimer() error {
	key := p.disputeKey
	p.disputeTimer, p.disputeKey = nil, ""
	if key == "" {
		return nil
	}
	var others []model.Dispute
	for _, d := range p.foreign {
		if p.ownLineEdit(d) && disputeKeyOf(d) == key {
			others = append(others, d)
		}
	}
	if len(others) == 0 {
		log.Printf("计时到但争议已不在")
		return nil
	}
	p.handled[key] = true
	if p.rng.Intn(2) == 0 {
		t := others[p.rng.Intn(len(others))]
		log.Printf("接受他人候选 → RequestFollow dispute=%s", t.ID.Hex())
		p.awaitFollow = true
		p.enqueue(protocol.Op{
			ID:   model.NewID().Hex(),
			Kind: protocol.TypeFollow,
			Follow: &protocol.Follow{
				Type: protocol.TypeFollow, PersonID: p.personID,
				DisputeID: t.ID.Hex(), ClientTs: time.Now().UnixMilli(),
			},
		})
		return nil
	}
	log.Printf("拒绝：保留本人候选（可继续编辑）")
	return nil
}

func (p *peer) stopDisputeTimer() {
	if p.disputeTimer != nil {
		p.disputeTimer.Stop()
		p.disputeTimer = nil
	}
	p.disputeKey = ""
}

func (p *peer) markSeen(id string) {
	if id == "" {
		return
	}
	p.seen[id] = true
}

func (p *peer) writeJSON(v any) error {
	if p.conn == nil {
		return fmt.Errorf("无连接")
	}
	return p.conn.WriteJSON(v)
}

func followKey(f *protocol.Follow) string {
	return f.PersonID + "|" + f.DisputeID + "|" + fmt.Sprintf("%d", f.ClientTs)
}

func disputeKeyOf(d model.Dispute) string {
	a := d.Action
	if a == model.ActionDelete {
		a = model.ActionEdit
	}
	return d.RealLine.Hex() + "|" + a
}

func cloneDispute(d model.Dispute) model.Dispute {
	out := d
	out.Content = append([]string(nil), d.Content...)
	out.Followers = append([]string(nil), d.Followers...)
	out.Pending = append([]model.PendingConfirm(nil), d.Pending...)
	out.BaseIDs = append([]model.ID(nil), d.BaseIDs...)
	return out
}

func utf16Len(s string) int { return len(utf16.Encode([]rune(s))) }

func randPhrase(r *rand.Rand) string {
	return phrases[r.Intn(len(phrases))] + "，" + phrases[r.Intn(len(phrases))] + "。"
}

func rotateLine(base, extra string, maxRunes int) string {
	next := strings.TrimSpace(base + " " + extra)
	r := []rune(next)
	if len(r) <= maxRunes {
		return next
	}
	return string(r[len(r)-maxRunes:])
}

func normalizeLocalServer(raw string) (string, error) {
	raw = strings.TrimSpace(raw)
	u, err := url.Parse(raw)
	if err != nil || raw == "" || (u.Scheme != "http" && u.Scheme != "https") {
		return "", fmt.Errorf("无效 --server")
	}
	host := u.Hostname()
	if host != "127.0.0.1" && host != "localhost" {
		return "", fmt.Errorf("仅本机，收到 %s", host)
	}
	if ip := net.ParseIP(host); ip != nil && !ip.IsLoopback() {
		return "", fmt.Errorf("非回环: %s", host)
	}
	u.Path = strings.TrimRight(u.Path, "/")
	return strings.TrimRight(u.String(), "/"), nil
}

func toWS(httpBase string) (string, error) {
	u, err := url.Parse(httpBase)
	if err != nil {
		return "", err
	}
	if u.Scheme == "https" {
		u.Scheme = "wss"
	} else {
		u.Scheme = "ws"
	}
	u.Path, u.RawQuery, u.Fragment = "/ws", "", ""
	return u.String(), nil
}

func createArticle(base, title string) (string, error) {
	body, _ := json.Marshal(map[string]string{"title": title})
	resp, err := http.Post(base+"/api/articles", "application/json", strings.NewReader(string(body)))
	if err != nil {
		return "", fmt.Errorf("创建文章失败: %w", err)
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(resp.Body)
	var out struct {
		ID string `json:"id"`
	}
	if resp.StatusCode >= 300 || json.Unmarshal(data, &out) != nil || out.ID == "" {
		return "", fmt.Errorf("创建文章失败: %s", strings.TrimSpace(string(data)))
	}
	return out.ID, nil
}
