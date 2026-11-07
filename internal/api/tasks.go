package api

import (
	"strings"

	"github.com/gin-gonic/gin"

	"laborlaw/internal/port"
)

// getTask 处理 GET /api/v1/tasks/{taskId}。
//
// 没有额外的归属校验：TaskStore.Get 只认 id，任务表也没有 user_id 列。
// 这一条限制是可接受的——任务 id 是 26 位 ULID（时间戳 + 80 位随机），
// 猜到别人的 id 不比猜中一次会话令牌容易；但它确实不是权限边界，
// 真正的数据在 /reviews/{id} 与 /reviews/{id}/findings，那两条都按 user_id 过滤。
func (a *API) getTask(c *gin.Context) {
	ctx := c.Request.Context()
	if a.store == nil {
		a.notImplemented("任务查询")(c)
		return
	}
	taskID := strings.TrimSpace(c.Param("taskId"))
	if taskID == "" {
		FailCode(c, port.CodeInvalidArgument, "任务 ID 不能为空")
		return
	}
	task, err := a.store.Task().Get(ctx, taskID)
	if err != nil {
		if port.IsCode(err, port.CodeNotFound) {
			FailCode(c, port.CodeNotFound, "任务不存在")
			return
		}
		Fail(c, err)
		return
	}
	OK(c, toTaskDTO(task))
}

// cancelTask 处理 POST /api/v1/tasks/{taskId}/cancel。
//
// 取消只改状态，不杀 goroutine：worker 里正在跑的那次 LLM 调用没法安全打断
// （打断会把半条流式响应留在上游计费里），它跑完写库时会撞上 409 并被丢弃。
// 用户看到的是"已取消"，数据不会因为取消而半途而废。
func (a *API) cancelTask(c *gin.Context) {
	ctx := c.Request.Context()
	if a.store == nil {
		a.notImplemented("任务取消")(c)
		return
	}
	taskID := strings.TrimSpace(c.Param("taskId"))
	if taskID == "" {
		FailCode(c, port.CodeInvalidArgument, "任务 ID 不能为空")
		return
	}
	task, err := a.store.Task().Get(ctx, taskID)
	if err != nil {
		if port.IsCode(err, port.CodeNotFound) {
			FailCode(c, port.CodeNotFound, "任务不存在")
			return
		}
		Fail(c, err)
		return
	}
	if task.Status.Terminal() {
		FailCode(c, port.CodeConflict, "任务已结束，无法取消")
		return
	}
	if err := a.store.Task().Finish(ctx, taskID, port.TaskCancelled, "用户取消"); err != nil {
		Fail(c, err)
		return
	}
	// 重新读一次：返回的必须是落库后的真实状态，而不是入参的副本。
	// 顺带把 updated_at / error 一起带回去，前端轮询就能立刻收敛。
	updated, err := a.store.Task().Get(ctx, taskID)
	if err != nil {
		Fail(c, err)
		return
	}
	OK(c, toTaskDTO(updated))
}
