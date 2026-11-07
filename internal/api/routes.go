package api

import (
	"net/http"

	"github.com/gin-gonic/gin"

	"laborlaw/internal/port"
)

// buildRouter 一次性把路由树建完。
//
// 中间件顺序即执行顺序：recovery 必须在最外层（它要能接住后面所有层的 panic），
// trace_id 必须在 access_log 之前（日志要带链路 ID），access_log 放最后
// （它要能看到最终状态码，包括后面中间件写下的 401/403）。
func (a *API) buildRouter() *gin.Engine {
	r := gin.New()
	// 必须显式设置。不调用时 gin 信任 0.0.0.0/0，ClientIP() 就无条件采信
	// X-Forwarded-For——登录限流按 IP 计数，这等于让客户端自己挑计数桶。
	// 配错（例如写了域名）时退回"谁都不信"，绝不带着默认值继续跑。
	if err := r.SetTrustedProxies(a.cfg.HTTP.TrustedProxies); err != nil {
		a.log.Error("TRUSTED_PROXIES 配置无效，退回只信任 RemoteAddr", "err", err.Error())
		if err := r.SetTrustedProxies(nil); err != nil {
			a.log.Error("关闭代理信任失败，ClientIP 仍按默认值工作", "err", err.Error())
		}
	}
	r.HandleMethodNotAllowed = true
	r.Use(recovery(a.log), traceID(), accessLog(a.log))

	// 探针不套 Envelope：k8s / docker healthcheck 只认 200/503 与简单的 JSON 形状。
	r.GET("/healthz", a.handleHealthz)
	r.GET("/readyz", a.handleReadyz)

	v1 := r.Group("/api/v1")

	// 公开接口：注册、登录、续期不需要令牌。
	//
	// 三条都挂 bodyLimit：它们是最容易被打的入口，而请求体本身不到 200 字节，
	// 没有任何正当理由让它们接受任意长度的输入。
	v1.POST("/auth/register", bodyLimit(maxAuthBodyBytes), a.register)
	v1.POST("/auth/login", bodyLimit(maxAuthBodyBytes), a.login)
	v1.POST("/auth/refresh", bodyLimit(maxAuthBodyBytes), a.refresh)

	// 受保护接口：分组挂 requireAuth，新增路由时不会漏鉴权。
	authed := v1.Group("", a.requireAuth())
	{
		authed.GET("/auth/me", a.me)
		authed.POST("/auth/logout", a.logout)

		authed.GET("/sessions", a.listSessions)
		authed.POST("/sessions", a.createSession)
		authed.GET("/sessions/:sessionId", a.getSession)
		authed.DELETE("/sessions/:sessionId", a.deleteSession)
		authed.GET("/sessions/:sessionId/messages", a.listMessages)

		authed.POST("/chat/completions", a.chatCompletions)

		authed.POST("/files", a.uploadFile)
		authed.GET("/files/:fileId", a.getFile)
		authed.GET("/files/:fileId/pages", a.getFilePages)
		authed.GET("/files/:fileId/content", a.downloadFile)

		authed.POST("/contracts/review", a.createReview)
		authed.GET("/reviews", a.listReviews)
		authed.GET("/reviews/:reviewId", a.getReview)
		authed.GET("/reviews/:reviewId/findings", a.listFindings)

		authed.GET("/tasks/:taskId", a.getTask)
		authed.POST("/tasks/:taskId/cancel", a.cancelTask)

		authed.GET("/statutes", a.listStatutes)
		authed.GET("/statutes/:statuteId", a.getStatute)
	}

	r.NoRoute(func(c *gin.Context) {
		FailCode(c, port.CodeNotFound, "接口不存在")
	})
	r.NoMethod(func(c *gin.Context) {
		// 405 不是错误码表里的码，但必须有人接：否则 gin 会返回纯文本，
		// 前端按 JSON 解析会直接抛异常。
		c.AbortWithStatusJSON(http.StatusMethodNotAllowed, Envelope{
			Code:    int(port.CodeInvalidArgument),
			Message: "该路径不支持此方法",
			TraceID: TraceID(c),
		})
	})

	return r
}
