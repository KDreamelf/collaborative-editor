# livepeer — 正式中立入场真人协作夹具

单事件循环写 WebSocket。连本机**唯一正式服务器**的中立入场（`neutralJoin` Bootstrap / Relay）。`Bootstrap.Base`=当前共享链，`Disputes`=显式主张。Edit / DisputeCC / Follow / FollowAnswer 走同一串行 outQ：ACK Applied 才出队；断线队列留内存，重连原 Op.ID 重发。脚本状态不落盘。Claim.ID 稳定；内容更新用新 Op.ID。

## 启动

```bash
go run -tags fixture ./fixture/livepeer --server http://127.0.0.1:8787
go run -tags fixture ./fixture/livepeer --server http://127.0.0.1:8787 --article <id> --person <personId>
```

仅 `127.0.0.1` / `localhost`。无 `--article` 时创建标题「测试」的文档；显示名「测试脚本」。启动后打印 `articleId` / `personId`。

**请用桌面客户端进入测试文档。** 新参与者会先分到自己的空行；要制造与「测试脚本」的争议，请主动移动到它正在写的行。

## 行为

1. 首次 Join：用 `boot.Base` 当前行链定 `yourLine`/`ownText`；没空字才首写随机文。`boot.Disputes` 中**他人**主张按 Claim.ID 导入争议并开 10s 决策；本人主张只稳本机 `claimID`，不自争议。
2. 重连注意力失活：无未确认 Edit / 未决本人 CC 时，该行正文采纳 `boot.Base` 当前 Content；若 outQ 仍有本机 Edit/CC，保留 `ownText` 与原 Op.ID 重发，避免服务器快照盖字。
3. ACK 丢失的 outQ 用 `boot.Base` 同文 / `boot.Disputes` 同 Claim.ID+Content 判定已落地；无法确认则原 Op.ID 重发（服务器 `seen` / 预生 LineIDs 幂等）。不清队列、不造新 Op.ID。
4. Bootstrap 后服务器用 raw `TypeFollow` Relay 重投 `Dispute.Pending`；夹具照旧立即入队 `FollowAnswer{Accept:true}`（稳定 Answer Op.ID，ACK 后才标记已答）。
5. 约每 5 秒续写同一行（控长）。`CursorMsg` 上报真实 UTF-16 行尾 offset。本端 `sendEdit` 后才有注意力。
6. 在线 raw Relay 普通 Edit/CC **单切面**保持：有注意力时别人改活跃行异文 → 只发本人 DisputeCC（本端不因发 CC 进争议）；收到他人 DisputeCC 才记争议并开 10s。保留本人后继续写并再 CC；`RequestFollow` 后等 Answer，其间不写。
7. `FollowAnswer` 接受后：暂时放下注意力，跟随目标人后续普通 Edit/CC；本端再 `sendEdit` 才恢复注意力。

## 检查

```bash
go test -tags fixture ./fixture/livepeer
go build -tags fixture ./fixture/livepeer
```
