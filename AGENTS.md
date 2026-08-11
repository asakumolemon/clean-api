# AGENTS.md

## 项目状态

- **全部里程碑 M1–M6 已完成**：M1 骨架 → M2 渠道+探测 → M3 协议转换·非流式 → M4 流式+全入口 → M5 管理面完善 → M6 打磨（panic 恢复、探测证据、key 冷却可配、密钥缺失告警、版本信息、Dockerfile、README、依赖整理）。里程碑计划见 `REQUIREMENTS.md` 第 5 节。
- **M6 后有未入里程碑的修复**（见「M6 后修复决策」）：tool_choice 归一化（下游用 Responses/Anthropic 协议接旧版 OpenAI 兼容上游时的兼容适配）、空对话拦截、失败/流式成功请求日志渠道 ID 与落库补齐。
- 仓库名 `clean-api`，但 Go module 名是 `api-gateway`（对应 REQUIREMENTS 里的项目根目录名）。
- `REQUIREMENTS.md`（中文）是唯一且权威的需求/设计文档：含目录结构、SQLite 数据模型、IR 设计、里程碑 M1–M6 等。改动需求前先看它。
- `prototype.html` 是管理后台的**静态原型**（Pico.css + 前端 JS 切页）。它只是设计参考，**不是生产模板**：正式方案是服务端渲染 `html/template`，不前后端分离、无构建工具（见 REQUIREMENTS 2.5）。不要照搬原型里的 SPA 式 `data-page` 切页写法。

## 已实现代码结构

- `cmd/server/main.go`：入口，加载配置、建库、首启建管理员、建 crypto/channel/router 管理器、挂路由；`recoverMW` panic 恢复中间件（500 JSON + 堆栈日志）；`-version` flag（`-ldflags "-X main.version=..."` 注入）；启动时检测「库中有加密 key 但未配 GATEWAY_ENC_KEY」并告警。
- `internal/config`：`config.json` + `GATEWAY_*` 环境变量覆盖；`session_secure`（明文 HTTP 必须为 false）。
- `internal/store`：`database/sql` + `modernc.org/sqlite`，users/tokens/channels/channel_keys/models/request_logs 的 CRUD 已齐。M5 新增：`logs.go`（日志 CRUD/筛选/分页/清理，`LogFilter{Model,Token,Status,UserID}`）、`export.go`（`ExportAll`/`ImportAll` 全量导出与替换式导入，独立 DTO 字段小写）、users 扩展（改角色/重置密码/删除级联删令牌）。
- `internal/auth`：bcrypt、token 生成(32B Base64)/sha256、session（Secure 属性可配）、`APIAuth` 中间件、`CheckModelAllowed`。
- `internal/crypto`：上游 API key 的 AES-GCM 加解密，无 `GATEWAY_ENC_KEY` 时明文降级；密文带 `enc:` 前缀。
- `internal/channel`：协议自动识别（GET /v1/models→openai、POST /v1/messages→anthropic、POST /v1/responses→responses；**探测失败时错误信息带各协议证据**：状态码/网络错误+响应体截断，M6）、模型列表同步、**能力标注（默认不探测**：`probe_capabilities` 配置或渠道页「探测能力」按钮开启；默认写入保守值 system/tools 开、vision/json_mode 关，模型管理页可手动调整——避免免费模型配额被 100 次探测打爆）、多 key 轮换（random/round_robin）+ 冷却（时长可配 `SetCooldown`，默认 60s）、异步探测进度（内存态，管理页轮询，`ProbeStatus`，`StartCapabilitiesProbe`/`ProbeCapabilitiesOnly` 供手动触发）。M5 新增 `health.go`：`HealthChecker` 定时巡检 active/down 渠道（经 Manager 取真实 key 发最小请求，避免假 key 401 误判），连续失败 N 次→down、成功 1 次→恢复 active。
- `internal/protocol`：IR 定义（`ChatRequest`/`Message`/`ChatResponse`/`StreamEvent` 等，按 REQUIREMENTS §2.3.2）+ 三协议解析/序列化/流式翻译：
  - `openai.go`：Chat 入口解析与出口序列化 + `OpenAIStreamParser`（上游流解析，tool_calls 按 index 累积）+ `OpenAIChatStreamWriter`（出口流编码，`[DONE]` 收尾）；**`normalizeToolChoice`（序列化层 tool_choice 归一化，M6 后修复）**：把各入口透传的新版对象式 tool_choice 转为 OpenAI 旧版兼容格式（`{"type":"auto"}`→`"auto"`、Anthropic `{"type":"tool","name":X}`→`{"type":"function","function":{"name":X}}`），解决只认旧格式的上游（OpencodeGo/Console Go 等）400 unknown variant；
  - `anthropic.go`：Messages 入口解析（system 多形态/tool_use/tool_result/image）与出口序列化（tool_use 块、stop_reason 映射）+ `AnthropicStreamWriter`（message_start/content_block_start/delta/stop/message_delta/message_stop 状态机）；
  - `responses.go`：Responses 入口解析（instructions/input 条目）与出口序列化（output 条目）+ `ResponsesStreamWriter`（output_text.delta/function_call_arguments.delta/response.completed，无 `[DONE]`）。
- `internal/upstream`：`Upstream` 接口（`Chat` + `ChatStream`；Models/Ping 为后续扩展点）+ `OpenAIAdapter`（IR→OpenAI→IR，流式 SSE 逐行解析，单行上限 1MB）。错误统一为 `*upstream.Error{StatusCode,Type,Message,Retryable}`：5xx/网络 `Retryable=true`，429/401 保留状态码供 key 冷却，4xx 透传。
- `internal/router`：模型→渠道路由（`ListChannelsByModel` 按 name/alias 命中、模型启用、渠道 active、带能力）、全局策略 `routing_strategy`（random/round_robin）、**全局模型重定向** `model_redirects`（请求名→实际名，M5）、5xx/网络错误换渠道重试 1 轮（最多 4 次尝试）、429/401 标记 key 冷却换 key、4xx 直接透传；`Chat` 返回 `*ChatResult{Resp, ChannelID}`、`ChatStream` 返回 `(channelID, error)`（请求日志用）；**错误路径也携带最后尝试渠道 ID（M6 后修复，此前一律返回 0/nil 导致失败日志 channel_id=0）**；`ChatStream` 重试只在**首个事件 emit 前**（已输出即中断）；上游不支持 system 时自动折叠 system 进首条 user（`protocol.FoldSystemIntoUser`）；非 openai 类型渠道返回 501（上游适配器留后续）。
- `internal/api`：三入口 handler（`/v1/chat/completions`、`/v1/responses`、`/v1/messages`）共用 `handle` 流水线（鉴权→入口解析→白名单→**空对话拦截**→router→出口序列化/流式编码；空对话拦截在**白名单之后**：`len(req.Messages)==0` 直接 400，不透传 `"messages":[]` 给上游，M6 后修复）；每请求生成 `X-Request-Id` 并**异步写请求日志**（`logRequest`，失败可丢，符合 §2.6；**流式成功路径也会落库**，M6 后修复此前完全不打日志）；流式 SSE 头在**首个事件时**才 WriteHeader 200，之前的路由错误仍返回正常 HTTP 错误；`GET /v1/models`（启用模型对外名列表，alias 非空用 alias、去重）。错误格式：OpenAI/Responses 用 `{error:{message,type}}`，Anthropic 用 `{type:"error",error:{...}}`（类型按状态码映射）。
- `internal/web` + 根目录 `web/`（embed FS）：登录/仪表盘（含最近请求）/令牌/**渠道**/**模型**/请求日志/测试台/用户管理/导入导出页（渠道页探测中带 meta 5s 自动刷新），样式为 **Tailwind CSS Play CDN**（`base.html` 引入脚本，运行时编译，无构建）。模板注册了 `add`/`sub` 函数（分页用）。`web.New(st, sessions, chm, router, version)` 依赖 router（测试台用），version 显示在侧栏。
- `README.md`：用户文档（快速开始/配置表/客户端接入/常见问题）；`Dockerfile` 多阶段构建（CGO_ENABLED=0 + alpine，HEALTHCHECK 探测 /admin/login）；Makefile 含 `docker-build`/`docker-run`。

## M6 后修复决策（上游兼容 + 日志）

- **tool_choice 归一化放序列化层**（`protocol.normalizeToolChoice`，`SerializeOpenAIChatRequest` 内调用）而非 router `prepareReq`：它是「上游格式」问题，放序列化层是三入口（OpenAI/Responses/Anthropic）+ 流式/非流式的唯一咽喉，OpenAI 入口用新版对象式 SDK 也一并受益。触发场景：下游用 Responses/Anthropic 协议接旧版 OpenAI Chat 兼容上游（OpencodeGo/Console Go 等），Anthropic 客户端发的 `{"type":"auto"}` 透传后上游 400 `unknown variant "auto"`。
- **空对话拦截在白名单之后**（`handle` 流水线，非解析器内）：避免把鉴权语义让位给校验错误（白名单外模型仍先 403）；空 messages/input 无法补全，入口直接 400 比透传 `"messages":[]` 给上游收到难懂的 400 更清晰。
- **失败请求也要记渠道**：`Chat` 错误时返回 `&ChatResult{ChannelID: lastChannelID}`（不返回 nil）、`ChatStream` 错误返回 `lastChannelID`；api 层错误日志用返回值而非硬编码 0。此前失败日志 channel_id 全为 0（成功路径一直正确）。
- **流式成功也写请求日志**：`handleStream` 在 `Finish()` 后补成功日志，用量取自 done 事件（emit 回调里缓存 `ev.Usage`）。此前流式成功完全不打日志，违反 §2.6 每请求记录。

## M2 关键决策

- 渠道 `type`：创建时默认 `auto`（自动探测）；探测成功后写回实际类型；失败保留 auto 供手动指定类型重试（编辑时改类型/base_url 自动触发重探测）。
- **能力探测默认关闭**（M6 起）：拉完模型列表直接以保守默认值入库（system/tools 开、vision/json_mode 关），模型管理页手动勾选调整；需要自动探测时配置 `probe_capabilities: true` 或渠道页「探测能力」按钮（`StartCapabilitiesProbe`）。原因：免费模型 RPM 仅 2~60，25 模型 × 4 项 ≈ 100 次探测会直接打爆配额。
- key 只存加密值；页面展示掩码（前 4 + … + 后 4）；编辑渠道时填新 key 才替换，留空保留。
- gorilla/sessions v1.4.0 默认 `Secure=true`，明文 HTTP 下 cookie 不生效——已加 `session_secure` 配置默认关。

## M6 关键决策

- **panic 恢复**：自定义 `recoverMW`（不用 chi 自带 Recoverer，它只返回纯文本）——堆栈记日志，未写头时返回 OpenAI 格式 500 JSON。
- **探测证据**：`Detect` 失败时错误信息 = 各协议证据拼接（`openai: HTTP 404: <body 截断>；anthropic: HTTP 404: 路径不存在…`），直接展示在渠道页探测状态；anthropic/responses 探测语义保持「非 404/405 即端点存在即命中」（500 也命中，因为探测请求参数可能不符）。
- **key 冷却**：`Manager.SetCooldown` 可配（config `key_cooldown_seconds`，默认 60），不改 New 签名避免大范围改动。
- **密钥告警**：启动时 `CountEncryptedKeys`（`key_enc LIKE 'enc:%'`）> 0 且未配 `GATEWAY_ENC_KEY` → ERROR 级告警（防静默明文降级）。
- **版本**：`main.version` ldflags 注入（Dockerfile ARG VERSION）；web 侧栏 `v{{.Version}}` 由 `render` 统一注入。
- 依赖标记用 `go mod tidy` 修正过（chi/sessions/sqlite 是直接依赖）。

## M5 关键决策

- **请求日志**：api 层统一写（`X-Request-Id` + 异步 `logRequest` goroutine，失败忽略不影响主流程）；`router.Chat` 改为返回 `*ChatResult{Resp, ChannelID}`、`ChatStream` 返回 `(channelID, error)` 以记录命中渠道；日志页筛选（模型模糊/令牌精确/状态段）+ 分页；保留清理（启动时 + 每小时，`log_retention_days`）。
- **健康检查**：只巡检 active/down 且已识别类型的渠道；必须经 `Manager.SelectKey` 取**真实 key** 发请求（假 key 会被上游 401 误判 down）；连续失败 N 次→down（路由自动绕开），成功 1 次→恢复 active。
- **导入导出**：全量 JSON（users/tokens/channels/channel_keys/models），独立 DTO 字段小写；导入为替换式（单事务清空重建，保留原 id，失败整体回滚）；令牌只含 hash 明文不可恢复、key 为密文需同一 `GATEWAY_ENC_KEY`——页面有注明。
- **全局重定向** `model_redirects`：api 层白名单校验用原始对外名，router 层查映射替换后再路由（与渠道内 alias 互补）。
- **用户管理**：删除用户先删其全部令牌（tokens 外键 REFERENCES users 已开启）；禁止删除当前登录账号；`adminOnly` 中间件每次回库读角色，改角色即时生效。
- 管理面仍为 admin-only（用户角色仅 API 使用，已确认不做分级）。

## M4 关键决策

- **统一事件流**：流式不写 9 个组合适配器，而是「上游侧 1 个解析器（SSE→`StreamEvent`）+ 出口侧每协议 1 个编码器（`StreamEvent`→SSE）」。SSE 手写 bufio 逐行（`data:`/`event:`），不引第三方库。
- **工具调用增量三事件**（`tool_call_start`/`tool_call_delta`/`tool_call_stop`）：Anthropic 客户端自己拼装增量，必须收到 content_block_start → input_json_delta → content_block_stop 序列；OpenAI 上游的 tool_calls 按 index 累积，finish_reason/usage 缓存到 `[DONE]` 时随 done 事件发出。
- **流式重试语义**：重试只发生在首个事件 emit 前（429/401 换 key、5xx 换渠道）；一旦开始输出，任何错误直接中断并写协议 error 事件，避免客户端收到重复数据。
- **SSE 头延迟提交**：`handleStream` 只在首个事件时才 WriteHeader 200——首事件前的路由错误（模型不存在/4xx）仍能返回正常 HTTP 错误状态码。
- 每个出口流编码器有 `Finish()`（幂等收尾）：上游未发 `[DONE]` 时补发结束事件（OpenAI `[DONE]` / Anthropic message_delta+message_stop / Responses response.completed），防客户端挂起。
- system 折叠按**渠道模型能力**（`ModelRoute.Caps`）决定：`capabilities.system=false` 时 `FoldSystemIntoUser` 把 system 文本拼进首条 user 并移除 system 消息。
- Anthropic 错误格式 `{type:"error",error:{type,message}}`，错误类型按状态码映射（404→not_found_error 等）；OpenAI/Responses 用 `{error:{message,type}}`。

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
- 管理面：`html/template` + **Tailwind CSS**（Play CDN `cdn.tailwindcss.com` 运行时编译；自定义样式放 `base.html` 的 `text/tailwindcss` 块用 `@apply`）。禁止引入构建工具 / node_modules / 前端框架；页面需联网才能加载样式，离线环境请换方案。
- 无计费逻辑（砍掉余额/充值/账单）。令牌白名单为必填项；「允许全部模型」必须显式勾选，默认关。
- 上游 API key 用 AES-GCM 加密存储（密钥来自环境变量，缺省明文+启动警告）。
- 对外入口三协议：OpenAI Chat / Responses / Anthropic Messages；内部统一转 IR 再按上游序列化。
- 按里程碑推进，M1–M3 先做「OpenAI 入口 → OpenAI 兼容上游」的最小闭环，M4 才做三协议互转。

## 验证命令

- 已有 `Makefile`：`make build` / `make test` / `make run` / `make vet` / `make fmt`（Windows 无 make 时用 `go build ./...`、`go test ./...`、`go vet ./...`、`gofmt -w .`；`fmt` 会直接改写文件）。
- 关键逻辑已有单测：auth/store/crypto/channel（协议探测用 httptest 模拟上游）+ protocol（三协议解析/序列化往返、流解析/流编码事件序列、**tool_choice 归一化各形态**）+ upstream（错误分类、流式多 chunk/工具调用）+ router（key 冷却换 key、5xx 换渠道、4xx 透传、alias 路由、round_robin 轮换、流式 emit 前重试/emit 后不重试、system 折叠、**错误路径携带渠道 ID**）+ api 三入口集成（OpenAI/Anthropic/Responses 流式与非流式、错误格式、**Anthropic tool_choice 归一化端到端、空 messages/空 input 400、流式成功日志**）+ web 集成测试（登录→添加渠道→自动探测→模型落库）。
- 注意：本机 `GOPROXY` 已切到 `https://goproxy.cn,direct`（默认 proxy 直连超时）；`go mod tidy` / `go get` 新增依赖时保持该代理。
