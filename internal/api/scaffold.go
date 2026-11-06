package api

import (
	"net/http"

	"github.com/gin-gonic/gin"

	"laborlaw/internal/port"
)

// notImplemented 是 M1 阶段的占位 handler。
//
// 为什么返回 501 而不是干脆不注册路由：404 在联调时会被误判成"路径写错了"，
// 501 明确表示"路由在、实现还没接"，两端对进度的认知不会错位。
// 每接一个真实 handler 就替换掉对应的一行，这个文件在 M3 结束时应当为空。
func (a *API) notImplemented(feature string) gin.HandlerFunc {
	return func(c *gin.Context) {
		c.AbortWithStatusJSON(http.StatusNotImplemented, Envelope{
			Code:    int(port.CodeInternal),
			Message: feature + "接口尚未实现",
			TraceID: TraceID(c),
		})
	}
}

// requireAuth 把当前用户写进上下文，后续 handler 只认 UserID(c)。
//
// M1 阶段只有一种来源：配置里的 AUTH_DEV_USER。真正的 JWT 校验要等
// internal/store 的 UserStore 落地——没有用户表，签出来的令牌无法验证归属，
// 现在写一套"看起来能验"的代码只会把安全缺口藏到联调之后。
// production 下 config.Validate 禁止 AUTH_DEV_USER，所以线上这里只会返回 40101。
func (a *API) requireAuth() gin.HandlerFunc {
	return func(c *gin.Context) {
		if a.cfg.Auth.DevUser != "" {
			c.Set(ctxUserID, a.cfg.Auth.DevUser)
			c.Next()
			return
		}
		Fail(c, port.New(port.CodeUnauthorized, "请先登录"))
		c.Abort()
	}
}
