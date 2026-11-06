# 劳动法 AI 顾问系统 · 后端

面向劳动者与用工场景的劳务法律 AI 顾问系统：劳动法问答（consult / qa）+ 劳动合同审查（review）。
技术栈 Go 1.22+ / Gin / MySQL 8 / Redis 7 / Eino。

两人分工：A = 后端（进程、存储、并发、传输、文件解析），B = Agent（编排、prompt、检索、抽取、规则）。
唯一接缝是 `internal/port` 的五个 interface。

## 目录归属

| 目录 | 归属 | 内容 |
|---|---|---|
| `cmd/` | A | api / worker 两个入口 |
| `internal/config/` | A | 环境变量 → 结构体 + Validate |
| `internal/api/` | A | 路由、中间件、DTO、SSE handler |
| `internal/store/` | A | DAL + 迁移 |
| `internal/worker/` | A | 协程池、任务表、进度 |
| `internal/llm/` | A | 模型薄封装：重试、限流、计量、路由 |
| `internal/file/` | A | PDF 解析、OCR 兜底 |
| `internal/stream/` | A | SSE 分帧、心跳、续传、背压 |
| `internal/port/` | **共用** | 五个 interface、DTO、错误码。只加不改 |
| `internal/agent/` `internal/prompt/` `internal/retrieval/` `internal/extract/` `internal/rules/` | B | Agent 全部实现 |
| `eval/` `data/` | B | 评测集、法条与规则数据 |
| `contract/` | A（B 只读） | openapi.yaml、schemas、CHANGELOG |
| `web/src/utils/pdf/` | A 手写 | 坐标换算（AI 禁区） |
| `deploy/` | A | Dockerfile、compose、Makefile、runbook |

## 当前状态（W1）

- [x] 建仓 + 目录 + `.ai/ignore` + CODEOWNERS
- [x] `internal/port` 五个 interface 起草（llm.Client / agent.Runner / store.Store / worker.Handler / file.ParsedDoc）
- [ ] B 逐条确认 interface（有否决权）
- [ ] `internal/config` + `cmd/api` 骨架
- [ ] `internal/llm` 封装 + FakeClient

## 验证

```bash
go build ./...
go vet ./...
go test ./...
go test -cover ./internal/port/
```

`GOFLAGS=-buildvcs=false`：本机工作区在非 git 目录时 `go build .` 会报 VCS status 错误，仓库根已 `git init`，但子目录单独构建仍需该开关。

## 环境变量

见 `.env.example`（W2 随 `internal/config` 一起落地）。LLM key 由 B 填，不入库。
