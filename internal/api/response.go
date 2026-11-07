package api

import (
	"net/http"

	"github.com/gin-gonic/gin"

	"laborlaw/internal/port"
)

// Envelope 是所有 JSON 接口的唯一响应外形。
//
// 为什么统一包络：前端只需写一个解包函数和一个错误分支；压测脚本、验收演示、
// AI 生成的前端都按同一形状解析。分散的响应体必然长出四五种错误格式。
type Envelope struct {
	// Code 是业务码，取值见 contract/openapi.yaml 的 x-error-codes。
	Code int `json:"code"`
	// Message 是给人看的一句话。禁止拼接 err.Error()。
	Message string `json:"message"`
	// Retryable 让前端决定要不要显示"重试"按钮。
	Retryable bool `json:"retryable,omitempty"`
	// Data 是业务载荷，成功时才有值。
	Data any `json:"data,omitempty"`
	// Fields 只在 40001 出现，key 是 JSON 路径。
	Fields map[string]string `json:"fields,omitempty"`
	// TraceID 与响应头 X-Request-ID 一致，报障时让用户直接报这一串。
	TraceID string `json:"trace_id"`
}

// OK 写 200 + code=0。
func OK(c *gin.Context, data any) {
	c.JSON(http.StatusOK, Envelope{
		Code:    int(port.CodeOK),
		Message: "ok",
		Data:    data,
		TraceID: TraceID(c),
	})
}

// Accepted 写 202 + code=0。
//
// 为什么单独一个函数而不是复用 OK：202 与 200 的语义差一个数量级——200 表示
// "事情已经做完，data 就是结果"，202 表示"事情接下了，去轮询 task_id"。
// 前端（和一些 HTTP 客户端）会按 2xx 的细分码分流，混用会让它们把 202 当同步结果解析。
func Accepted(c *gin.Context, data any) {
	c.JSON(http.StatusAccepted, Envelope{
		Code:    int(port.CodeOK),
		Message: "accepted",
		Data:    data,
		TraceID: TraceID(c),
	})
}

// Fail 把任意 error 写成响应体与状态码。
//
// 只有 *port.AppError 的 Message / Fields 会进响应体，未分类错误一律折成 50001
// 的固定文案。原因很具体：合同原文、身份证号、上游原始报文经常出现在 err.Error()
// 里，直接回显就是一次数据泄漏，而这类泄漏在演示时根本看不出来。
func Fail(c *gin.Context, err error) {
	if err == nil {
		OK(c, nil)
		return
	}

	appErr := port.AsAppError(err)
	if appErr.Code == port.CodeInternal {
		// 只有兜底路径才打完整 cause；已经分类的错误由调用方自己决定要不要记。
		loggerFrom(c).Error("request failed",
			"trace_id", TraceID(c),
			"err", appErr.Error(),
		)
	}

	pub := appErr.Public()
	c.AbortWithStatusJSON(pub.Code.HTTPStatus(), Envelope{
		Code:      int(pub.Code),
		Message:   pub.Message,
		Retryable: pub.Retryable,
		Fields:    pub.Fields,
		TraceID:   TraceID(c),
	})
}

// FailCode 是构造 AppError 的语法糖，省掉每个 handler 里 port.New 的噪音。
func FailCode(c *gin.Context, code port.ErrorCode, format string, args ...any) {
	Fail(c, port.New(code, format, args...))
}
