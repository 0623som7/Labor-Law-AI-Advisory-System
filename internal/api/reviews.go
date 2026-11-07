package api

import (
	"context"
	"encoding/json"
	"strconv"
	"strings"
	"time"

	"github.com/gin-gonic/gin"

	"laborlaw/internal/id"
	"laborlaw/internal/port"
)

// reviewTaskType 是审查任务的类型标识，与 openapi Task.type 的示例一致。
const reviewTaskType = "review.run"

// reviewAttempts 是审查任务的总尝试次数（首次 + 2 次重试）。
//
// 这个数字不是抄来的：port/worker.go 的契约写明"返回 *AppError 时按
// Code.Retryable 决定重试，最多 2 次"。编排层拿不到尝试次数，只能在这里
// 依据同一份契约判断"这次失败是不是终局"——判错的代价是任务行永远停在 running。
const reviewAttempts = 3

// createReviewRequest 对应 openapi 的 ReviewRequest。
type createReviewRequest struct {
	FileID    string   `json:"file_id"`
	RuleScope []string `json:"rule_scope"`
	// Async 目前被忽略：链路只有异步一种跑法。保留字段是为了让前端
	// 按契约传 async=false 时也不会 400——但响应始终是 202 Accepted。
	Async bool `json:"async"`
}

// reviewTaskPayload 是写进 tasks.payload 的任务载荷。
//
// 字段名必须与 internal/agent.ReviewPayload 逐字一致；两处各定义一份是刻意的：
// api 不该 import 编排层，否则整个依赖方向会反过来（api → agent → store → port）。
type reviewTaskPayload struct {
	ReviewID  string   `json:"review_id"`
	FileID    string   `json:"file_id"`
	UserID    string   `json:"user_id"`
	RuleScope []string `json:"rule_scope,omitempty"`
}

// createReview 处理 POST /api/v1/contracts/review。
//
// 顺序：验归属 → 验解析完成 → 复用已有审查 → 建 review + task → 入池。
// 每一步失败都带明确业务码，绝不"先建任务再校验"——那会留下一条永远 pending 的孤儿任务。
func (a *API) createReview(c *gin.Context) {
	ctx := c.Request.Context()
	userID := UserID(c)

	if a.store == nil || a.submitter == nil || a.reviewHandler == nil {
		a.notImplemented("合同审查")(c)
		return
	}

	var req createReviewRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		FailCode(c, port.CodeInvalidArgument, "请求体不是合法的 JSON")
		return
	}
	fileID := strings.TrimSpace(req.FileID)
	if fileID == "" {
		FailCode(c, port.CodeInvalidArgument, "file_id 不能为空")
		return
	}
	if len(req.RuleScope) > 32 {
		FailCode(c, port.CodeInvalidArgument, "rule_scope 最多 32 条")
		return
	}
	scope := make([]string, 0, len(req.RuleScope))
	for _, s := range req.RuleScope {
		if s = strings.TrimSpace(s); s != "" {
			scope = append(scope, s)
		}
	}

	// 1. 归属：Get 自带 user_id 条件，别人的文件在这里就是 40401。
	if _, err := a.store.File().Get(ctx, userID, fileID); err != nil {
		if port.IsCode(err, port.CodeNotFound) {
			FailCode(c, port.CodeNotFound, "合同不存在")
			return
		}
		Fail(c, err)
		return
	}

	// 2. 必须解析完成：没有文本块，抽取只能拿到空串，跑完也是 42201。
	if _, err := a.store.File().LoadPages(ctx, fileID); err != nil {
		if port.IsCode(err, port.CodeNotFound) {
			FailCode(c, port.CodeNoTextLayer, "合同尚未解析完成，请稍后再试")
			return
		}
		Fail(c, err)
		return
	}

	// 3. 同一文件不重复审：已成功直接回结果，进行中回任务号，失败则重开一份。
	//
	// "进行中"必须以**任务**状态为准，不能只看 review 的 status：
	// worker 进程被重启/超时打断时，任务被标成 failed，而 review 还停在
	// pending——只看 review 就会把用户引到一个永远不会前进的 task_id 上。
	var reopenExisting *port.Review
	if existing, err := a.store.Review().GetByFile(ctx, userID, fileID); err == nil {
		switch existing.Status {
		case port.TaskSucceeded:
			OK(c, toReviewDTO(existing))
			return
		case port.TaskPending, port.TaskRunning:
			if task, err := a.store.Task().Get(ctx, existing.TaskID); err == nil && !task.Status.Terminal() {
				Accepted(c, reviewAcceptedDTO{
					TaskID:   existing.TaskID,
					ReviewID: existing.ID,
					Status:   string(existing.Status),
				})
				return
			}
			// 任务已经死了，把这份审查收尾成 failed，再重开一份。
			reopenExisting = existing
		}
	} else if !port.IsCode(err, port.CodeNotFound) {
		Fail(c, err)
		return
	}

	// 3.1 旧审查收尾：留着 running/pending 的孤儿记录会让"最近一次审查"永远指向它。
	if reopenExisting != nil {
		_ = a.store.Review().Update(ctx, &port.Review{
			ID:        reopenExisting.ID,
			FileID:    reopenExisting.FileID,
			Status:    port.TaskFailed,
			UpdatedAt: time.Now().UTC(),
		})
	}

	// 4. 建任务与审查主记录。task 先建：review.task_id 要指向它。
	task := &port.Task{
		ID:      id.New(),
		Type:    reviewTaskType,
		Status:  port.TaskPending,
		Payload: mustJSON(reviewTaskPayload{FileID: fileID, UserID: userID, RuleScope: scope}),
	}
	review := &port.Review{
		ID:     id.New(),
		FileID: fileID,
		UserID: userID,
		Status: port.TaskPending,
		TaskID: task.ID,
	}
	// payload 里的 review_id 要在 review 生成后才能写入，这里补一次。
	task.Payload = mustJSON(reviewTaskPayload{
		ReviewID:  review.ID,
		FileID:    fileID,
		UserID:    userID,
		RuleScope: scope,
	})

	if err := a.store.Task().Create(ctx, task); err != nil {
		Fail(c, err)
		return
	}
	if err := a.store.Review().Create(ctx, review); err != nil {
		// 任务已落库但审查没建成：把任务标记失败，不留一条会被人捡起来的孤儿。
		_ = a.store.Task().Finish(ctx, task.ID, port.TaskFailed, "创建审查记录失败")
		Fail(c, err)
		return
	}

	// 5. 入池。队列满时 42901 会立刻返回，任务行同步标失败，前端重试即可。
	if err := a.submitter.Submit(ctx, task, a.reviewHandlerFor(task.ID)); err != nil {
		_ = a.store.Task().Finish(context.WithoutCancel(ctx), task.ID, port.TaskFailed, "任务排队失败，请重试")
		Fail(c, err)
		return
	}

	Accepted(c, reviewAcceptedDTO{
		TaskID:   task.ID,
		ReviewID: review.ID,
		Status:   string(port.TaskPending),
	})
}

// reviewHandlerFor 把编排层 handler 包上一层"任务状态回写"。
//
// 为什么状态回写放在这里而不是 worker 里：worker 只认识 port.Handler，
// 它不该知道任务表长什么样（pool.go 的注释也是这个意思——进度靠 ProgressSink）。
// api 同时持有 store 与 handler，是唯一自然的汇合点。
func (a *API) reviewHandlerFor(taskID string) port.Handler {
	attempt := 0
	return func(ctx context.Context, payload json.RawMessage, progress func(int, string)) error {
		attempt++
		err := a.reviewHandler(ctx, payload, progress)

		// 取消过的任务已经是终态：保留用户的选择，不被后续结果覆盖。
		finCtx := context.WithoutCancel(ctx)
		switch {
		case err == nil:
			a.finishTask(finCtx, taskID, port.TaskSucceeded, "")
		case ctx.Err() != nil:
			// worker 超时或进程停止时会 cancel ctx，此时上游错误码通常是可重试的
			// 5xx，但"还会重试"这条分支不能生效——worker 已经放弃这次执行，
			// 再留着 running 状态，前端会一直转圈到有人手动清理。
			a.finishTask(finCtx, taskID, port.TaskFailed, "任务已中断（超时或服务停止）")
		case port.IsRetryable(err) && attempt < reviewAttempts:
			// 还会重试，先不落终态，否则重试成功的结果会被 409 挡在门外。
		default:
			a.finishTask(finCtx, taskID, port.TaskFailed, port.AsAppError(err).Message)
		}
		return err
	}
}

// finishTask 回写任务终态；409（已被取消或已终态）与 404 只记 Debug。
func (a *API) finishTask(ctx context.Context, taskID string, status port.TaskStatus, message string) {
	err := a.store.Task().Finish(ctx, taskID, status, message)
	if err == nil {
		return
	}
	if port.IsCode(err, port.CodeConflict) || port.IsCode(err, port.CodeNotFound) {
		a.log.Debug("任务状态未回写", "task_id", taskID, "status", status, "err", err.Error())
		return
	}
	a.log.Warn("任务状态回写失败", "task_id", taskID, "status", status, "err", err.Error())
}

// listReviews 处理 GET /api/v1/reviews。
func (a *API) listReviews(c *gin.Context) {
	ctx := c.Request.Context()
	if a.store == nil {
		a.notImplemented("审查历史")(c)
		return
	}
	limit, offset, ok := parsePageParams(c)
	if !ok {
		return
	}
	items, err := a.store.Review().ListByUser(ctx, UserID(c), limit, offset)
	if err != nil {
		Fail(c, err)
		return
	}
	OK(c, toReviewDTOs(items))
}

// getReview 处理 GET /api/v1/reviews/{reviewId}。
func (a *API) getReview(c *gin.Context) {
	ctx := c.Request.Context()
	if a.store == nil {
		a.notImplemented("审查详情")(c)
		return
	}
	reviewID := strings.TrimSpace(c.Param("reviewId"))
	if reviewID == "" {
		FailCode(c, port.CodeInvalidArgument, "审查 ID 不能为空")
		return
	}
	rv, err := a.store.Review().Get(ctx, UserID(c), reviewID)
	if err != nil {
		if port.IsCode(err, port.CodeNotFound) {
			FailCode(c, port.CodeNotFound, "审查记录不存在")
			return
		}
		Fail(c, err)
		return
	}
	OK(c, toReviewDTO(rv))
}

// listFindings 处理 GET /api/v1/reviews/{reviewId}/findings。
//
// 先 Get 一次审查：findings 表没有 user_id 列，归属只能靠父记录验证。
// 少这一步就是"知道 review_id 就能看别人的风险清单"。
func (a *API) listFindings(c *gin.Context) {
	ctx := c.Request.Context()
	if a.store == nil {
		a.notImplemented("风险清单")(c)
		return
	}
	reviewID := strings.TrimSpace(c.Param("reviewId"))
	if reviewID == "" {
		FailCode(c, port.CodeInvalidArgument, "审查 ID 不能为空")
		return
	}
	if _, err := a.store.Review().Get(ctx, UserID(c), reviewID); err != nil {
		if port.IsCode(err, port.CodeNotFound) {
			FailCode(c, port.CodeNotFound, "审查记录不存在")
			return
		}
		Fail(c, err)
		return
	}

	severity := strings.TrimSpace(c.Query("severity"))
	if severity != "" {
		switch port.Severity(severity) {
		case port.SeverityHigh, port.SeverityMedium, port.SeverityLow:
		default:
			FailCode(c, port.CodeInvalidArgument, "severity 只能是 high、medium 或 low")
			return
		}
	}

	items, err := a.store.Review().ListFindings(ctx, reviewID)
	if err != nil {
		Fail(c, err)
		return
	}
	if severity != "" {
		filtered := make([]port.Finding, 0, len(items))
		for _, f := range items {
			if string(f.Severity) == severity {
				filtered = append(filtered, f)
			}
		}
		items = filtered
	}
	OK(c, toFindingDTOs(items))
}

// parsePageParams 解析 limit / offset；非法时直接写 40001 并返回 ok=false。
//
// 与 /statutes 的 top_k 保持一致的处理口径：非法参数一律 40001，
// 不做"悄悄退回默认值"——前端传错时应当立刻看到，而不是困惑于"为什么只返回 20 条"。
func parsePageParams(c *gin.Context) (limit, offset int, ok bool) {
	limit, offset = 20, 0
	if raw := strings.TrimSpace(c.Query("limit")); raw != "" {
		v, err := strconv.Atoi(raw)
		if err != nil || v <= 0 || v > 100 {
			FailCode(c, port.CodeInvalidArgument, "limit 必须是 1 到 100 的整数")
			return 0, 0, false
		}
		limit = v
	}
	if raw := strings.TrimSpace(c.Query("offset")); raw != "" {
		v, err := strconv.Atoi(raw)
		if err != nil || v < 0 {
			FailCode(c, port.CodeInvalidArgument, "offset 必须是不小于 0 的整数")
			return 0, 0, false
		}
		offset = v
	}
	return limit, offset, true
}

// mustJSON 序列化任务载荷；失败只可能来自不可序列化的类型，这里给空对象兜底。
func mustJSON(v any) []byte {
	b, err := json.Marshal(v)
	if err != nil {
		return []byte("{}")
	}
	return b
}
