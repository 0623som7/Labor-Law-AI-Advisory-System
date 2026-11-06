package api

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"log/slog"
	"net/http"
	"runtime/debug"
	"strings"
	"time"

	"github.com/gin-gonic/gin"

	"laborlaw/internal/port"
)

// gin.Context 的 key 一律用常量，避免某一层写 "traceID"、另一层读 "trace_id"。
const (
	ctxTraceID = "trace_id"
	ctxUserID  = "user_id"
	ctxLogger  = "logger"
)

// requestIDHeader 与 Nginx / 云网关的约定一致，便于跨层串联同一条链路。
const requestIDHeader = "X-Request-ID"

// TraceID 读取当前请求的链路 ID。handler、日志、错误响应都用它做关联。
func TraceID(c *gin.Context) string {
	return c.GetString(ctxTraceID)
}

// UserID 返回当前登录用户；未登录返回空串。
func UserID(c *gin.Context) string {
	return c.GetString(ctxUserID)
}

func loggerFrom(c *gin.Context) *slog.Logger {
	if v, ok := c.Get(ctxLogger); ok {
		if l, ok := v.(*slog.Logger); ok {
			return l
		}
	}
	return slog.Default()
}

// traceID 复用上游传入的 X-Request-ID，没有才自己生成。
//
// 复用是必须的：Nginx 与网关各自可能已经生成过一个 ID，重生成会让"同一请求"
// 在两层日志里对不上，而跨层追踪正是线上排障的第一手段。
func traceID() gin.HandlerFunc {
	return func(c *gin.Context) {
		id := strings.TrimSpace(c.GetHeader(requestIDHeader))
		if !validTraceID(id) {
			id = newTraceID()
		}
		c.Set(ctxTraceID, id)
		c.Header(requestIDHeader, id)
		c.Next()
	}
}

// validTraceID 校验外部传入的 ID：长度与字符都受限，防止把日志注入进 header。
func validTraceID(s string) bool {
	if len(s) < 8 || len(s) > 64 {
		return false
	}
	for _, r := range s {
		switch {
		case r >= '0' && r <= '9',
			r >= 'a' && r <= 'z',
			r >= 'A' && r <= 'Z',
			r == '-', r == '_':
		default:
			return false
		}
	}
	return true
}

// newTraceID 生成 96 位随机 ID。用 crypto/rand 而不是 math/rand：
// 链路 ID 会进日志与响应头，可预测的值在多租户场景下能被用来串请求。
func newTraceID() string {
	var b [12]byte
	if _, err := rand.Read(b[:]); err != nil {
		// 熵池异常时退化成时间戳：宁可 ID 不够随机，也不能让请求直接失败。
		return fmt.Sprintf("%x", time.Now().UnixNano())
	}
	return hex.EncodeToString(b[:])
}

// accessLog 记录一条结构化访问日志。
//
// 只记 method / path / status / latency / trace_id：请求体与查询串一律不记，
// 合同原文、手机号、身份证号都可能出现在那两处。
func accessLog(log *slog.Logger) gin.HandlerFunc {
	return func(c *gin.Context) {
		start := time.Now()
		c.Set(ctxLogger, log)
		c.Next()

		path := c.FullPath()
		if path == "" {
			// 404 没有匹配到路由，FullPath 是空串，退回原始路径。
			path = c.Request.URL.Path
		}
		attrs := []any{
			"method", c.Request.Method,
			"path", path,
			"status", c.Writer.Status(),
			"latency_ms", time.Since(start).Milliseconds(),
			"trace_id", TraceID(c),
		}
		if uid := UserID(c); uid != "" {
			attrs = append(attrs, "user_id", uid)
		}

		switch {
		case c.Writer.Status() >= http.StatusInternalServerError:
			log.Error("http", attrs...)
		case c.Writer.Status() >= http.StatusBadRequest:
			log.Warn("http", attrs...)
		default:
			log.Info("http", attrs...)
		}
	}
}

// recovery 捕获 handler 里的 panic 并折成 50001。
//
// 库代码禁止主动 panic，但 gin 的中间件链与第三方 JSON 编码仍可能在运行时抛出；
// 没有这一层，一个空指针就会带走整个进程，而 Go 的默认行为是不打印请求上下文。
// 必须挂在最外层：挂在里面只能接住它之后的那些层。
func recovery(log *slog.Logger) gin.HandlerFunc {
	return func(c *gin.Context) {
		defer func() {
			r := recover()
			if r == nil {
				return
			}
			log.Error("panic recovered",
				"trace_id", TraceID(c),
				"panic", fmt.Sprint(r),
				"stack", string(debug.Stack()),
			)
			// 响应已写出时不能再写一次头，否则 net/http 会打 "superfluous WriteHeader"。
			if c.Writer.Written() {
				c.Abort()
				return
			}
			c.AbortWithStatusJSON(http.StatusInternalServerError, Envelope{
				Code:    int(port.CodeInternal),
				Message: "服务内部错误",
				TraceID: TraceID(c),
			})
		}()
		c.Next()
	}
}
