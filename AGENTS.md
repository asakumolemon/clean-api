# AGENTS.md

## 项目状态

- **全部里程碑 M1–M6 已完成**：M1 骨架 → M2 渠道+探测 → M3 协议转换·非流式 → M4 流式+全入口 → M5 管理面完善 → M6 打磨（panic 恢复、探测证据、key 冷却可配、密钥缺失告警、版本信息、Dockerfile、README、依赖整理）。里程碑计划见 `REQUIREMENTS.md` 第 5 节。
- **M6 后有未入里程碑的修复**（见「M6 后修复决策」）：tool_choice 归一化（下游用 Responses/Anthropic 协议接旧版 OpenAI 兼容上游时的兼容适配）、空对话拦截、失败/流式成功请求日志渠道 ID 与落库补齐。
- **M7（新增，未入 REQUIREMENTS 里程碑表）**：**Anthropic Messages 原生上游支持**——新增 `AnthropicAdapter`（上游协议从「仅 OpenAI Chat 兼容」扩展为「OpenAI + Anthropic 兼容」）；router 按渠道类型分派适配器；health 检查 anthropic 渠道走 POST /v1/messages。Responses 原生上游与厂商类型仍 501。
- 仓库名 `clean-api`，但 Go module 名是 `api-gateway`（对应 REQUIREMENTS 里的项目根目录名）。
- `REQUIREMENTS.md`（中文）是唯一且权威的需求/设计文档：含目录结构、SQLite 数据模型、IR 设计、里程碑 M1–M6 等。改动需求前先看它。
- `prototype.html` 是管理后台的**静态原型**（Pico.css + 前端 JS 切页）。它只是设计参考，**不是生产模板**：正式方案是服务端渲染 `html/template`，不前后端分离、无构建工具（见 REQUIREMENTS 2.5）。不要照搬原型里的 SPA 式 `data-page` 切页写法。

## 已实现代码结构

- `cmd/server/main.go`：入口，加载配置、建库、首启建管理员、建 crypto/channel/router 管理器、挂路由；`recoverMW` panic 恢复中间件（500 JSON + 堆栈日志）；`-version` flag（`-ldflags "-X main.version=..."` 注入）；启动时检测「库中有加密 key 但未配 GATEWAY_ENC_KEY」并告警。
- `internal/config`：`config.json` + `GATEWAY_*` 环境变量覆盖；`session_secure`（明文 HTTP 必须为 false）。
- `internal/store`：`database/sql` + `modernc.org/sqlite`，users/tokens/channels/channel_keys/models/request_logs 的 CRUD 已齐。M5 新增：`logs.go`（日志 CRUD/筛选/分页/清理，`LogFilter{Model,Token,Status,UserID}`）、`export.go`（`ExportAll`/`ImportAll` 全量导出与替换式导入，独立 DTO 字段小写）、users 扩展（改角色/重置密码/删除级联删令牌）。
- `internal/auth`：bcrypt、token 生成(32B Base64)/sha256、session（Secure 属性可配）、`APIAuth` 中间件（兼容 `Authorization: Bearer` 与 Anthropic 系客户端的 `x-api-key` 两种令牌头）、`CheckModelAllowed`。
- `internal/crypto`：上游 API key 的 AES-GCM 加解密，无 `GATEWAY_ENC_KEY` 时明文降级；密文带 `enc:` 前缀。
- `internal/channel`：协议自动识别（GET /v1/models→openai、POST /v1/messages→anthropic、POST /v1/responses→responses；**探测失败时错误信息带各协议证据**：状态码/网络错误+响应体截断，M6）、模型列表同步、**能力标注（默认不探测**：`probe_capabilities` 配置或渠道页「探测能力」按钮开启；默认写入保守值 system/tools 开、vision/json_mode 关，模型管理页可手动调整——避免免费模型配额被 100 次探测打爆）、多 key 轮换（random/round_robin）+ 冷却（时长可配 `SetCooldown`，默认 60s）、异步探测进度（内存态，管理页轮询，`ProbeStatus`，`StartCapabilitiesProbe`/`ProbeCapabilitiesOnly` 供手动触发）。M5 新增 `health.go`：`HealthChecker` 定时巡检 active/down 渠道（经 Manager 取真实 key 发最小请求，避免假 key 401 误判），连续失败 N 次→down、成功 1 次→恢复 active。
- `internal/protocol`：IR 定义（`ChatRequest`/`Message`/`ChatResponse`/`StreamEvent` 等，按 REQUIREMENTS §2.3.2）+ 三协议解析/序列化/流式翻译：
  - `openai.go`：Chat 入口解析与出口序列化 + `OpenAIStreamParser`（上游流解析，tool_calls 按 index 累积）+ `OpenAIChatStreamWriter`（出口流编码，`[DONE]` 收尾）；**`normalizeToolChoice`（序列化层 tool_choice 归一化，M6 后修复）**：把各入口透传的新版对象式 tool_choice 转为 OpenAI 旧版兼容格式（`{"type":"auto"}`→`"auto"`、Anthropic `{"type":"tool","name":X}`→`{"type":"function","function":{"name":X}}`），解决只认旧格式的上游（OpencodeGo/Console Go 等）400 unknown variant；
  - `anthropic.go`：Messages 入口解析（system 多形态/tool_use/tool_result/image）与出口序列化（tool_use 块、stop_reason 映射）+ `AnthropicStreamWriter`（message_start/content_block_start/delta/stop/message_delta/message_stop 状态机）；**上游侧（M7）：`SerializeAnthropicMessagesRequest`（IR→Anthropic 请求体，system 提取、tool_result 嵌 user、max_tokens 缺省 1024）+ `ParseAnthropicMessagesResponse`（→IR，stop_reason/usage 映射）+ `AnthropicStreamParser`（SSE→StreamEvent，text_delta/input_json_delta 增量、message_stop→done）**；
  - `responses.go`：Responses 入口解析（instructions/input 条目）与出口序列化（output 条目）+ `ResponsesStreamWriter`（output_text.delta/function_call_arguments.delta/response.completed，无 `[DONE]`）。
- `internal/upstream`：`Upstream` 接口（`Chat` + `ChatStream`；Models/Ping 为后续扩展点）+ `OpenAIAdapter`（IR→OpenAI→IR，流式 SSE 逐行解析，单行上限 1MB）+ **`AnthropicAdapter`**（IR→Anthropic Messages→IR，流式 `AnthropicStreamParser` 处理 message_start/content_block_*/message_delta/message_stop，无 `[DONE]`）。错误统一为 `*upstream.Error{StatusCode,Type,Message,Retryable}`：5xx/网络 `Retryable=true`，429/401 保留状态码供 key 冷却，4xx 透传。
- `internal/router`：模型→渠道路由（`ListChannelsByModel` 按 name/alias 命中、模型启用、渠道 active、带能力）、全局策略 `routing_strategy`（random/round_robin）、**全局模型重定向** `model_redirects`（请求名→实际名，M5）、5xx/网络错误换渠道重试 1 轮（最多 4 次尝试）、429/401 标记 key 冷却换 key、4xx 直接透传；`Chat` 返回 `*ChatResult{Resp, ChannelID}`、`ChatStream` 返回 `(channelID, error)`（请求日志用）；**错误路径也携带最后尝试渠道 ID（M6 后修复，此前一律返回 0/nil 导致失败日志 channel_id=0）**；`ChatStream` 重试只在**首个事件 emit 前**（已输出即中断）；上游不支持 system 时自动折叠 system 进首条 user（`protocol.FoldSystemIntoUser`）；**`newAdapter` 按渠道类型分派适配器（M7）：openai→OpenAIAdapter、anthropic→AnthropicAdapter，未知类型（responses 原生/厂商类型）→ 501**。
- `internal/api`：三入口 handler（`/v1/chat/completions`、`/v1/responses`、`/v1/messages`）共用 `handle` 流水线（鉴权→入口解析→白名单→**空对话拦截**→router→出口序列化/流式编码；空对话拦截在**白名单之后**：`len(req.Messages)==0` 直接 400，不透传 `"messages":[]` 给上游，M6 后修复）；每请求生成 `X-Request-Id` 并**异步写请求日志**（`logRequest`，失败可丢，符合 §2.6；**流式成功路径也会落库**，M6 后修复此前完全不打日志）；流式 SSE 头在**首个事件时**才 WriteHeader 200，之前的路由错误仍返回正常 HTTP 错误；`GET /v1/models`（启用模型对外名列表，alias 非空用 alias、去重）。错误格式：OpenAI/Responses 用 `{error:{message,type}}`，Anthropic 用 `{type:"error",error:{...}}`（类型按状态码映射）。
- `internal/web` + 根目录 `web/`（embed FS）：登录/仪表盘（含最近请求）/令牌/**渠道**/**模型**/请求日志/测试台/用户管理/导入导出页（渠道页探测中带 meta 5s 自动刷新），样式为 **Tailwind CSS Play CDN**（`base.html` 引入脚本，运行时编译，无构建）。模板注册了 `add`/`sub` 函数（分页用）。`web.New(st, sessions, chm, router, version)` 依赖 router（测试台用），version 显示在侧栏。
- `README.md`：用户文档（快速开始/配置表/客户端接入/常见问题）；`Dockerfile` 多阶段构建（CGO_ENABLED=0 + alpine，HEALTHCHECK 探测 /admin/login）；Makefile 含 `docker-build`/`docker-run`。

## M6 后修复决策（上游兼容 + 日志）

- **tool_choice 归一化放序列化层**（`protocol.normalizeToolChoice`，`SerializeOpenAIChatRequest` 内调用）而非 router `prepareReq`：它是「上游格式」问题，放序列化层是三入口（OpenAI/Responses/Anthropic）+ 流式/非流式的唯一咽喉，OpenAI 入口用新版对象式 SDK 也一并受益。触发场景：下游用 Responses/Anthropic 协议接旧版 OpenAI Chat 兼容上游（OpencodeGo/Console Go 等），Anthropic 客户端发的 `{"type":"auto"}` 透传后上游 400 `unknown variant "auto"`。
- **空对话拦截在白名单之后**（`handle` 流水线，非解析器内）：避免把鉴权语义让位给校验错误（白名单外模型仍先 403）；空 messages/input 无法补全，入口直接 400 比透传 `"messages":[]` 给上游收到难懂的 400 更清晰。
- **失败请求也要记渠道**：`Chat` 错误时返回 `&ChatResult{ChannelID: lastChannelID}`（不返回 nil）、`ChatStream` 错误返回 `lastChannelID`；api 层错误日志用返回值而非硬编码 0。此前失败日志 channel_id 全为 0（成功路径一直正确）。
- **流式成功也写请求日志**：`handleStream` 在 `Finish()` 后补成功日志，用量取自 done 事件（emit 回调里缓存 `ev.Usage`）。此前流式成功完全不打日志，违反 §2.6 每请求记录。

## M7 关键决策（Anthropic 原生上游）

- **上游适配器按渠道类型分派**：router 新增 `newAdapter(chType, ...)`——`openai`→`OpenAIAdapter`、`anthropic`→`AnthropicAdapter`；`supportedChannelType` 只认 openai/anthropic/空，**responses 原生上游与厂商类型仍 501**（不做的事别静默走错适配器，避免 OpenAI 格式打到 Anthropic 端点产生难懂的 404/502）。`prepareReq` 与 IR 无关，协议适配全部在适配器内。
- **Anthropic 强制 max_tokens**：`SerializeAnthropicMessagesRequest` 在 IR `MaxTokens` 为 nil 时给默认 1024（下游 Anthropic 入口解析已强制该校验，但 OpenAI/Responses 入口不带）。
- **Anthropic 消息模型差异在序列化层转换**：system 从 Messages 提取到顶层 `system` 字段；`role=tool` 消息 → user 内的 `tool_result` 块（Anthropic 无独立 tool 角色）；assistant ToolCalls → `tool_use` 块；tool_choice 归一化（`"auto"→{"type":"auto"}`、`"required"→{"type":"any"}`、OpenAI 函数对象→`{"type":"tool","name":X}`）。
- **Anthropic 流式无 `[DONE]`**：`AnthropicStreamParser` 以 `message_stop` 收尾发 done 事件；`stop_reason`/`usage` 在 `message_delta` 缓存（与 OpenAI 的 finish_reason/usage 缓存到 `[DONE]` 同理）；`content_block_start` 按 index 记录 tool 块，`input_json_delta` 只对 tool 块发 delta 事件。
- **健康检查按协议分派**：anthropic 渠道 `ping` 发 `POST /v1/messages`（模型名取渠道内启用模型，占位名会 400 误判）；openai/responses 保持 `GET /v1/models`。
- **Anthropic 模型列表仍走 `GET /v1/models` + x-api-key 头**：Anthropic 原生 API 无该端点时需渠道页手动添加模型（现状行为，README 已注明）。

## 令牌配置体验优化（M7 后，方案见 docs/token-ux-research.md）

- **白名单弹窗信息透明（方案 B）**：`enabledModelNames` 升级为 `modelOptions` 视图（`internal/web/handlers.go`）——按对外名（alias 优先）去重分组，统计提供渠道总数与健康（active）渠道数（`ListModels` + `ListChannels` 内存聚合，无新 SQL），能力取各渠道**并集**（白名单按名授权，能力只作展示参考）。弹窗每行显示「N 渠道 · M 可用」（0 可用红字「无可用渠道」）+ 能力标签；筛选栏「只看有可用渠道」+ 能力筛选（data-* 属性 + 原生 JS，`applySearch` 三者叠加，隐藏行勾选不受影响）。
- **一键建令牌（方案 A，已改多选批量）**：早期实现为模型管理页每行「建令牌」按钮 → POST `/admin/tokens` 带 `model` 参数（对外名）预填白名单、名称为空自动命名「X 令牌」；因模型页改为多选 + 搜索右侧批量建令牌（见「模型多选建令牌」决策），该每行入口已移除、`createToken` 不再收 `model`。**生成令牌必须同响应 render 展示明文**（不能 302，否则令牌明文丢失——现有 `NewToken` 区块即该模式）。
- **按客户端一键复制（方案 C）**：生成令牌后展示块从单一 curl 扩展为四项：OpenAI Chat curl、Anthropic Messages curl（x-api-key + anthropic-version）、Claude Code 三环境变量 shell、NextChat 三要素，各带独立复制按钮（复用 `copyText`，id 唯一）。
- **测试台下拉去重**：`renderPlayground` 按对外名去重（此前多渠道同模型会重复出现）。
- **模型页搜索**：`modelsPage` 支持 `?q=` 按模型名/别名/渠道名模糊过滤（store 新增 `ListModelsPageFiltered`/`CountModelsFiltered`，JOIN channels 过滤渠道名，排序与 `ListModelsPage` 一致）；搜索与分页叠加，分页链接与操作表单（alias/override/toggle）均带 hidden `q`，`modelListRedirect` 保留搜索词；空结果显示「无匹配模型」。
- **令牌编辑白名单**：令牌列表每行「编辑模型」按钮 → 独立编辑弹窗（`#edit-models-modal`，模型行从新建弹窗 cloneNode 复制、按 `WhitelistJSON` 预勾选，`tokenView` 包装 `WhitelistJSON` 字段）→ POST `/admin/tokens/{id}/whitelist`（`updateTokenWhitelist`，复用与创建一致的校验：空白名单必须显式勾选 allow_all）→ store `UpdateTokenWhitelist`（M5 已有）。编辑弹窗的 checkbox 在独立表单内，避免随新建表单提交；新建弹窗的 `applySearch`/`selected` 选择器限定 `#models-modal` 作用域防止互相干扰。白名单中不在弹窗候选（模型被禁用/删除）的项由 JS 动态追加一行并标注「模型已不在可用列表」，避免编辑保存时静默丢弃。
- **令牌一键复制**：每行「复制」按钮 → POST `/admin/tokens/{id}/copy`（`copyToken`），沿用源令牌的 Whitelist+AllowAll 生成新令牌、归属当前用户、名称空时默认「原名 副本」，**同响应 render 展示新明文**（复用 `NewToken` 区块）。JS 用 `prompt()` 询问新名（默认「原名 副本」）后带 hidden `name` 提交；`openEditModal`/复制处理都挂在 IIFE 内，行内按钮用到的必须先 `window.xxx = ...` 暴露（`openEditModal` 曾漏挂导致 `ReferenceError`）。
- **令牌分组**：tokens 加 `group TEXT DEFAULT ''` 列（`group` 是 SQLite 关键字，**所有 SQL 里必须反引号** `` `group` ``；新库建表带列、老库 `ensureColumn` 幂等补列）。无分组（空）统一展示为「默认分组」，令牌页按分组**分节**（默认分组排最前，其余按名排序；`tokenGroups` 内存分组）。分组编辑：每行「分组」列 + 「改」按钮 → JS `prompt`（默认当前分组）→ POST `/admin/tokens/{id}/group`（`setTokenGroup`，留空归默认）。创建表单带分组输入框（datalist 提示已有分组，不列「默认分组」伪项）；复制令牌沿用源分组。导出/导入 DTO 带 `group`。
- **模型多选建令牌**：模型页每行原生「建令牌」**移除**，改为行首 checkbox 多选 + 搜索按钮右侧「建令牌」按钮（`#batch-create-token`，`type=button` JS 收集勾选 → 动态建表单 POST `/admin/tokens/batch`）；`createTokensFromModels` 对每个选中模型各建一个令牌（whitelist=[该模型]、自动命名「X 令牌」、默认分组），**全部明文同响应逐个展示**（`NewTokens []newTokenView` 循环，多令牌只展示明文+复制，单令牌仍保留按客户端一键复制四块）。选择基于当前页（跨页不做）。`createToken` 不再收 `model` 参数（model 一键直达改由 batch 承接）。
- **时间本地化**：管理面所有时间由原本 UTC 展示改为按 `timezone` 配置（IANA 名，默认服务器本地 `time.Local`，`GATEWAY_TIMEZONE` 可覆盖；`web.New` 加 `tz` 参数 → `Server.loc`）。模板新增 `localtime` 函数（FuncMap 注册，`t.In(loc).Format`），替换 4 处模板 `.Format`（tokens/users/logs/dashboard）+ channels.go 冷却显示改 `s.loc`。日志 `from`/`to` 日期 `ParseInLocation` 按 `s.loc` 解析再转 UTC 比对；`LogUsageStats` 加 `loc` 参数——SQLite 无法解析 Go 时间文本全格式，故 `datetime(substr(ts,1,19), printf('%+d seconds', offsetSec))` 平移后按前 10 位分桶，使按天×模型与日期筛选与展示时区一致（offset 取 loc 当前偏移秒，自托管单时区够用）。

## 响应缓存与命中率（M7 后）

- **范围**：仅缓存**非流式**成功响应（`stream=false`），流式不缓存（记未命中）。命中直接返回缓存响应、**不调上游**，省上游调用；`model_redirects` 为静态映射，同一对外名恒映射同一实际名，用对外名+请求体作 key 安全。
- **介质**：进程内存 + TTL（`cache_ttl_seconds` 默认 300，`cache_enabled` 默认开、可关；`internal/cache` 新包，`sync.Mutex` + map，`maxEntries=5000` 防内存膨胀：超限先清过期，仍超则拒写）。**命中率统计不靠缓存自身计数**——每请求的命中与否写进请求日志，持久化到 SQLite，重启不丢。
- **key**：`sha256(tokenID + 原始请求体)`——按令牌隔离（不同令牌不共享缓存）、参数全量入键；白名单校验在缓存查询**之前**（命中路径不绕过鉴权语义）。
- **命中路径**：`handle` 流水线白名单/空对话校验后、流式分流前查缓存；命中 → 出口序列化（与正常路径同函数）→ 200，日志 `cache_hit=1`、`channel_id=0`（未调上游）、tokens 用缓存 usage；未命中 → 正常路由，`handleNonStream` 成功后写缓存（只缓存成功）。
- **落库**：`request_logs` 新增 `cache_hit INTEGER DEFAULT 0`——新库建表直接带列；**老库 `migrate()` 里 `ensureColumn`（PRAGMA table_info 检测 + ALTER TABLE ADD COLUMN，幂等）补列，项目首个 ALTER 先例**。`LogUsageStats` 每行加 `CacheHits`（SUM），新增 `CountCacheHits(filter)`（复用 `logFilterWhere`）。
- **展示**：日志页汇总卡片「缓存命中率 X%（N 次命中）」+ 按天×模型明细表「缓存命中 N/M」列（随现有筛选联动）；仪表盘第 5 张卡片。命中率 = 命中数/请求数，无请求显示 `-`。
- **配置**：`cache_enabled` 用 `*bool`（区分「未设置→默认 true」与「显式 false」）；`GATEWAY_CACHE_ENABLED`/`GATEWAY_CACHE_TTL_SECONDS` 环境变量。

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
- **角色分级（M5 后新增决策，覆盖旧「admin-only」）**：user 角色可登录后台访问**只读页**（仪表盘 `/`、请求日志 `/logs`、测试台 `/playground`+`/playground/chat`，`authOnly` 中间件）；管理页（令牌/渠道/模型/用户/导入导出）仅 admin（`adminOnly`）。`adminOnly` 对已登录非 admin 返回 302 `/admin/` + flash「无权限」，不跳登录页（避免 user 误以为被登出）；侧边栏导航按 `.Role` 渲染（`render` 统一注入），user 只显示只读导航。

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
- 关键逻辑已有单测：auth/store/crypto/channel（协议探测用 httptest 模拟上游、**anthropic 渠道健康检查 POST /v1/messages**）+ protocol（三协议解析/序列化往返、流解析/流编码事件序列、tool_choice 归一化各形态、**Anthropic 上游序列化/响应解析/流解析器**）+ upstream（错误分类、流式多 chunk/工具调用、**Anthropic 非流式/流式/工具调用事件序列**）+ router（key 冷却换 key、5xx 换渠道、4xx 透传、alias 路由、round_robin 轮换、流式 emit 前重试/emit 后不重试、system 折叠、错误路径携带渠道 ID、**anthropic 渠道分派**）+ api 三入口集成（OpenAI/Anthropic/Responses 流式与非流式、错误格式、Anthropic tool_choice 归一化端到端、空消息 400、流式成功日志、**下游 OpenAI/Anthropic → anthropic 上游端到端**）+ web 集成测试（登录→添加渠道→自动探测→模型落库）。
- 注意：本机 `GOPROXY` 已切到 `https://goproxy.cn,direct`（默认 proxy 直连超时）；`go mod tidy` / `go get` 新增依赖时保持该代理。
