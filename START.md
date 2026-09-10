# 🚀 快速启动指南

## 一键启动命令

### Git Bash / Linux / macOS

```bash
# 开发模式（推荐）- 前端 + 后端，支持热更新
./start.sh

# 仅后端
./start.sh backend

# 后端 + IPFLY 代理
./start.sh proxy
```

### PowerShell / Windows

```powershell
# 开发模式（推荐）
.\start.ps1

# 仅后端
.\start.ps1 backend

# 后端 + IPFLY 代理
.\start.ps1 proxy
```

## 停止网关

### Git Bash
```bash
./stop.sh
```

### PowerShell
```powershell
.\stop.ps1
```

## 访问地址

启动成功后，访问：

- **开发模式**：
  - 前端 UI: http://127.0.0.1:5173
  - 网关 API: http://127.0.0.1:18093

- **后端模式**：
  - 网关 API: http://127.0.0.1:18093

## 健康检查

```bash
# 检查服务是否正常
curl http://127.0.0.1:18093/__health

# 查看状态
curl http://127.0.0.1:18093/__state

# 列出模型
curl http://127.0.0.1:18093/v1/models
```

## 配置 IPFLY 代理（可选）

如果需要使用 IPFLY 代理避免 Claude 封号：

1. 编辑 `start-with-ipfly.sh` 或 `start-with-ipfly.ps1`
2. 填入 IPFLY 配置：
   ```bash
   IPFLY_HOST="你的IP"
   IPFLY_PORT="端口"
   IPFLY_USER="用户名"
   IPFLY_PASS="密码"
   ```
3. 启动：`./start.sh proxy`

## 常见问题

### 端口被占用

如果提示端口 18093 被占用：

```bash
# 查找占用进程
netstat -ano | grep :18093

# 停止旧进程
./stop.sh  # 或 .\stop.ps1
```

### 首次启动慢

首次运行会自动安装 npm 依赖，需要几分钟时间，请耐心等待。

### OAuth 回调失败

如果遇到 `localhost:1455` 无法访问：

1. 确保 Clash 配置了 localhost 直连
2. 或临时关闭 Clash 系统代理
3. 或使用手动粘贴 code 的方式完成认证

---

**详细文档**：请查看项目根目录的 `README.md`
