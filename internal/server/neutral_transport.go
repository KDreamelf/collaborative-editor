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

func pendingFollowOpID(disputeID, from, to string, clientTs int64) string {
	return fmt.Sprintf("pending-follow:%s:%s:%s:%d", disputeID, from, to, clientTs)
}

// neutralBatch 中立批处理：锁内逐 Op 候选 apply；仅成功新 Op 入 seenPayload。
// 有 Mongo 时先 save 候选 View，成功才替换 r.doc / 记 seen / 计入 Applied 并转发。
// 域校验失败：ACK.Message 填自然文案。Mongo 保存失败：不 ACK、关发送者 WS，客户端重连原 ID 重发。
// 同 ID 同载荷幂等；同 ID 异载荷拒。不广播全局争议快照。调用方锁外 sendAll。
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
	var fresh []protocol.Op
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
		fresh = append(fresh, op)
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
	for _, op := range fresh {
		ev := protocol.RelayEvent{Type: protocol.TypeRelay, Op: op}
		msgs = append(msgs, r.broadcastPayload(mustJSON(ev), client)...)
	}
	return msgs
}

func neutralBaseView(v document.View) document.View {
	base := v
	base.Disputes = []model.Dispute{}
	return base
}
