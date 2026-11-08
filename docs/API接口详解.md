# 你干了什么 · 这些接口是什么

> 对应提交 `d1902af`（初稿写于 2026-11-06，M4 账号体系与 M5 咨询编排落地后于 2026-11-07 对齐）
> 前置：`docs/零基础导读.md`（怎么看这个仓库）、`docs/框架与业务流程.md`（怎么分层、数据怎么流）

---

## 一、你到目前为止干了什么

数据说话（`git ls-files | wc -l` 等命令可复现）：**153 个受版本控制的文件、17352 行 Go、245 个测试函数、62 个提交，2026-11-06 到 11-07 两天内完成。**

| # | 干成的事 | 产出物 | 怎么证明它成了 |
|---|---|---|---|
| 1 | 定死 A/B 之间的合同 | `internal/port/` 5 个 interface + 14 个错误码 + 334 行测试 | `go test ./internal/port` 全绿 |
| 2 | 定死对外接口 | `contract/openapi.yaml` 1403 行：23 路径 / 25 操作 / 37 schema | 行数 + 无坏引用 |
| 3 | 程序能起来、能退干净 | `cmd/api/main.go` + `internal/api/`：路由、中间件、包络、健康检查 | `curl /healthz` → 200；`/readyz` → 503（依赖没起时） |
| 4 | 配置能校验 | `internal/config/`：44 个键，缺一个就起不来，报错不泄漏值 | `go test ./internal/config` |
| 5 | 数据库能自动建出来 | `internal/store/migrations/0001_init.up.sql`：14 张表 | `AUTO_MIGRATE=true` 启动后 `SHOW TABLES` 有 14 行 |
| 6 | 模型能调、账能记、钱能控 | `internal/llm/`：路由 / 重试 / 令牌桶 / 日成本闸 / FakeClient | `go test ./internal/llm`（无 key 也能跑） |
| 7 | 本地有独立数据库 | `scripts/dev-mysql.ps1`（3307 端口，不动系统 3306） | `dev-mysql.ps1 status` 显示 ready |
| 8 | 部署材料齐了但没上过线 | `deploy/`：Dockerfile、compose、MySQL/Redis 调参、runbook | 文件存在，未实跑 |

**一句话总结**：你现在拥有的是**门面（接口契约 + 路由）、地基（配置 + 数据库 + 迁移）、大脑（模型客户端）**，
缺的是**把门面后面的房间修好**——25 条路由里已有 14 条真的在处理请求（`/healthz`、`/readyz`、auth 五条、`/chat/completions`、4 条 `/files`、2 条 `/statutes`），其余 11 个统一返回：

```json
{"code": 50001, "message": "创建会话接口尚未实现", "trace_id": "..."}
```

`501 Not Implemented` 不是错误，是**挂号**：路由在那儿，实现没接。每接一个真 handler，就删掉 `routes.go` 里的一行 501。（截至本轮，23 条 API 路由全部接上，`grep -c notImplemented internal/api/routes.go` = 0。）

---

## 二、看懂接口前必须懂的三件事

### 1. 接口 = 前后端约定"做一件事"

前端不能直接读数据库，只能**敲一个 URL、带点参数**，后端回一个 JSON。
这个"敲门方式 + 回话格式"就是接口（API）。定下来之后，前端和后端就可以各写各的。

命名规则（REST 风格，看 URL 就知道干什么）：

```
GET  /api/v1/sessions          取列表      （不改数据）
POST /api/v1/sessions          新建        （改数据）
GET  /api/v1/sessions/{id}     取一个
DELETE /api/v1/sessions/{id}   删一个
```

**动词只是 HTTP 方法，名词才是资源**。所以是 `POST /files` 而不是 `/uploadFile`——`/uploadFile` 把实现细节写进了 URL。

### 2. 所有回话都是同一个信封

```json
{ "code": 0, "message": "ok", "data": { ... }, "trace_id": "…" }
```

- `code=0` 成功；非 0 看错误码表（§四）
- `data` 才是业务数据；列表、详情都塞在里面
- `trace_id` 用来查日志：把这一串拿去搜服务端日志，就能看到这次请求走过哪几层、哪一步慢

例外：`/healthz`、`/readyz` 不套信封（容器探针只认 200/503 和简单形状），SSE 也不是 JSON 而是事件流。

### 3. 两套"号码"别搞混

| 层 | 例子 | 谁看 |
|---|---|---|
| HTTP 状态码 | 200 / 400 / 401 / 404 / 429 / 501 / 502 | 浏览器、代理、监控 |
| 业务错误码 | 40001 参数错 / 40101 没登录 / 40401 找不到 / 42901 太频繁 / 50201 上游模型挂了 | 前端代码（决定弹什么提示） |

一个 401 可能带 `40101`；但 501 是例外，它是"没实现"，业务码故意填 `50001`。

---

## 三、25 个接口，按业务分组

### 组 0 · 运维探针（2 条，**已实现**）

| 接口 | 干什么 | 回什么 |
|---|---|---|
| `GET /healthz` | 进程还活着吗 | 200 `{"status":"ok"}`，**不查数据库** |
| `GET /readyz` | 能不能接活 | 200/503 + `checks:{mysql, redis}` |

为什么分两个：MySQL 挂了应该**摘流量**，而不是让容器被反复重启（重启掩盖依赖故障）。所以存活看 healthz，就绪看 readyz。

### 组 1 · 认证 auth（5 条，已全部实现 `99ca4c5`，加固 `e2d873a`）

| 接口 | 干什么 | 关键点 |
|---|---|---|
| `POST /auth/register` | 注册 | 成功直接返令牌，省一次登录；手机号重复 → 409/40901 |
| `POST /auth/login` | 登录 | 失败**一律 40101**，不区分"账号不存在"和"密码错"（否则等于送人一个账号枚举器） |
| `POST /auth/refresh` | 换新 access token | refresh token 必须命中 Redis 白名单；access 2h / refresh 30d |
| `POST /auth/logout` | 登出 | 把 refresh 从白名单删掉，access 到期自然失效 |
| `GET /auth/me` | 当前用户 | 手机号已脱敏（`138****8000`），永不返回原文 |

实现要点：`store.UserStore` 手机号 HMAC 落库、bcrypt 口令、JWT 双令牌；refresh 走 Redis 白名单，续期用原子 `GETDEL` 换链（并发换双链会被 40101 拦掉）。
`requireAuth` 现在认 `Authorization: Bearer <token>`；完全不带这个头时才回落到 `AUTH_DEV_USER`，令牌不合法不会静默降级。

### 组 2 · 会话 session（5 条，已实现 `2f9b6b7`）

一个"会话"= 一次连续对话（像微信里一个聊天窗口）。会话与消息在 `POST /chat/completions` 里自动落库
（`62f1675`）：不带 `session_id` 就建新会话（标题取首句前 20 字），第二轮带上 `session_id`
就能把上一轮读回提示词。

| 接口 | 干什么 | 关键点 |
|---|---|---|
| `GET /sessions` | 我的会话列表 | 按最后活跃时间倒序，`limit/offset` 分页 |
| `POST /sessions` | 新建会话 | 入参 `scene`（`consult` 咨询 / `qa` 检索问答）+ `title` |
| `GET /sessions/{id}` | 会话详情 | 别人的会话返回 **404 不是 403**（不暴露"这东西存在"） |
| `DELETE /sessions/{id}` | 删除 | **软删**（只置 `deleted_at`），消息保留，用于审计 |
| `GET /sessions/{id}/messages` | 消息历史 | 按时间正序，默认 50 条 |

### 组 3 · 咨询 chat（1 条，**最重要的一条**；背后是真实 `agent.Runner`：检索 → 提示词 → 流式 → 引用，`256a201`）

| 接口 | 干什么 |
|---|---|
| `POST /chat/completions` | 劳动法咨询，返回 SSE 流 |

它跟别的接口不一样：**不是"一次请求一次回复"，而是一条一直开着的管道**。真实的帧长这样（openapi 里有原样示例）：

```
event: meta
data: {"message_id":"01J8Z0K3Q7","session_id":"01J8Z0JZP1","model":"consult"}

event: delta
data: {"seq":1,"text":"根据《劳动合同法》"}

event: citation
data: {"seq":2,"citation":{"statute_id":"LCL-38","law":"劳动合同法","article":"第三十八条","text":"用人单位有下列情形之一的……"}}

event: delta
data: {"seq":3,"text":"第三十八条，你可以随时解除。"}

event: done
data: {"seq":4,"message_id":"01J8Z0K3Q7","finish_reason":"stop","usage":{...}}
```

四个必须记住的细节：

1. **前端不能用 `EventSource`**，要用 `fetch` + `ReadableStream`
2. 断线重连带 `Last-Event-ID: <seq>`，服务端从 `seq+1` 续推，同一个 seq 不会出现两次
3. `citation` 里的法条原文**必须来自数据库**（`statutes` 表），B 不许现场编造——这是"零编造"铁律的落点
4. 这条接口的实现分两半：A 做 SSE 分帧、心跳、背压（`internal/stream`），B 做 `Runner.Run` 往里 `emit` 事件

### 组 4 · 文件 files（4 条，已全部实现 `b9c3c77`）

| 接口 | 干什么 | 关键点 |
|---|---|---|
| `POST /files` | 上传合同 PDF | 字段名 `file`；校验顺序 **大小 → 魔数 `%PDF-` → 页数 → SHA256 去重**；同一个人重复传同一份，直接返回已解析过的 `file_id` |
| `GET /files/{id}` | 文件元数据 | 文件名只用于展示，**禁止拼磁盘路径**（防目录穿越） |
| `GET /files/{id}/pages` | 解析结果：每页每个文本块 + 坐标 | **标注层的唯一数据源**；坐标单位 pt、原点左下、y 轴向上 |
| `GET /files/{id}/content` | 下载原始 PDF | `Content-Disposition: inline`，PDF.js 拿它当底图 |

### 组 5 · 审查 reviews（4 条，已实现 `8226608`）

| 接口 | 干什么 | 关键点 |
|---|---|---|
| `POST /contracts/review` | 发起审查 | 入参 `file_id` + 可选 `rule_scope`（只跑指定规则）+ `async`；**当前恒 202**，回 `task_id` + `review_id`；同步 200 分支按设计不做（一次审查实测 50–110 秒，同步会被网关掐断，见冲刺计划）；池满立刻 429，不静默排队 |
| `GET /reviews` | 审查历史 | 按创建时间倒序，分页 |
| `GET /reviews/{id}` | 审查详情 | 含整体风险等级、评分、条款通过率、高/中/低计数 |
| `GET /reviews/{id}/findings` | 风险清单 | 每条含 `rule_id`、`severity`、`page_no`、`bbox`、`evidence`（合同原文）、`legal_basis`（法条依据）、`suggestion`；`?severity=high` 过滤 |

`202 Accepted` 这个设计值得多看一眼：它说的是"**我收下了，但还没做完**"，回的是 `task_id` 而不是结果。
前端拿它去 `GET /tasks/{id}` 轮询。这样长合同不会把浏览器卡死 30 秒。

### 组 6 · 任务 tasks（2 条，已实现 `8226608`）

| 接口 | 干什么 | 关键点 |
|---|---|---|
| `GET /tasks/{id}` | 查进度 | 返回 `status`（pending/running/succeeded/failed/cancelled）+ `progress` + `stage`；进度写库有 ≥500ms 节流，读到的值最多落后半秒 |
| `POST /tasks/{id}/cancel` | 取消 | 只有非终态能取消，否则 409 |

### 组 7 · 法条 statutes（2 条，已实现 `8d37755` / `d1902af`）

| 接口 | 干什么 | 关键点 |
|---|---|---|
| `GET /statutes?q=&law=&top_k=` | 关键词检索法条 | `q` 必填、≤100 字；`law` 精确过滤法律名称；`top_k` 1–20，默认 5。205 条规模用 LIKE + 口语归一多路召回（`internal/retrieval`），**不引向量库/ES** |
| `GET /statutes/{id}` | 法条详情 | ID 形如 `LCL-38`（劳动合同法第 38 条）；不存在 → 404/40401 |

---

## 四、错误码对照表（14 个，`internal/port/errors.go`）

| 码 | 名字 | 什么时候出现 | 能不能重试 |
|---|---|---|---|
| 0 | OK | 成功 | — |
| 40001 | InvalidArgument | 参数缺/格式错 | 不能，重试只会同样错 |
| 40101 | Unauthorized | 没令牌 / 令牌过期 / 登录失败 | 要重新登录 |
| 40301 | Forbidden | 有身份但没权限 | 不能 |
| 40401 | NotFound | 资源不存在（**含"别人的资源"，故意不区分**） | 不能 |
| 40901 | Conflict | 状态机非法转移、手机号已注册、重复提交 | 不能 |
| 41301 | FileTooLarge | 上传超 10MB | 不能 |
| 41501 | UnsupportedMedia | 不是 PDF（魔数不对） | 不能 |
| 42201 | NoTextLayer | PDF 没有文字层（扫描件），置信度 < 0.6 | 可换 OCR 再试 |
| 42901 | RateLimited | 限流（QPS）或 worker 池满 | 稍后重试 |
| 50001 | Internal | 代码缺陷、未分类错误 | 可以（`port.CodeInternal.Retryable()` 返回 true） |
| 50002 | RuleEngine | 单条规则异常 | 该条降级，其余继续 |
| 50201 | Upstream | 模型供应商报错 | **可以，最多 2 次** |
| 50301 | UpstreamTimeout | 上游超时 | 可以 |
| 50401 | TaskTimeout | 任务整体超时 | 不能 |

这张表最值钱的是最后三列的"能不能重试"——`worker.Handler` 就靠它决定重试还是直接标 failed。

---

## 五、两条链路各自调用哪些接口

**咨询（一次问答）**

```
POST /auth/login
POST /sessions                     → 拿到 session_id
POST /chat/completions  ← SSE      → meta / delta / citation / done
（结束后异步落 messages + usage_records）
GET  /sessions/{id}/messages       → 刷新页面时读历史
```

**审查（传一份合同）**

```
POST /auth/login
POST /files                        → file_id（同步解析完，返回页数+置信度）
GET  /files/{id}/pages             → 若前端要立即预览坐标
POST /contracts/review             → 200 直接给 results / 202 给 task_id
GET  /tasks/{id}                   → 轮询到 succeeded
GET  /reviews/{id}/findings        → 风险清单
GET  /files/{id}/content           → PDF.js 渲染底图
（前端 coords.ts 把 bbox 的 pt 转成屏幕像素，点击高亮）
```

---

## 六、自己动手试（服务起来后，另开一个终端）

```bash
# 1. 探针（现在就能看到真实响应）
curl -i http://127.0.0.1:8080/healthz
curl -i http://127.0.0.1:8080/readyz

# 2. 没带令牌 → 401
curl -i http://127.0.0.1:8080/api/v1/sessions

# 3. 带了开发用户 → 能过鉴权并真正建出一个会话
#    先确认 .env 里 AUTH_DEV_USER 有值，requireAuth 就会放行
#    注意：Windows 上 curl -d '中文' 会按 GBK 发出去，服务端按 UTF-8 解就是乱码，
#    含中文的请求体请写进文件再 --data-binary @req.json
curl -i -X POST http://127.0.0.1:8080/api/v1/sessions \
  -H "Content-Type: application/json" -d '{"scene":"consult","title":"试用期"}'

# 4. 不存在的路径 → 404 + 业务码 40401（不是 gin 的纯文本）
curl -i http://127.0.0.1:8080/api/v1/nope

# 5. 用错方法 → 405，也套信封（防止前端 JSON 解析崩）
curl -i -X PUT http://127.0.0.1:8080/api/v1/sessions
```

每条响应里都带 `trace_id`，拿它去服务端日志里搜，就能看到这次请求经过的每一层。**这是你以后排错的主力工具。**

---

## 七、接下来"接口"这件事怎么往下推

原则：**契约不改，逐个替换 501**。顺序取决于依赖，不取决于哪个好写：

| 顺序 | 接口 | 依赖什么 | 归属 |
|---|---|---|---|
| 1 | `POST /chat/completions` | ~~`internal/stream`~~（已完成 `bb7bdac`）+ `agent.Runner` | ✅ `de00845` + 真 Runner `256a201` |
| 2 | auth 五条 | `store.UserStore` + JWT + bcrypt | ✅ `99ca4c5`（加固 `e2d873a`） |
| 3 | sessions + messages | `store.Session/Message` 实现 | ✅ 已完成 `7ad5dc2` + `2f9b6b7` |
| 4 | files 四条 | `internal/file` 解析 + 坐标 | ✅ 已完成 `b9c3c77` |
| 5 | `contracts/review` + tasks | `internal/worker` + `agent.Runner`（review 场景） | ✅ 已完成 `8226608`（审查编排 `4f2f68d`） |
| 6 | reviews 四条 | `store.Review` 实现 | ✅ 已完成 `8226608` |
| 7 | statutes 两条 | 205 条法条入库 + 检索归一 | ✅ `8d37755` / `d1902af` |

验收标准写在 openapi 里：前端代码生成读同一份文件，所以**接口一旦定了，前端就能并行开工**——这就是"契约先行"的全部好处。
