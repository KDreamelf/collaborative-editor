# 协同编辑器

多人同时写，不锁行。同一处两份主张都留着，谁也不盖谁。设计在 `需求设计.md`。

## 跑起来

先起 MongoDB。服务端没连上库也能先编辑，内存为准，连上后再写入。

```powershell
go run ./cmd/server
```

默认 `mongodb://127.0.0.1:27017`，库名 `editor`。一篇文章一个集合。

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
