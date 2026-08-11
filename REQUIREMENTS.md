# 智能 API 分发网关 — 需求与设计文档

> 版本：v0.1（需求阶段）
> 日期：2026-08-10
> 技术栈：Go（单二进制）
> 定位：个人/小团队自用的 LLM API 分发网关

---

## 1. 项目概述

### 1.1 一句话描述

填一个 `base_url + api_key`，网关自动探测上游类型、自动拉取模型列表、自动判断模型能力，统一对外提供 OpenAI / Responses / Anthropic 三种协议；带轻量用户令牌访问控制，**不含任何计费逻辑**。

### 1.2 为什么不做 one-api

| one-api | 本项目 |
|---|---|
| 渠道手动填（类型/模型/地址全手写） | 自动探测 + 自动拉模型 |
| 用户 + 令牌 + 计费 + 充值 + 账单 | 用户 + 令牌 + 模型授权，**无计费** |
| 只支持 OpenAI 协议出口 | chat / responses / anthropic 三种协议互转 |
| 依赖 MySQL/Redis，部署重 | SQLite + 单二进制，几十 MB 内存 |

### 1.3 核心设计原则

1. **轻**：单二进制、SQLite、无外部服务依赖
2. **智能**：协议识别、模型同步、能力探测全自动，手动是兜底不是默认
3. **无钱**：砍掉余额/计费/充值，保留访问控制（令牌 + 模型白名单）
4. **透明**：请求日志可视化，出问题一眼定位到上游

---

## 2. 功能需求

### 2.1 用户与令牌（访问控制，无计费）

- 首次启动自动创建管理员账号（从环境变量或启动参数读初始密码）
- 用户角色：`admin` / `user`
  - admin：管理渠道、模型、用户、令牌、查看全部日志；**支持多个管理员**（角色为 admin 即可，无数量限制）
  - user：查看模型列表、使用自己的令牌、查看自己的日志
- 令牌管理：
  - 生成令牌（随机 32 字节，Base64），仅创建时展示一次明文
  - 存储只存哈希（sha256），数据库泄露也不暴露可用 key
  - 吊销 / 启用 / 禁用
  - **模型白名单（必须指定）**：新建令牌时必须指定至少一个模型；不提供隐式全放行。如需全放行，页面上需显式勾选「允许全部模型」选项（默认不勾，创建时明确选择）
- 鉴权流程：
  1. 请求头 `Authorization: Bearer <token>`
  2. 查哈希 → 令牌存在且启用
  3. 校验目标模型在令牌白名单内（白名单必填；仅当创建时显式勾选「允许全部模型」才全部放行）
  4. 通过 → 放行到路由层；不通过 → `403 {"error": "model not allowed"}`（**不发上游请求**）

### 2.2 渠道接入（智能）

添加渠道时只需提供：

```json
{
  "name": "deepseek 主号",
  "base_url": "https://api.deepseek.com",
  "api_keys": ["sk-xxx", "sk-yyy"],
  "type": "auto"          // auto = 自动探测，也可手动指定覆盖
}
```

**自动探测流程**（添加渠道时异步执行，页面显示进度）：

1. **协议识别**（按顺序探测，命中即停）：
   - `GET {base}/v1/models` → 200 且返回模型数组 → **OpenAI 兼容**
   - `POST {base}/v1/messages`（最小请求）→ 200 → **Anthropic**
   - `POST {base}/v1/responses`（最小请求）→ 200 → **Responses**
   - 都不行 → 尝试已知厂商 SDK 特征（讯飞 appid+secret、百度 AK/SK 等）→ 走专用适配器
   - 全部失败 → 标记「探测失败」，允许手动指定类型重试
2. **模型列表同步**：拉取全部可用模型 → 写入 `models` 表 → 管理页展示
3. **能力标注**（默认不探测，省时省配额）：
   - 模型列表同步后能力字段写入**保守默认值**（system / 工具调用默认支持，视觉 / JSON mode 默认关），**用户在模型管理页手动勾选调整**（结果可手动覆盖）
   - 需要自动探测时：配置 `probe_capabilities: true`（添加/重探测渠道时对每个模型发最小试调用），或渠道页「探测能力」按钮手动触发
   - 探测内容：system 支持、function calling、视觉、JSON mode 各发一次最小试调用，2xx 视为支持
   - 说明：默认不做能力探测，避免免费额度模型（RPM 2~60）被 25 模型 × 4 项 ≈ 100 次探测请求打爆配额
4. **多 key 管理**：一个渠道可配多个 key，轮换策略：随机 / 顺序 / 失败切换（某 key 429/401 自动标记冷却，优先用其他 key）

**健康检查**：可选开启，定时（默认 5 分钟）向各渠道发最小探测请求，失败连续 N 次（默认 3）标记 `down`，路由时自动绕开；恢复探测自动回 `up`。

### 2.3 协议转换（核心）

#### 2.3.1 对外入口（三种全支持）

| 路径 | 协议 | 典型客户端 |
|---|---|---|
| `POST /v1/chat/completions` | OpenAI Chat | NextChat / LobeChat / 各类 SDK |
| `POST /v1/responses` | OpenAI Responses | OpenAI SDK 新版 |
| `POST /v1/messages` | Anthropic Messages | Claude Code / Cursor / Anthropic SDK |

#### 2.3.2 统一中间表示（IR）

所有入口协议先解析为 IR，再按上游协议序列化：

```go
type ChatRequest struct {
    Model       string
    Messages    []Message   // role: system/user/assistant/tool
    Tools       []Tool
    ToolChoice  any         // string | object
    Stream      bool
    Temperature *float64
    MaxTokens   *int
    ResponseFormat any      // json_object / json_schema / text
    Images      []ImageInput // 统一为 url 或 base64
}

type Message struct {
    Role    string
    Content []ContentPart // text / image / tool_result
    Name    string
    ToolCalls []ToolCall
}

type ToolCall struct {
    ID       string
    Name     string
    Arguments json.RawMessage
}
```

流式输出统一为内部事件流 `[]StreamEvent{Type, Delta, ToolCall, Done, Usage}`，再翻译成各协议 SSE 格式。

#### 2.3.3 转换对照表

| 能力 | OpenAI Chat | Responses | Anthropic |
|---|---|---|---|
| system 提示词 | `messages[role=system]` | `instructions` | `system` 参数（无则折叠为首条 user） |
| 工具调用 | `tools` (function) | `tools` (function) | `tools` (tool_use) |
| 工具结果 | `role=tool` 消息 | `function_call_output` | `tool_result` block |
| 图片 | `content[type=image_url]` | `input[type=image]` | `content[type=image]` base64 |
| 流式 | `data: {choices[0].delta}` | `data: {type: response.output_text.delta}` | `data: {type: content_block_delta}` |
| 结束 | `finish_reason` | `response.completed` | `message_stop` |
| 用量 | `usage` | `usage` | `usage` |

**不支持 system 的模型**：IR 解析后若上游模型 `capabilities.system == false`，把 system 内容折叠进第一条 user 消息（one-api 已有此思路，照做）。

#### 2.3.4 上游适配器接口

```go
type Upstream interface {
    Chat(ctx, *ChatRequest) (*ChatResponse, error)     // 非流式
    ChatStream(ctx, *ChatRequest, func(StreamEvent) error) error // 流式
    Models() ([]ModelInfo, error)                      // 拉模型列表
    Ping() error                                       // 健康检查
}
```

适配器实现：`openai`（默认）、`anthropic`、`responses`、`xunfei`（讯飞 SDK）、`qianfan`（百度）、`hunyuan`（腾讯）……协议兼容的厂商直接走通用适配器 + `server_url` 覆盖。

### 2.4 分发路由

- **路由目标**：`模型名 → 渠道` 映射
  - 同一模型在多个渠道 → 按策略选择：`random` / `round_robin` / `priority`（渠道可配权重）
  - 渠道 `down` / key 冷却 / 限流 → 自动切换下一可用渠道
- **模型别名**：对外模型名可改（如内部 `ds-main` 对外叫 `deepseek-chat`）；支持全局重定向（所有入站请求的模型名映射）
- **超时与重试**：默认请求超时 120s（可配）；上游 5xx/网络错误重试 1 次换渠道；4xx 不重试直接透传错误

### 2.5 管理面（服务端渲染，不前后端分离）

技术：`html/template` + **Tailwind CSS**（Play CDN 运行时编译，无构建工具、无 node_modules）

| 页面 | 路由 | 功能 |
|---|---|---|
| 登录 | `/admin/login` | 账号密码登录，session cookie |
| 仪表盘 | `/admin/` | 渠道/模型/令牌数量、最近请求、健康状态总览 |
| 渠道管理 | `/admin/channels` | 列表、添加（触发探测）、编辑、启停、重探测、删除 |
| 模型管理 | `/admin/models` | 自动同步的模型列表、能力标签、启用/禁用、别名、手动覆盖能力 |
| 令牌管理 | `/admin/tokens` | 生成（弹窗展示明文一次）、吊销、编辑白名单 |
| 用户管理 | `/admin/users` | 增删用户、重置密码、改角色 |
| 请求日志 | `/admin/logs` | 按时间/模型/令牌/状态筛选，分页 |
| 测试台 | `/admin/playground` | 页面上选模型直接聊天（调内部代理，不走令牌） |

**配置导入导出**：全部配置（渠道/模型/别名/用户/令牌）导出为单个 JSON，可导入恢复——迁移服务器一键搞定。

### 2.6 请求日志

- 每条请求落 SQLite：时间、令牌 ID、用户、模型、命中渠道、状态码、延迟、输入/输出 tokens、错误信息
- 管理页可查；保留策略：默认保留 7 天（可配），定期清理
- 日志写入失败不影响主流程（异步写、可丢）

### 2.7 非功能需求

- 单二进制，无外部依赖；SQLite 用 `modernc.org/sqlite`（纯 Go，无 CGO，交叉编译友好）
- 目标常驻内存 < 80MB
- 全部 API key 入库前加密存储（AES-GCM，密钥从环境变量读；缺省则明文+启动警告）
- 支持 Docker 与裸二进制两种部署
- 日志分级 + 请求追踪 ID（`X-Request-Id`）

---

## 3. 架构设计

### 3.1 请求流转

```
客户端 (OpenAI/Responses/Anthropic 客户端)
   │
   ▼
HTTP 入口 (chi router)
   │
   ▼
鉴权层 (Bearer token → 用户 → 模型白名单校验)
   │
   ▼
协议解析层 (chat/responses/messages → IR)
   │
   ▼
路由分发层 (模型 → 渠道策略 → 负载均衡 → 故障切换)
   │
   ▼
上游适配层 (IR → 上游协议, 流式翻译)
   │
   ▼
上游服务 (DeepSeek / 千帆 / 混元 / Claude / ...)
```

### 3.2 目录结构

```
api-gateway/
├── cmd/
│   └── server/
│       └── main.go            # 入口：加载配置、初始化、启动
├── internal/
│   ├── config/                # 配置加载（JSON + 环境变量）
│   ├── store/                 # SQLite 层（users/tokens/channels/models/logs）
│   ├── auth/                  # 令牌鉴权、session、密码哈希
│   ├── protocol/              # IR 定义 + 三协议解析/序列化 + SSE 翻译
│   ├── channel/               # 渠道管理、自动探测、健康检查
│   ├── router/                # 模型路由、负载均衡、故障切换、重试
│   ├── upstream/              # 适配器（openai/anthropic/responses/厂商）
│   ├── api/                   # 对外 HTTP handler（/v1/*）
│   └── web/                   # 管理面 handler + templates + static
├── web/
│   ├── templates/             # html/template
│   └── static/                # css/js
├── config.json                # 网关自身配置
├── go.mod
└── Makefile                   # build/test/run
```

### 3.3 数据模型（SQLite）

```sql
users(
  id INTEGER PRIMARY KEY,
  username TEXT UNIQUE NOT NULL,
  password_hash TEXT NOT NULL,      -- bcrypt
  role TEXT NOT NULL DEFAULT 'user', -- admin | user
  created_at DATETIME
);

tokens(
  id INTEGER PRIMARY KEY,
  user_id INTEGER REFERENCES users(id),
  name TEXT,
  key_hash TEXT UNIQUE NOT NULL,   -- sha256(明文 key)
  model_whitelist TEXT,            -- JSON 数组，新建令牌必填（至少一个模型）
  allow_all INTEGER DEFAULT 0,     -- 显式「允许全部模型」开关，默认关
  enabled INTEGER DEFAULT 1,
  created_at DATETIME,
  last_used_at DATETIME
);

channels(
  id INTEGER PRIMARY KEY,
  name TEXT,
  type TEXT,                       -- openai|anthropic|responses|xunfei|qianfan|hunyuan|...
  base_url TEXT,
  status TEXT DEFAULT 'active',    -- active|down|disabled
  weight INTEGER DEFAULT 1,        -- 优先级/权重
  balance_strategy TEXT DEFAULT 'random', -- random|round_robin
  created_at DATETIME
);

channel_keys(
  id INTEGER PRIMARY KEY,
  channel_id INTEGER REFERENCES channels(id),
  key_enc TEXT NOT NULL,           -- 加密存储
  cooldown_until DATETIME          -- 429/401 冷却
);

models(
  id INTEGER PRIMARY KEY,
  channel_id INTEGER REFERENCES channels(id),
  name TEXT,
  enabled INTEGER DEFAULT 1,
  alias TEXT,                      -- 对外别名（可空）
  capabilities TEXT,               -- JSON: {system, tools, vision, json_mode}
  capability_override TEXT,        -- 手动覆盖标记
  last_sync_at DATETIME,
  UNIQUE(channel_id, name)
);

request_logs(
  id INTEGER PRIMARY KEY,
  ts DATETIME,
  request_id TEXT,
  token_id INTEGER,
  user_id INTEGER,
  model TEXT,
  channel_id INTEGER,
  status INTEGER,
  latency_ms INTEGER,
  prompt_tokens INTEGER,
  completion_tokens INTEGER,
  error TEXT
);
```

---

## 4. 对外 API 规范

- 三个入口协议各自保持**原生格式透传**（不强制 OpenAI 化），由协议层内部转换
- 错误响应统一带 `error: {message, type, code}`，HTTP 状态码尽量贴近上游原意
- `GET /v1/models`：返回当前启用且对外可见的模型列表（OpenAI 格式，Anthropic/Responses 客户端一般不走这个，但保留方便调试）

---

## 5. 开发里程碑

| 阶段 | 内容 | 验收标准 |
|---|---|---|
| **M1 骨架** | 项目结构、配置加载、SQLite、用户/令牌 CRUD、鉴权中间件、管理页登录 | 能登录、能生成令牌、带令牌调 `/v1/chat/completions` 返回 403/401 正确 |
| **M2 渠道+探测** | 渠道 CRUD、协议自动识别、模型列表同步、能力探测 | 添加 DeepSeek 渠道 → 自动识别 openai 类型、自动拉出模型列表 |
| **M3 协议转换·非流式** | IR、chat 入口 → openai 适配器、多 key 轮换、路由分发 | 用 NextChat 通过网关成功对话（非流式） |
| **M4 协议转换·流式+全入口** | SSE 翻译、responses 入口、anthropic 入口与适配器、工具调用转换 | Claude Code / Cursor 能连网关用 DeepSeek；OpenAI SDK 新版能连 |
| **M5 管理面完善** | 日志页、Playground、模型别名、导入导出、健康检查 | 管理页全功能可用 |
| **M6 打磨** | 错误处理、冷却重试、加密存储、Dockerfile、文档 | 稳定运行一周 |

**M1–M3 为最小可用闭环**（先跑通 OpenAI 入口 → 任意 OpenAI 兼容上游），M4 再上三种协议互转。

---

## 6. 风险与对策

| 风险 | 对策 |
|---|---|
| 厂商接口暗改导致适配器失效 | 适配器隔离 + 探测逻辑集中管理；出问题只改对应适配器 |
| 协议探测误判 | 探测结果允许手动覆盖；记录探测证据（响应摘要）供排查 |
| 流式转换是最复杂部分 | 先做非流式闭环，流式逐事件测试；用 `go-sse` 库降低手写风险 |
| API key 泄露 | 加密存储 + 页面只显示掩码；日志不记录 key |
| 单点失败（网关挂了全挂） | 单用户场景可接受；Docker 自带重启策略；配置导出保证可恢复 |

---

## 7. 已确认决策（2026-08-10）

| 事项 | 决策 |
|---|---|
| 开发分工 | 本项目仅产出文档，开发另行交给其他 agent 执行 |
| 管理页 UI | Pico.css（单文件引入，无构建工具） |
| 管理员数量 | 支持多管理员（admin 角色无数量限制） |
| 令牌白名单 | 新建令牌必须指定至少一个模型；显式勾选「允许全部模型」才全放行（默认关） |
| 上游超时默认值 | 120s（可在渠道级覆盖） |
| 能力探测 | 默认关闭（保守默认值 + 模型管理页手动调整）；`probe_capabilities` 配置或渠道页「探测能力」按钮按需开启 |
