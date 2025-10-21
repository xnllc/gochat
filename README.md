# 📄 Go 聊天室项目说明文档

---

## 🎯 项目简介

这是一个基于 **Go 语言** 开发的轻量级 **实时聊天系统**，支持：

- 多人实时文字聊天（WebSocket）
- 文件上传与分享（图片自动预览，其他文件可下载）
- 系统消息通知（用户上线/离线）
- 响应式网页界面（适配桌面与手机）

适用于内网协作、学习演示或小型团队沟通。

---

## 📁 项目结构

```
go-chat/
├── go.mod                # Go 模块定义
├── main.go               # 服务端主程序
└── public/
    ├── index.html        # 聊天主界面
    └── (可选) files.html # 文件管理页（如有）
```

文件默认上传至 `./uploads/` 目录。

---

## ⚙️ 运行环境要求

- Go 1.20+（你使用的是 1.24.8，完全兼容）
- 网络可访问（部署后通过浏览器访问）
- （推荐）Linux / Windows / macOS 均可

---

## 🚀 快速启动（国内环境）

### 1. 克隆或创建项目目录

```bash
mkdir go-chat && cd go-chat
```

### 2. 创建 `go.mod`

```go
module go-chat

go 1.24.8

require (
	github.com/gorilla/websocket v1.5.3
	github.com/rs/cors v1.11.1
)
```

### 3. 保存 `main.go` 和 `public/index.html`

- 将你的服务端代码保存为 `main.go`
- 将优化后的 HTML 保存为 `public/index.html`

### 4. 创建上传目录

```bash
mkdir uploads
```

### 5. 一条命令运行（自动使用国内镜像）

```bash
GOPROXY=https://goproxy.cn,direct go run main.go
```

> 首次运行会自动下载依赖，后续可直接 `go run main.go`

---

## 🌐 访问方式

服务默认监听 **3027 端口**，启动后：

- 聊天页面：`http://<服务器IP>:3027/`
- 示例：`http://192.168.1.100:3027`（内网）或 `http://your-domain.com:3027`（公网）

> 💡 建议搭配 Nginx 反向代理 + HTTPS 用于公网部署。

---

## 🔒 安全提醒（重要！）

本项目为**演示/内网用途设计**，若部署到公网，请务必：

1. **限制文件上传类型**（仅允许 `.jpg`, `.png`, `.pdf` 等）
2. **重命名上传文件**（避免路径遍历或恶意脚本执行）
3. **不要开放 `uploads` 目录的执行权限**
4. **启用 HTTPS/WSS**（防止消息被窃听）
5. **考虑添加身份验证**（当前无登录机制）

> ⚠️ 未经加固的公网部署可能导致服务器被入侵！



```bash
# 默认启动
go run main.go

# 自定义配置
./go-chat -port=9000 -upload-dir=chat_files -max-size=200M

# Windows
go-chat.exe -max-size=1.5G -upload-dir="D:\chat\uploads"
```
