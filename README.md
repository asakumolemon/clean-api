# 智能 API 分发网关

填一个 `base_url + api_key`，网关自动探测上游类型、自动拉取模型列表、自动判断模型能力，对外统一提供 **OpenAI Chat / Responses / Anthropic Messages** 三种协议。轻量、单二进制、无外部依赖，**不含任何计费逻辑**。

## 特性

- **智能接入**：添加渠道只需填 `base_url + api_key`，自动识别协议（OpenAI / Anthropic / Responses）、自动同步模型列表、自动探测能力（system / 工具调用 / 视觉 / JSON mode），结果可手动覆盖
- **三协议入口**：`/v1/chat/completions`（NextChat / LobeChat / 各类 SDK）、`/v1/responses`（新版 OpenAI SDK）、`/v1/messages`（Claude Code / Cursor / Anthropic SDK），流式与非流式均支持，工具调用三协议互转
- **路由分发**：模型 → 渠道映射（random / round_robin）、多 key 轮换与冷却（429/401 自动切换）、5xx 换渠道重试、渠道健康检查自动绕开故障渠道、全局模型重定向与渠道内模型别名
- **访问控制**：令牌（Bearer）+ 模型白名单（必须显式指定；「允许全部模型」需显式勾选），无计费
- **管理面**：服务端渲染（Tailwind CSS Play CDN，无构建工具）——渠道/模型/令牌/用户管理、请求日志（筛选+分页）、测试台、配置导入导出
- **轻量**：Go 单二进制 + SQLite（纯 Go，无 CGO），常驻内存几十 MB；上游 key AES-GCM 加密存储

## 快速开始

### 方式一：裸二进制

```bash
# 构建
make build            # 或 go build -o bin/gateway ./cmd/server

# 配置密钥（生产必配）
export GATEWAY_ENC_KEY=$(openssl rand -hex 16)   # 渠道 key 加密密钥
export GATEWAY_ADMIN_PASSWORD=你的管理员密码        # 首启创建管理员

# 启动
make run              # 或 ./bin/gateway
```

启动后访问管理面 `http://127.0.0.1:8080/admin/login`（默认配置端口 8080），按提示登录。

### 方式二：Docker

```bash
make docker-build     # 构建镜像（VERSION=xxx 可注入版本号）

docker run -d --name api-gateway -p 8080:8080 \
  -v gateway-data:/data \
  -e GATEWAY_ADMIN_PASSWORD=你的管理员密码 \
  -e GATEWAY_ENC_KEY=$(openssl rand -hex 16) \
  api-gateway:latest
```

数据（SQLite）持久化在 `gateway-data` 卷。

### 三步跑通

1. **添加渠道**：管理面 → 渠道管理 → 新建，填名称 + `base_url`（如 `https://api.deepseek.com`）+ API key，类型选「自动探测」。探测完成后渠道页显示识别出的协议类型与模型列表。
2. **生成令牌**：令牌管理 → 新建，指定模型白名单（或显式勾选「允许全部模型」），**明文只展示一次，请立即保存**。
3. **调用**：

```bash
curl http://127.0.0.1:8080/v1/chat/completions \
  -H "Authorization: Bearer <令牌>" \
  -H "Content-Type: application/json" \
  -d '{"model":"deepseek-chat","messages":[{"role":"user","content":"你好"}]}'
```

## 客户端接入

| 客户端 | 配置 |
|---|---|
| **NextChat** | 自定义接口：`https://你的网关地址`，API Key 填网关令牌，模型填渠道同步出的模型名（非流式/流式均可） |
| **Claude Code** | `ANTHROPIC_BASE_URL=http://你的网关地址`、`ANTHROPIC_AUTH_TOKEN=<网关令牌>`、`ANTHROPIC_MODEL=<模型名>`（或 settings.json 配置），走 `/v1/messages`，支持流式与工具调用 |
| **Cursor** | 设置 → Models → OpenAI-compatible provider，base URL 填网关地址，API key 填网关令牌 |
| **新版 OpenAI SDK** | `base_url` 指向网关，走 `/v1/responses` |

## 配置说明

`config.json`（可用环境变量覆盖，前缀 `GATEWAY_`）：

| 配置项 | 环境变量 | 默认值 | 说明 |
|---|---|---|---|
| `addr` | `GATEWAY_ADDR` | `:8080` | HTTP 监听地址 |
| `db_path` | `GATEWAY_DB_PATH` | `data/gateway.db` | SQLite 文件路径 |
| `session_secret` | `GATEWAY_SESSION_SECRET` | 随机生成 | 管理面会话签名密钥；缺省随机生成（重启后登录态失效） |
| `session_secure` | `GATEWAY_SESSION_SECURE` | `false` | 管理面 cookie 加 Secure（仅 HTTPS 时开） |
| `log_level` | `GATEWAY_LOG_LEVEL` | `info` | debug/info/warn/error |
| `log_retention_days` | — | `7` | 请求日志保留天数 |
| `default_timeout_seconds` | — | `120` | 上游请求超时 |
| `enc_key` | `GATEWAY_ENC_KEY` | 空 | 渠道 key 的 AES-GCM 加密密钥；**生产必配**，缺省明文存储（启动有警告） |
| `routing_strategy` | `GATEWAY_ROUTING_STRATEGY` | `random` | 模型→渠道选择：random/round_robin |
| `health_check_enabled` | `GATEWAY_HEALTH_CHECK_ENABLED` | `true` | 渠道健康检查开关 |
| `health_check_interval_seconds` | `GATEWAY_HEALTH_CHECK_INTERVAL_SECONDS` | `300` | 健康检查间隔 |
| `health_check_max_failures` | `GATEWAY_HEALTH_CHECK_MAX_FAILURES` | `3` | 连续失败 N 次标记 down |
| `key_cooldown_seconds` | `GATEWAY_KEY_COOLDOWN_SECONDS` | `60` | 单 key 冷却时长（429/401 后） |
| `model_redirects` | — | `{}` | 全局模型重定向：`{"旧模型名": "实际模型名"}` |
| `admin_username` | `GATEWAY_ADMIN_USERNAME` | `admin` | 首启管理员用户名 |
| `admin_password` | `GATEWAY_ADMIN_PASSWORD` | 随机生成 | 首启管理员密码（随机时打印到日志） |

## 对外 API

三个入口协议保持**原生格式**（请求与响应都按对应协议），内部统一转 IR 再路由到上游：

- `POST /v1/chat/completions` — OpenAI Chat（`stream` 参数控制流式）
- `POST /v1/messages` — Anthropic Messages（请求需带 `max_tokens`；`x-api-key` 头或 `Authorization: Bearer` 均可鉴权）
- `POST /v1/responses` — OpenAI Responses
- `GET /v1/models` — 启用模型的对外名列表（OpenAI 格式）

错误响应：OpenAI/Responses 用 `{"error": {"message", "type"}}`，Anthropic 用 `{"type": "error", "error": {...}}`，状态码贴近上游原意。每个响应带 `X-Request-Id`，可在请求日志页排查。

## 管理面功能

登录 / 仪表盘（统计 + 最近请求）/ 渠道管理（添加即自动探测、启停、重探测、删除）/ 模型管理（启停、别名、能力覆盖）/ 令牌管理（白名单必填）/ 请求日志（模型/令牌/状态筛选 + 分页）/ 测试台（免令牌直连路由）/ 用户管理（多管理员）/ 导入导出（全量 JSON 迁移）。

## 开发

```bash
make build    # 构建（go build ./...）
make test     # 全部单测（go test ./...）
make vet      # 静态检查
make fmt      # 格式化（gofmt -w .）
make docker-build
```

需求与设计文档见 `REQUIREMENTS.md`（唯一权威）。代码结构与关键决策见 `AGENTS.md`。

## 常见问题

- **探测失败**：渠道页显示各协议探测证据（状态码 + 响应摘要）。常见原因：`base_url` 少了 `https://`、上游需要代理（当前版本不内置代理）、API key 无效、上游非标准协议（可手动指定类型）。
- **明文 HTTP 下管理面登录不生效**：`session_secure` 必须保持 `false`（仅 HTTPS 部署时开）。
- **换了 `GATEWAY_ENC_KEY`**：库中已加密的渠道 key 将全部无法解密（启动时有明确告警），需重新添加渠道 key。
- **令牌明文丢了**：令牌只存哈希，无法找回；删掉重新生成。
- **导入导出**：导入为替换式（单事务、失败回滚）；key 为密文需同一 `GATEWAY_ENC_KEY`，令牌明文不可恢复——迁移后重新生成令牌。
- **模型「不存在」**：模型在渠道页必须处于启用状态，且渠道状态为 active（健康检查失败会被自动标记 down，路由绕开）。

## 安全提示

- 生产环境务必配置 `GATEWAY_ENC_KEY` 与 `GATEWAY_ADMIN_PASSWORD`，并在 HTTPS 后置部署（或开启 `session_secure`）。
- 网关本身不含计费/限流，请勿直接暴露到公网（个人/团队内网自用定位）。
