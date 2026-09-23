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

后台地址 `http://127.0.0.1:5174`，接口 `http://127.0.0.1:8080`。
