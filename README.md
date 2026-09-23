# 协同编辑器

多人同时写，不锁行。同一处两份主张都留着，谁也不盖谁。设计在 `需求设计.md`。

## 跑起来

```powershell
go run ./cmd/server
```

默认 `ADDR=:8787`，`MONGO_URI=mongodb://127.0.0.1:27017`，`MONGO_DB=editor`。一篇文章一个集合。

Mongo 启动时不可达：只要 URI 能建起 client 就保留连接，Ping 失败只记日志；编辑仍走内存。`StartFlush` 每 2 秒重试：补加载库里尚未进内存的文章（已打开/已改房间不覆盖），并把 dirty 房间落盘；读写失败留待下轮，不会把暂时读不到当成空库写回。完全连不上（URI 非法等）则本进程无持久化，需修好环境后重启。

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

`fixture/` 是测试夹具，不进正式编译。`cases/*.jsonl` 才是用例。夹具用 ACP（Agent Client Protocol，标准输入输出上每行一条 JSON-RPC）执行用例里的操作。跑完按时间戳记在 `fixture/archive/`。

```powershell
go test -tags fixture ./fixture
```
