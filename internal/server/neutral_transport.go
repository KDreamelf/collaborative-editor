package server

import (
	"context"
	"crypto/sha256"
	"fmt"
	"log"
	"time"

	"github.com/KDreamelf/collaborative-editor/internal/document"
	"github.com/KDreamelf/collaborative-editor/internal/model"
	"github.com/KDreamelf/collaborative-editor/internal/protocol"
)

// 客户端 onAck 只在 Message 非空时认失败槽；文案不暴露协议字段。
const neutralSyncFailMsg = "这处修改暂时无法同步，内容仍保存在本机"

// neutralJoin 中立入场：加入顺序决定初始行；行数不够时补系统空行。
// Bootstrap 后对 Pending.To==personID 补发 raw TypeFollow Relay（稳定 pending key，不入 seen）。
// 给其他在线者 TypeSnapshot 更新在场信息。调用方在锁外 sendAll。
func (h *Hub) neutralJoin(r *room, client *wsClient, personID, name string) []outbound {
	if personID == "" {
		return []outbound{{client, mustJSON(protocol.ErrMsg{Type: protocol.TypeError, Message: "缺少 personId"})}}
	}
	if r == nil {
		return []outbound{{client, mustJSON(protocol.ErrMsg{Type: protocol.TypeError, Message: "文章不存在"})}}
	}

	r.mu.Lock()
	defer r.mu.Unlock()

	v, err := r.doc.View()
	if err != nil {
		return []outbound{{client, mustJSON(protocol.ErrMsg{Type: protocol.TypeError, Message: err.Error()})}}
	}
	idx := r.personIndex(personID)
	newJoiner := idx < 0
	if newJoiner {
		idx = len(r.joinOrder)
	}
	if len(v.Lines) <= idx {
		if err := r.doc.EnsureLines(idx + 1); err != nil {
			return []outbound{{client, mustJSON(protocol.ErrMsg{Type: protocol.TypeError, Message: err.Error()})}}
		}
		v, err = r.doc.View()
		if err != nil {
			return []outbound{{client, mustJSON(protocol.ErrMsg{Type: protocol.TypeError, Message: err.Error()})}}
		}
		r.dirty = true
	}

	client.personID = personID
	client.name = name
	r.clients[client] = struct{}{}
	r.names[personID] = name
	if newJoiner {
		r.joinOrder = append(r.joinOrder, personID)
	}
	yourLine := v.Lines[idx].ID.Hex()
	if _, ok := r.cursors[personID]; !ok {
		r.cursors[personID] = protocol.Cursor{
			PersonID: personID,
			Name:     name,
			LineID:   yourLine,
			Offset:   0,
			SelEnd:   0,
		}
	}

	boot := protocol.Bootstrap{
		Type:     protocol.TypeBootstrap,
		Base:     neutralBaseView(v),
		Disputes: append([]model.Dispute(nil), v.Disputes...),
		YourLine: yourLine,
		People:   r.onlinePeople(),
		Cursors:  r.cursorList(),
	}
	msgs := []outbound{{client, mustJSON(boot)}}
	msgs = append(msgs, pendingFollowRelays(client, personID, v.Disputes)...)
	msgs = append(msgs, pendingDisputeRelays(client, personID, v.Disputes)...)
	if snap, err := r.buildSnapshot(""); err == nil {
		msgs = append(msgs, r.broadcastPayload(mustJSON(snap), client)...)
	}
	return msgs
}

// pendingFollowRelays 把指向 personID 的待确认追随重发成 raw Follow Relay。
// Op.ID 用稳定 pending key，不写入 seenPayload。
func pendingFollowRelays(client *wsClient, personID string, disputes []model.Dispute) []outbound {
	var msgs []outbound
	for _, d := range disputes {
		for _, p := range d.Pending {
			if p.To != personID {
				continue
			}
			ev := protocol.RelayEvent{
				Type: protocol.TypeRelay,
				Op: protocol.Op{
					ID:   pendingFollowOpID(d.ID.Hex(), p.From, p.To, p.ClientTs),
					Kind: protocol.TypeFollow,
					Follow: &protocol.Follow{
						PersonID:  p.From,
						DisputeID: d.ID.Hex(),
						ClientTs:  p.ClientTs,
					},
				},
			}
			msgs = append(msgs, outbound{client, mustJSON(ev)})
		}
	}
	return msgs
}

// pendingDisputeRelays 重连时把写给 personID 的主张包再送一次。旁观者不收。
func pendingDisputeRelays(client *wsClient, personID string, disputes []model.Dispute) []outbound {
	var msgs []outbound
	for _, d := range disputes {
		if d.Target != personID || d.Person == personID || d.ID.IsZero() {
			continue
		}
		ev := protocol.RelayEvent{
			Type: protocol.TypeRelay,
			Op: protocol.Op{
				ID:   "dispute-target:" + d.ID.Hex(),
				Kind: protocol.TypeDisputeCC,
				DisputeCC: &protocol.DisputeCC{
					TargetPersonID: personID,
					Claim:          d,
				},
			},
		}
		msgs = append(msgs, outbound{client, mustJSON(ev)})
	}
	return msgs
}

func pendingFollowOpID(disputeID, from, to string, clientTs int64) string {
	return fmt.Sprintf("pending-follow:%s:%s:%s:%d", disputeID, from, to, clientTs)
}

// neutralBatch 中立批处理：锁内逐 Op 候选 apply；仅成功新 Op 入 seenPayload。
// 有 Mongo 时先 save 候选 View，成功才替换 r.doc / 记 seen / 计入 Applied 并转发。
// 域校验失败：ACK.Message 填自然文案。Mongo 保存失败：不 ACK、关发送者 WS，客户端重连原 ID 重发。
// 同 ID 同载荷幂等；同 ID 异载荷拒。不广播全局快照。
// 主张、追随、追随答复只送给当事人；旁观者另收已落盘争议记录。调用方锁外 sendAll。
func (h *Hub) neutralBatch(r *room, client *wsClient, batch protocol.Batch) []outbound {
	if r == nil {
		return nil
	}
	r.mu.Lock()
	defer r.mu.Unlock()

	senderID := ""
	if client != nil {
		senderID = client.personID
	}

	applied := 0
	ackMsg := ""
	persistFail := false
	var fresh []routedOp
	for _, op := range batch.Ops {
		hash := sha256.Sum256(mustJSON(op))
		if op.ID != "" {
			if prev, ok := r.seenPayload[op.ID]; ok {
				if prev != hash {
					log.Printf("neutralBatch reject duplicate id with different payload op=%s", op.ID)
					ackMsg = neutralSyncFailMsg
					break
				}
				applied++
				continue
			}
		}
		party, scoped := partyPersonID(r, op)
		routed := routedOp{op: op, party: party, partyScoped: scoped}
		if h.mongo == nil {
			if err := r.applyNeutralOp(senderID, op); err != nil {
				log.Printf("neutralBatch apply failed op=%s: %v", op.ID, err)
				ackMsg = neutralSyncFailMsg
				break
			}
		} else {
			cand := r.doc.Clone()
			prev := r.doc
			r.doc = cand
			if err := r.applyNeutralOp(senderID, op); err != nil {
				r.doc = prev
				log.Printf("neutralBatch apply failed op=%s: %v", op.ID, err)
				ackMsg = neutralSyncFailMsg
				break
			}
			view, err := r.doc.View()
			if err != nil {
				r.doc = prev
				log.Printf("neutralBatch view failed op=%s: %v", op.ID, err)
				persistFail = true
				break
			}
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			err = h.mongo.save(ctx, view)
			cancel()
			if err != nil {
				r.doc = prev
				log.Printf("neutralBatch mongo save failed op=%s: %v", op.ID, err)
				persistFail = true
				break
			}
			r.dirty = false
		}
		if r.seenPayload == nil {
			r.seenPayload = map[string][32]byte{}
		}
		r.seenPayload[op.ID] = hash
		applied++
		fresh = append(fresh, routed)
	}
	if h.mongo == nil {
		r.dirty = true
	}

	var msgs []outbound
	if persistFail {
		if client != nil && client.conn != nil {
			_ = client.conn.Close()
		}
	} else if client != nil {
		msgs = append(msgs, outbound{client, mustJSON(protocol.Ack{
			Type:    protocol.TypeAck,
			Seq:     batch.Seq,
			Applied: applied,
			Message: ackMsg,
		})})
	}
	sawDispute := false
	for _, item := range fresh {
		ev := protocol.RelayEvent{Type: protocol.TypeRelay, Op: item.op}
		raw := mustJSON(ev)
		if item.partyScoped {
			sawDispute = true
			if item.party != "" && item.party != senderID {
				if to := r.clientByPerson(item.party); to != nil {
					msgs = append(msgs, outbound{to, raw})
				}
			}
			continue
		}
		msgs = append(msgs, r.broadcastPayload(raw, client)...)
	}
	if sawDispute {
		if view, err := r.doc.View(); err == nil {
			raw := mustJSON(protocol.DisputeRecord{
				Type:     protocol.TypeDisputeRecord,
				Disputes: view.Disputes,
			})
			for c := range r.clients {
				if c == client || c.personID == senderID {
					continue
				}
				msgs = append(msgs, outbound{c, raw})
			}
		}
	}
	return msgs
}

// routedOp 记下转发前的当事人。追随可能在 apply 后收口，收件人必须事先记下。
type routedOp struct {
	op          protocol.Op
	party       string
	partyScoped bool
}

func partyPersonID(r *room, op protocol.Op) (string, bool) {
	switch op.Kind {
	case protocol.TypeDisputeCC:
		if op.DisputeCC == nil {
			return "", true
		}
		return op.DisputeCC.TargetPersonID, true
	case protocol.TypeFollow:
		return followOwner(r, op), true
	case protocol.TypeFollowAnswer:
		if op.FollowAnswer == nil {
			return "", true
		}
		return op.FollowAnswer.FromID, true
	default:
		return "", false
	}
}

func followOwner(r *room, op protocol.Op) string {
	if r == nil || r.doc == nil || op.Follow == nil {
		return ""
	}
	id, err := model.ParseID(op.Follow.DisputeID)
	if err != nil {
		return ""
	}
	view, err := r.doc.View()
	if err != nil {
		return ""
	}
	for _, d := range view.Disputes {
		if d.ID == id {
			return d.Person
		}
	}
	return ""
}

func (r *room) clientByPerson(personID string) *wsClient {
	if personID == "" {
		return nil
	}
	for c := range r.clients {
		if c.personID == personID {
			return c
		}
	}
	return nil
}

func neutralBaseView(v document.View) document.View {
	base := v
	base.Disputes = []model.Dispute{}
	return base
}
