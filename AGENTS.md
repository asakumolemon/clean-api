# AGENTS.md

## 项目概览

- `clean-api` 是面向个人与小团队的单二进制 Go + SQLite LLM API 网关；Go module 名为 `api-gateway`。
- 提供 OpenAI Chat、OpenAI Responses、Anthropic Messages 三种对外入口，通过内部 IR 转换并路由至上游；不实现计费、余额或账单逻辑。
- `REQUIREMENTS.md` 是需求基线。涉及既有行为时，以当前代码、测试和 README 为准；其中部分早期设计已演进。

## 架构边界

- `cmd/server/main.go` 是组合根：配置、数据库、依赖装配、路由和后台任务都从这里启动。
- `internal/protocol` 定义 IR，负责三种协议的请求/响应/SSE 转换；协议格式兼容应放在这里或对应上游适配器，不要塞进路由层。
- `internal/upstream` 负责 IR 与上游协议之间的适配。目前仅支持 OpenAI 兼容与 Anthropic Messages 原生上游；Responses 原生及厂商特有上游应保持明确的未支持错误（501）。
- `internal/router` 只负责模型→渠道选择、别名/重定向、能力降级和重试；`internal/channel` 负责探测、模型同步、密钥轮换与健康检查。
- `internal/store` 负责 SQLite 持久化和迁移；`internal/api` 负责鉴权后的 API 流水线、缓存和请求日志；`internal/web` 与根目录 `web/` 是嵌入二进制的服务端管理后台模板。

## 技术与文案约束

- 保持 Go 单二进制、chi、`modernc.org/sqlite`（纯 Go、无 CGO）的技术栈。
- 管理后台使用 `html/template` 和 Tailwind CSS Play CDN。禁止引入 Node、`node_modules`、前端框架或前端构建流程；不要把 `prototype.html` 的 SPA 切页方式搬进生产模板。
- 文档、注释、UI 文案和用户可见错误默认使用中文，并沿用现有命名与代码风格。
- 配置来自 `config.json`，可由 `GATEWAY_*` 环境变量覆盖。生产环境应固定 `GATEWAY_ENC_KEY` 和会话密钥；明文 HTTP 下 `session_secure` 必须为 `false`。

## 安全与行为不变量

- API 令牌仅在创建时展示明文，数据库只存 SHA-256 哈希；同时支持 `Authorization: Bearer` 与 `x-api-key`。
- 令牌模型白名单默认必填；“允许全部模型”必须显式选择。不得绕过或静默扩大授权范围。
- 上游 API key 在配置 `GATEWAY_ENC_KEY` 时必须 AES-GCM 加密；缺失时的明文降级必须保留启动告警，不能静默处理。
- 鉴权和模型白名单校验必须先于缓存查询。缓存只保存成功的非流式响应，按令牌与原始请求体隔离；流式请求不缓存。
- 对外模型名用于白名单校验；模型重定向在 router 内随后应用。不要改变此顺序。
- 429/401 应触发 key 冷却与轮换；普通上游 4xx 直接透传；网络错误和 5xx 才可跨渠道重试。流式仅能在首个事件发出前重试。
- 请求日志异步写入，落库失败不得影响请求；成功流式与失败请求也要记录，失败记录应携带最后尝试的渠道 ID。

## 数据库与渠道注意事项

- 迁移必须向后兼容且幂等。`tokens` 表的 `group` 是 SQLite 保留字，SQL 中必须写为 `` `group` ``。
- 能力探测默认关闭，以免消耗上游配额；同步模型使用保守能力默认值，能力探测须显式启用或手动触发。
- Anthropic 健康检查使用 `POST /v1/messages`。原生 Anthropic 上游通常不能通过 `/v1/models` 列模型，必要时在管理后台手动添加模型。
- 管理后台包含令牌分组与批量建令牌、渠道和模型管理、日志、用户、测试台及导入导出；时间展示遵循配置的时区。

## 验证

在仓库根目录运行：

- `make build`：构建 `bin/gateway`，并注入版本号。
- `make test`：运行全部 Go 测试。
- `make vet`：运行静态检查。
- `make run`：本地启动服务。
- `make fmt`：执行 `gofmt -w .`，会直接修改文件。
- `make docker-build` / `make docker-run`：构建或运行 Docker 镜像。

修改请求转换、路由、迁移、鉴权、缓存或管理后台行为后，至少运行相关测试；常规改动运行 `make test`。