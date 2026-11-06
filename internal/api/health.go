package api

import (
	"context"
	"net/http"
	"time"

	"github.com/gin-gonic/gin"
)

// readyTimeout 是 /readyz 整体的探测预算。
//
// 探针自己也有超时，这里是第二道闸：某个探针实现忘了设超时也不会把 healthcheck 挂死。
const readyTimeout = 3 * time.Second

type healthStatus struct {
	Status string            `json:"status"`
	Checks map[string]string `json:"checks,omitempty"`
}

// handleHealthz 只证明进程还在跑，不查任何依赖。
//
// 与 readyz 分开是容器编排的基本要求：MySQL 挂了应该摘流量（readyz 失败），
// 但不该让容器被反复 kill（healthz 也失败），否则真正的依赖故障会被重启噪音盖住。
func (a *API) handleHealthz(c *gin.Context) {
	c.JSON(http.StatusOK, healthStatus{Status: "ok"})
}

// handleReadyz 逐个探依赖，任一失败返回 503。
//
// 没有注册任何 Checker 时返回 200 + 空 checks：M1 阶段还没有 MySQL/Redis 连接，
// 让容器保持 healthy 才能继续联调；等 store 落地后由 main 注入真实探针。
func (a *API) handleReadyz(c *gin.Context) {
	ctx, cancel := context.WithTimeout(c.Request.Context(), readyTimeout)
	defer cancel()

	checks := make(map[string]string, len(a.checkers))
	ready := true
	for _, ck := range a.checkers {
		if err := ck.Check(ctx); err != nil {
			checks[ck.Name()] = "down"
			ready = false
			a.log.WarnContext(ctx, "readiness check failed", "check", ck.Name(), "err", err.Error())
			continue
		}
		checks[ck.Name()] = "up"
	}

	if !ready {
		c.JSON(http.StatusServiceUnavailable, healthStatus{Status: "degraded", Checks: checks})
		return
	}
	c.JSON(http.StatusOK, healthStatus{Status: "ok", Checks: checks})
}
