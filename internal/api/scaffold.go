package api

import (
	"net/http"
	"strings"

	"github.com/gin-gonic/gin"

	"laborlaw/internal/auth"
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
// 令牌解析必须排在 DevUser 之前，且解析失败**不许回落**到 DevUser：
// 早先的实现是"只要配了 AUTH_DEV_USER 就放行任何人"，于是本地联调时
// 一个过期令牌也会被当成合法用户——这类洞在演示里看不出来，
// 只会在换到真实环境时变成"为什么谁都能进"。
// 现在的顺序：带了 Authorization 就必须是有效令牌；只有完全不带该头时才用 DevUser。
// production 下 config.Validate 禁止 AUTH_DEV_USER，所以线上这里只有令牌一条路。
func (a *API) requireAuth() gin.HandlerFunc {
	return func(c *gin.Context) {
		if raw := strings.TrimSpace(c.GetHeader("Authorization")); raw != "" {
			const prefix = "bearer "
			if len(raw) <= len(prefix) || !strings.EqualFold(raw[:len(prefix)], prefix) {
				Fail(c, port.New(port.CodeUnauthorized, msgUnauthorized))
				c.Abort()
				return
			}
			claims, err := auth.Parse(a.cfg.Auth.Secret, a.cfg.Auth.Issuer,
				strings.TrimSpace(raw[len(prefix):]), auth.TypAccess)
			if err != nil {
				// 失败原因只进日志：alg=none / 过期 / 伪造 对前端是同一件事——
				// 重新登录。把 err 回显给调用方等于给探测者一份免费的诊断报告。
				loggerFrom(c).Warn("访问令牌校验失败", "trace_id", TraceID(c), "err", err.Error())
				Fail(c, port.New(port.CodeUnauthorized, msgUnauthorized))
				c.Abort()
				return
			}
			c.Set(ctxUserID, claims.Sub)
			c.Next()
			return
		}

		if a.cfg.Auth.DevUser != "" {
			c.Set(ctxUserID, a.cfg.Auth.DevUser)
			c.Next()
			return
		}
		Fail(c, port.New(port.CodeUnauthorized, msgNeedLogin))
		c.Abort()
	}
}
