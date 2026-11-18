# 劳动法 AI 顾问系统

面向劳动者的法律问答与劳动合同风险审查。问一句「公司不给我交社保怎么办」，系统检索法条、给出可执行的下一步；传一份合同，系统把违法条款逐条框在原文上，并说明依据哪一条。

线上：<https://laborlaw-ai.cn>（单机 Docker Compose，腾讯云轻量 2C2G）

## 两条业务线

| | 输入 | 输出 |
|---|---|---|
| 法律咨询 | 自然语言问题，可附一份合同 | 流式回答 + 可展开的法条引用卡（条号、原文、生效日期） |
| 合同审查 | PDF 劳动合同 | 风险条款清单（等级、依据、修改建议）+ PDF 原文上的高亮框，可跳页定位 |

两条线共用一套检索，但审查侧多一层规则引擎：同一份合同先跑确定性规则（试用期超限、押金、社保缺失…），规则覆盖不到的部分才交给模型兜底，所以演示合同 57ms 出结论，不必等模型。

## 当前状态（2026-11-18）

| 项 | 实测 |
|---|---|
| 法条库 | **517 条 / 九部法**（劳动合同法 98、劳动法 107、实施条例 38、工伤保险条例 67、带薪年休假 10、劳务派遣暂行规定 29、女职工特别规定 16、劳动争议调解仲裁法 54、社会保险法 98），每条 1024 维向量 |
| 检索评测 | **112 条用例**，四档 hit@3：关键词 59.8%（PR#7 词表后 67/112）/ 纯向量 86.6% / RRF 77.7% / **RRF+重排 96.4%**，hit@5 99.1%，零召回 0 |
| 定位评测 | 五份已知缺陷的样本，召回 8/8、条款命中 8/8、页命中 8/8、框命中 8/8、误报 0、深页命中 3 |
| 测试 | Go **548 个测试函数 / 66 个文件**，19 个包全绿；前端 **162 条**（`node --test`） |
| 规模 | Go 35,335 行、前端 78 个文件、14 张表、271 次提交 |

数字口径与重跑命令见 [eval/README.md](eval/README.md) 与 [docs/评测基线.md](docs/评测基线.md)。**112 条 / 100 条 / 40 条三套分母不同，任何两个数都不许直接相减。**

## 架构

```
cmd/api ──► internal/api（HTTP 层，只依赖 internal/port）
                 │
                 ├─► internal/agent     咨询链路：检索 → 系统提示词 → 流式生成
                 ├─► internal/rules     审查链路：确定性规则，先跑
                 ├─► internal/extract   PDF 文本层抽取 + 要素抽取（规则优先、模型兜底）
                 ├─► internal/retrieval 关键词 / 向量 / RRF 融合 / 交叉编码器重排
                 └─► internal/store     MySQL 访问，14 张表，迁移 0001–0007
```

`internal/port` 是唯一的契约层，零实现：api 不 import agent，agent 不 import api。统一响应信封 `{code,message,data,trace_id}`；咨询链路顺序不可换（检索在生成之前，否则模型会先凭记忆报错条号）；流式一旦吐字绝不重试。

## 本地跑起来

```bash
# 1. 依赖：MySQL 8（本地端口 3307）+ Redis
cp .env.example .env                       # 填 LLM_API_KEY，其余用默认

# 2. 建库与种子（生产禁 AUTO_MIGRATE，本地也一样手动跑）
go run ./cmd/api -migrate
go run ./cmd/statuteseed -import                        # 517 条法条
go run ./cmd/embeddingseed                              # 517 条 × 1024 维向量

# 3. 起服务
go run ./cmd/api                                        # :8080
cd web && pnpm install && pnpm dev                       # :5173，代理 /api 到 8080
```

`make help` 列出全部常用目标（`build` / `test` / `test-race` / `vet` / `fmt-check` / `up` / `down` / `logs` / `ps`）。

## 常用命令

| 做什么 | 命令 |
|---|---|
| 后端全量测试 | `go test ./... -count=1`（要跑真 MySQL 的用例加 `TEST_MYSQL_DSN=...`） |
| 前端单测 / 类型检查 / 构建 | `cd web && pnpm test` / `pnpm typecheck` / `pnpm build` |
| 检索评测四档 | `go run ./eval -mode rerank -dsn "$DSN"`（`keyword` / `semantic` / `hybrid` / `rerank`） |
| 审查定位评测 | `node web/e2e/locate-eval.mjs`（加 `REMOTE=1 API_BASE=https://劳动法域名` 打线上） |
| 上线前自检 | `node web/e2e/preflight.mjs --remote` |
| 演示全流程 | `node web/e2e/demo.mjs` → `DEMO OK` |

## 目录

```
cmd/          入口：api、statuteseed、embeddingseed
internal/     port（契约）/ api / agent / rules / extract / retrieval / store / llm / stream / worker …
web/          Vue 3 + Vite + TS；e2e/ 里是验收脚本与评测样本
eval/         检索评测：cases.json（112 条）+ 四档基线
deploy/       Dockerfile、compose、nginx、runbook
docs/         设计、流程、评测、导演卡、零基础导读
scripts/      备份与恢复演练、合同样本生成
```

## 文档

| 想知道什么 | 看哪份 |
|---|---|
| 整体设计与取舍 | [docs/项目大纲与技术选型.md](docs/项目大纲与技术选型.md) |
| 四层分层、两条链路、状态机、ER | [docs/框架与业务流程.md](docs/框架与业务流程.md) |
| 接口与错误码 | [docs/API接口详解.md](docs/API接口详解.md) 与 [contract/openapi.yaml](contract/openapi.yaml) |
| 评测怎么量的、为什么这么量 | [eval/README.md](eval/README.md)、[docs/评测基线.md](docs/评测基线.md) |
| 答辩演示每一步说什么 | [docs/演示导演卡.md](docs/演示导演卡.md) |
| 上线、备份、回滚 | [deploy/runbook.md](deploy/runbook.md) |
| 完全没接触过这个仓库 | [docs/零基础导读.md](docs/零基础导读.md) |

## 已知边界

这些不是 bug，是这一版明确不做的（详见 [docs/冲刺计划_1个月.md](docs/冲刺计划_1个月.md) 的「冻结」一节）：

- 多轮记忆按 token 硬截断（`LLM_CONTEXT_TOKENS`），超长会话的早期内容会被静默丢弃；
- worker 池跑在 api 进程内，重启会丢掉在跑的任务，启动对账只兜底不恢复；
- 检索词表里仍有四条与评测题面逐字重合的整句键，属评测集拟合，不计入泛化收益；
- 域名已备案（粤ICP备2026184096号-1），但历史链接仍兼容 IP:8443。

## 团队

校级大学生创新创业训练计划项目。A 侧（后端、部署、前端合同审查）与 B 侧（检索、抽取、规则、评测）分工，接口以 `internal/port` 为界。
