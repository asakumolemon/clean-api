# AGENTS.md

## 项目状态

- 已完成 **M1 骨架**（脚手架 + store/users/tokens + auth + `/v1/chat/completions` 鉴权桩 + 管理面登录/仪表盘/令牌页），下一步是 M2 渠道+探测。里程碑计划见 `REQUIREMENTS.md` 第 5 节。
- 仓库名 `clean-api`，但 Go module 名是 `api-gateway`（对应 REQUIREMENTS 里的项目根目录名）。
- `REQUIREMENTS.md`（中文）是唯一且权威的需求/设计文档：含目录结构、SQLite 数据模型、IR 设计、里程碑 M1–M6 等。改动需求前先看它。
- `prototype.html` 是管理后台的**静态原型**（Pico.css + 前端 JS 切页）。它只是设计参考，**不是生产模板**：正式方案是服务端渲染 `html/template`，不前后端分离、无构建工具（见 REQUIREMENTS 2.5）。不要照搬原型里的 SPA 式 `data-page` 切页写法。

## 已实现代码结构

- `cmd/server/main.go`：入口，加载配置、建库、首启建管理员、挂路由。
- `internal/config`：`config.json` + `GATEWAY_*` 环境变量覆盖。
- `internal/store`：`database/sql` + `modernc.org/sqlite`，全表 schema 已建（users/tokens/channels/channel_keys/models/request_logs），CRUD 目前只到 users/tokens。
- `internal/auth`：bcrypt、token 生成(32B Base64)/sha256、session、`APIAuth` 中间件、`CheckModelAllowed`。
- `internal/api`：`/v1/chat/completions` 桩（鉴权链完成后返回 501，协议转换是 M3）。
- `internal/web` + 根目录 `web/`（embed FS）：登录/仪表盘/令牌页，Pico.css 内嵌。

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

- 已有 `Makefile`：`make build` / `make test` / `make run`（Windows 无 make 时用 `go build ./...`、`go test ./...`）。
- M1 起关键逻辑已有单测（auth/store/api 鉴权链）。
- 注意：本机 `GOPROXY` 已切到 `https://goproxy.cn,direct`（默认 proxy 直连超时）；`go mod tidy` / `go get` 新增依赖时保持该代理。
