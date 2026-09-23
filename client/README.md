# 桌面客户端

在项目根目录运行服务端后，在本目录执行：

```powershell
wails dev
```

客户端包含文档列表、新建与继续编辑、连接设置、连续编辑面、追随确认和未同步原文面板。主张、离线队列和本地保存由 Go 处理，Vue 负责显示与操作。

正式流程应通过桌面窗口或 Wails 开发服务打开。单独运行前端 Vite 不包含 Go 桥接。

## 布局演示

```powershell
cd frontend
npm install
npm run dev -- --port 5175
```

访问 `http://127.0.0.1:5175/?mock=1` 查看插入争议布局；追加 `&span=1` 查看选区范围候选。演示数据只在开发模式载入，不包含真实同步、离线恢复或持久化。

## 验证与打包

```powershell
go test ./...
cd frontend
npm run check:editor
npm run build
cd ..
wails build
```

`check:editor` 验证选区替换、多行光标、插入上下文和局部文本同步。窗口切换、输入法、追随与离线恢复还需要真实客户端联调；mock 页面不能代替这些验证。
