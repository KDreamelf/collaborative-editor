# 协同编辑器

多人同时写，不锁行。同一处两份主张都留着，谁也不盖谁。新参与者先分到自己的初始空行，仍可主动移到别人的行编辑。设计在 `需求设计.md`；已踩过的架构偏移见 `过度设计踩坑记录.md`。

## 跑起来

```powershell
go run ./cmd/server
```

默认 `ADDR=:8787`，不配置 `MONGO_URI` 时使用内存。需要持久化时明确配置 `MONGO_URI`；连接失败会阻止服务启动。`MONGO_DB` 默认 `editor`，一篇文章一个集合。

配置 Mongo 后，普通操作先保存成功再确认送达；运行中保存失败时客户端保留修改并重连重发。

桌面端：

```powershell
cd client
wails dev
```

管理后台：

```powershell
cd admin
npm install
npm run dev
```

后台地址 `http://127.0.0.1:5174`，接口 `http://127.0.0.1:8787`。

`fixture/livepeer` 是专属客户端，不是网页替身。脚本按正式协议加入文档、定时编辑；收到写给自己的主张后等 10 秒再随机决定是否追随。旁观到的落盘争议不把自己变成当事人。请在桌面客户端加入该文档联测。

## 未来方向

夹具已经说明：参与编辑不必坐在桌面界面前，一个专属客户端就能代表一个人。AI 用 ACP（Agent Client Protocol，智能体客户端协议）的 stdio 驱动 `fixture/editoracp`，直接加入同一篇文档。

```powershell
go run ./fixture/editoracp
```

stdin 一行一个 JSON-RPC。`initialize`、`session/new` 之后，`session/prompt` 的文本是一条命令：`join <http> <articleId> <personId>`、`edit <行号> <正文>`、`say <正文>`、`accept <行号> <人>`、`reject <行号> <人>`、`view`。`say` 在文末另起一行。`accept` 是追随对方主张，`reject` 是不接受、保留自己的句子。stdout 只回协议。编辑规则仍以 `需求设计.md` 为准。

```powershell
go run -tags fixture ./fixture/livepeer --server http://127.0.0.1:8787
```
