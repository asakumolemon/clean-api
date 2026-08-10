# AGENTS.md

## 项目状态

- 已完成 **M1 骨架**（脚手架 + store/users/tokens + auth + `/v1/chat/completions` 鉴权桩 + 管理面登录/仪表盘/令牌页）、**M2 渠道+探测**（渠道 CRUD + 协议自动识别 + 模型同步 + 能力探测 + 多 key 轮换/冷却 + 渠道/模型管理页）与 **M3 协议转换·非流式**（IR + OpenAI 入口/适配器 + 路由分发 + key 冷却接线 + `GET /v1/models`，NextChat 非流式可对话）。下一步是 M4 协议转换·流式+全入口。里程碑计划见 `REQUIREMENTS.md` 第 5 节。
- 仓库名 `clean-api`，但 Go module 名是 `api-gateway`（对应 REQUIREMENTS 里的项目根目录名）。
- `REQUIREMENTS.md`（中文）是唯一且权威的需求/设计文档：含目录结构、SQLite 数据模型、IR 设计、里程碑 M1–M6 等。改动需求前先看它。
- `prototype.html` 是管理后台的**静态原型**（Pico.css + 前端 JS 切页）。它只是设计参考，**不是生产模板**：正式方案是服务端渲染 `html/template`，不前后端分离、无构建工具（见 REQUIREMENTS 2.5）。不要照搬原型里的 SPA 式 `data-page` 切页写法。

## 已实现代码结构

- `cmd/server/main.go`：入口，加载配置、建库、首启建管理员、建 crypto/channel 管理器、挂路由。
- `internal/config`：`config.json` + `GATEWAY_*` 环境变量覆盖；`session_secure`（明文 HTTP 必须为 false）。
- `internal/store`：`database/sql` + `modernc.org/sqlite`，users/tokens/channels/channel_keys/models 的 CRUD 已齐（request_logs 表已建，M5 用）。
- `internal/auth`：bcrypt、token 生成(32B Base64)/sha256、session（Secure 属性可配）、`APIAuth` 中间件、`CheckModelAllowed`。
- `internal/crypto`：上游 API key 的 AES-GCM 加解密，无 `GATEWAY_ENC_KEY` 时明文降级；密文带 `enc:` 前缀。
- `internal/channel`：协议自动识别（GET /v1/models→openai、POST /v1/messages→anthropic、POST /v1/responses→responses）、模型列表同步、能力探测（system/tools/vision/json_mode，最小试调用）、多 key 轮换（random/round_robin）+ 冷却、异步探测进度（内存态，管理页轮询，`ProbeStatus`）。
- `internal/protocol`：IR 定义（`ChatRequest`/`Message`/`ChatResponse` 等，按 REQUIREMENTS §2.3.2）+ OpenAI Chat 解析/序列化（`ParseOpenAIChatRequest`、`SerializeOpenAIChatRequest`、`ParseOpenAIChatResponse`、`SerializeOpenAIChatResponse`）。M3 只覆盖非流式；Responses/Anthropic 与 SSE 是 M4。
- `internal/upstream`：`Upstream` 接口（M3 仅 `Chat`）+ `OpenAIAdapter`（IR→OpenAI→IR，错误分类：5xx/网络 `Retryable=true`，429/401 保留状态码供 key 冷却，4xx 透传）。错误统一为 `*upstream.Error{StatusCode,Type,Message,Retryable}`。
- `internal/router`：模型→渠道路由（`ListChannelsByModel` 按 name/alias 命中、模型启用、渠道 active）、全局策略 `routing_strategy`（random/round_robin，来自配置）、5xx/网络错误换渠道重试 1 轮（最多 4 次尝试）、429/401 标记 key 冷却换 key、4xx 直接透传；非 openai 类型渠道返回 501（M4 支持）。
- `internal/api`：`/v1/chat/completions`（鉴权→IR 解析→白名单→`router.Chat`→序列化；`stream=true` 返回 501）+ `GET /v1/models`（启用模型对外名列表，alias 非空用 alias、去重）。
- `internal/web` + 根目录 `web/`（embed FS）：登录/仪表盘/令牌/**渠道**/**模型**页（渠道页探测中带 meta 5s 自动刷新），Pico.css 内嵌。

## M2 关键决策

- 渠道 `type`：创建时默认 `auto`（自动探测）；探测成功后写回实际类型；失败保留 auto 供手动指定类型重试（编辑时改类型/base_url 自动触发重探测）。
- 能力探测对每个模型发 4 次最小调用（max_tokens=1），2xx 视为支持；结果可手动覆盖（`capability_override` 记录覆盖字段）。
- key 只存加密值；页面展示掩码（前 4 + … + 后 4）；编辑渠道时填新 key 才替换，留空保留。
- gorilla/sessions v1.4.0 默认 `Secure=true`，明文 HTTP 下 cookie 不生效——已加 `session_secure` 配置默认关。

## M3 关键决策

- 请求/响应都走完整 IR 往返（入口协议 → IR → 上游序列化 → 解析回 IR → 出口序列化），openai→openai 也解析再序列化，为 M4 三协议互转打底；`content` 单文本分片退化为字符串、多分片保持数组，保证保真。
- 路由错误统一用 `*upstream.Error`：4xx 直接透传（含状态码与上游 message）；429/401 触发 key 冷却换 key；5xx/网络错误换渠道重试 1 轮（渠道序列跑两遍，单渠道则重试同一家），总尝试上限 4 次；网络错误（StatusCode=0）对外映射 502。
- 模型→渠道选择策略是**全局配置** `routing_strategy`（random 默认 / round_robin 按模型名计数轮换）；渠道级 `balance_strategy` 只负责渠道内多 key 轮换。渠道权重 priority 留到 M5。
- 路由查询 `ListChannelsByModel` 按 `models.name` 或 `alias` 命中（空 alias 不会误命中），模型启用且渠道 active 才可路由；请求到达上游前把对外名换成渠道内真实模型名。
- 对外名（含 alias 后的名字）是令牌白名单的校验对象；`GET /v1/models` 返回启用模型的对外名列表（alias 优先、去重）。

## 语言约定

- 仓库文档、需求、UI 文案均为中文。新增/修改文档、注释、UI 文案默认用中文，与现状一致。模板注释也是中文。

## 已定技术约束（实现时必须遵守，均来自 REQUIREMENTS.md）

- Go 单二进制；路由用 chi；SQLite 用 `modernc.org/sqlite`（纯 Go、无 CGO）。
- 管理面：`html/template` + **Pico.css**（单 CSS 引入），禁止引入构建工具 / node_modules / 前端框架。
- 无计费逻辑（砍掉余额/充值/账单）。令牌白名单为必填项；「允许全部模型」必须显式勾选，默认关。
- 上游 API key 用 AES-GCM 加密存储（密钥来自环境变量，缺省明文+启动警告）。
- 对外入口三协议：OpenAI Chat / Responses / Anthropic Messages；内部统一转 IR 再按上游序列化。
- 按里程碑推进，M1–M3 先做「OpenAI 入口 → OpenAI 兼容上游」的最小闭环，M4 才做三协议互转。

## 验证命令

- 已有 `Makefile`：`make build` / `make test` / `make run` / `make vet` / `make fmt`（Windows 无 make 时用 `go build ./...`、`go test ./...`、`go vet ./...`、`gofmt -w .`；`fmt` 会直接改写文件）。
- 关键逻辑已有单测：auth/store/crypto/channel（协议探测用 httptest 模拟上游）+ protocol（解析/序列化往返）+ upstream（错误分类）+ router（key 冷却换 key、5xx 换渠道、4xx 透传、alias 路由、round_robin 轮换）+ api 鉴权链与完整闭环 + web 集成测试（登录→添加渠道→自动探测→模型落库）。
- 注意：本机 `GOPROXY` 已切到 `https://goproxy.cn,direct`（默认 proxy 直连超时）；`go mod tidy` / `go get` 新增依赖时保持该代理。
