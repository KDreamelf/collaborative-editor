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

`fixture/livepeer` 是真人联测夹具：创建「测试」文档，以「测试脚本」身份定时编辑，收到别人的主张后等 10 秒再随机决定是否追随。请在桌面客户端加入该文档联测。

```powershell
go run -tags fixture ./fixture/livepeer --server http://127.0.0.1:8787
```
