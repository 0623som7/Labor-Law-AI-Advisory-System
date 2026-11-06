package port

import (
	"errors"
	"fmt"
)

// ErrorCode 是穿过 API 边界的稳定错误码，数值与响应体里的 code 字段逐位相同。
//
// 为什么不用 error 类型或 HTTP status 当业务码：前端要根据 code 决定交互（跳登录、弹重试、
// 提示压缩文件），HTTP status 一个值对应多种业务原因（400 既是参数错也是文件错），
// 而字符串 error 会被随手改文案改坏契约。数值码是唯一能同时喂给前端 switch 和运维告警的东西。
type ErrorCode int

// 错误码全表，与 contract/openapi.yaml 的 x-error-codes 段一一对应。
// 新增错误码属于"加"，但必须同步 openapi 与前端 switch，否则前端会走到 default 分支。
const (
	// CodeOK 不是错误，只用于响应包络的 code 字段。
	CodeOK ErrorCode = 0

	// CodeInvalidArgument 参数校验失败，Fields 里带字段级原因。
	CodeInvalidArgument ErrorCode = 40001
	// CodeUnauthorized 未登录或 token 失效。刻意不区分"用户不存在"与"密码错"，防账号枚举。
	CodeUnauthorized ErrorCode = 40101
	// CodeForbidden 已登录但越权访问他人资源。
	// 用 403 而不是 404，是因为排查线上问题时"权限错"和"资源没了"必须一眼分开。
	CodeForbidden ErrorCode = 40301
	// CodeNotFound 资源不存在。
	CodeNotFound ErrorCode = 40401
	// CodeConflict 重复提交，或任务状态机收到非法转移（如 pending → succeeded）。
	CodeConflict ErrorCode = 40901
	// CodeFileTooLarge 文件超过 10MB 或 100 页。
	CodeFileTooLarge ErrorCode = 41301
	// CodeUnsupportedMedia 文件类型不支持（非 pdf）。
	CodeUnsupportedMedia ErrorCode = 41501
	// CodeNoTextLayer PDF 无可抽取文本层（扫描件），OCR 兜底也失败。
	CodeNoTextLayer ErrorCode = 42201
	// CodeRateLimited 限流。可重试，前端应退避。
	CodeRateLimited ErrorCode = 42901
	// CodeInternal 未归类的内部错误。出现即代表有缺陷，需要带 trace_id 上报。
	CodeInternal ErrorCode = 50001
	// CodeRuleEngine 单条规则执行异常。携带 rule_id，只让那一条规则降级，不许整体失败。
	CodeRuleEngine ErrorCode = 50002
	// CodeUpstream LLM 上游返回错误。可重试（≤2 次）。
	CodeUpstream ErrorCode = 50201
	// CodeUpstreamTimeout LLM 上游超时。可重试。
	CodeUpstreamTimeout ErrorCode = 50301
	// CodeTaskTimeout 审查任务超过 worker 超时（默认 120s）。可重试，或转异步。
	CodeTaskTimeout ErrorCode = 50401
)

// HTTPStatus 把业务码映射成 HTTP 状态码。
//
// 为什么放在 port 而不是 api 层：B 在 Agent 里也要构造错误码（比如检索失败转 50002），
// 他需要知道这个码最终会以什么 HTTP 形态出去，才能判断前端会不会重试。
// 未登记的码一律 500，避免手写漏项时把错误伪装成 2xx。
func (c ErrorCode) HTTPStatus() int {
	switch c {
	case CodeOK:
		return 200
	case CodeInvalidArgument:
		return 400
	case CodeUnauthorized:
		return 401
	case CodeForbidden:
		return 403
	case CodeNotFound:
		return 404
	case CodeConflict:
		return 409
	case CodeFileTooLarge:
		return 413
	case CodeUnsupportedMedia:
		return 415
	case CodeNoTextLayer:
		return 422
	case CodeRateLimited:
		return 429
	case CodeInternal, CodeRuleEngine:
		return 500
	case CodeUpstream:
		return 502
	case CodeUpstreamTimeout:
		return 503
	case CodeTaskTimeout:
		return 504
	default:
		return 500
	}
}

// Retryable 表示调用方是否可以对同一个请求原样重试。
//
// 只对"重试有可能得到不同结果"的码返回 true：限流、上游故障、上游超时、任务超时、内部错误。
// 参数错、鉴权错、文件错重试一万次也是同样的结果，把它们标成可重试会让前端陷入重试风暴。
func (c ErrorCode) Retryable() bool {
	switch c {
	case CodeRateLimited, CodeInternal, CodeUpstream, CodeUpstreamTimeout, CodeTaskTimeout:
		return true
	default:
		return false
	}
}

// String 让错误码在日志里以 "50201" 而不是数字字面量出现，便于 grep。
func (c ErrorCode) String() string {
	return fmt.Sprintf("%d", int(c))
}

// AppError 是唯一允许跨层传递的业务错误。
//
// 它同时承载两份信息，且刻意分成两个出口：
//   - Error()：给日志和上报用，包含 cause 链，允许携带内部细节。
//   - Public()：给 HTTP 响应用，只含 Message 与 Fields，绝不含 cause。
//
// 这样拆是因为合同原文、身份证号、上游原始报文经常出现在 cause 里，
// 而 Message 由调用方用固定文案构造，天然不会泄漏。禁止把 Error() 的返回值直接写进响应体。
type AppError struct {
	// Code 是稳定错误码，前端与告警都按它分流。
	Code ErrorCode
	// Message 是面向前端的可读文案。禁止拼接 err.Error()、合同原文、手机号、身份证号。
	Message string
	// Fields 是参数校验场景下的字段级原因，key 用 JSON 路径（如 "compensation.monthly_base"）。
	// 只有 CodeInvalidArgument 会填它。
	Fields map[string]string

	// cause 是内部链路，永不出口。用 errors.Is / errors.As 透传，配合 %w 保留原始栈。
	cause error
}

// 编译期断言：AppError 必须能用 errors.Is / errors.As 处理。
var _ error = (*AppError)(nil)

// New 构造一个不带底层 cause 的 AppError。
//
// format 走 fmt.Sprintf，因此 Message 里禁止出现 %v 拼接的外部输入。
func New(code ErrorCode, format string, args ...any) *AppError {
	return &AppError{Code: code, Message: fmt.Sprintf(format, args...)}
}

// Wrap 用底层 error 构造 AppError，并把 err 挂到 cause 上。
//
// err 为 nil 时退化成 New 的行为——这样调用方不必为了"有时有 cause 有时没有"写两套分支。
// 典型用法：return port.Wrap(port.CodeNoTextLayer, err, "pdf 第 %d 页无文本层", page)
func Wrap(code ErrorCode, err error, format string, args ...any) *AppError {
	e := &AppError{Code: code, Message: fmt.Sprintf(format, args...)}
	if err != nil {
		e.cause = err
	}
	return e
}

// WithField 返回带一条字段级原因的副本。
//
// 返回副本而不是原地改，是因为 AppError 会被重试逻辑和多个 goroutine 共享读，
// 原地改 Fields 会让并发的校验分支互相串字段。
func (e *AppError) WithField(field, reason string) *AppError {
	clone := &AppError{Code: e.Code, Message: e.Message, cause: e.cause}
	clone.Fields = make(map[string]string, len(e.Fields)+1)
	for k, v := range e.Fields {
		clone.Fields[k] = v
	}
	clone.Fields[field] = reason
	return clone
}

// Error 实现 error 接口，输出给日志用，含 cause。
//
// 不要把这个字符串写进 HTTP 响应：它可能含上游原始报文与业务原文。
func (e *AppError) Error() string {
	if e == nil {
		return "<nil>"
	}
	if e.cause != nil {
		return fmt.Sprintf("port: code=%d message=%q: %v", int(e.Code), e.Message, e.cause)
	}
	return fmt.Sprintf("port: code=%d message=%q", int(e.Code), e.Message)
}

// Unwrap 让 errors.Is / errors.As 能继续往下钻到 cause。
func (e *AppError) Unwrap() error {
	if e == nil {
		return nil
	}
	return e.cause
}

// Is 支持 errors.Is(err, &port.AppError{Code: port.CodeUpstream}) 这种按码匹配。
//
// 只比 Code，不比 Message：文案随时会改，比对文案的测试会在改错别字时挂掉。
func (e *AppError) Is(target error) bool {
	if e == nil || target == nil {
		return false
	}
	var t *AppError
	if !errors.As(target, &t) {
		return false
	}
	return t != nil && e.Code == t.Code
}

// PublicError 是可以直接序列化成响应体的安全视图。
type PublicError struct {
	// Code 是稳定错误码。
	Code ErrorCode `json:"code"`
	// Message 是面向用户的文案。
	Message string `json:"message"`
	// Retryable 告诉前端要不要给"重试"按钮。
	Retryable bool `json:"retryable"`
	// Fields 只在参数校验失败时出现。
	Fields map[string]string `json:"fields,omitempty"`
}

// Public 返回不含 cause 的安全视图，API 层只能把它写进响应体。
func (e *AppError) Public() PublicError {
	if e == nil {
		return PublicError{Code: CodeOK, Message: "ok"}
	}
	return PublicError{
		Code:      e.Code,
		Message:   e.Message,
		Retryable: e.Code.Retryable(),
		Fields:    e.Fields,
	}
}

// CodeOf 取出错误链上第一个 AppError 的码。
//
// 约定：err 为 nil 返回 CodeOK；非 AppError 的错误返回 CodeInternal。
// 把"没分类的错误"折成 50001 而不是 0，是为了让漏包的 error 一定以 5xx 暴露出来，
// 而不是被当成成功响应静默吞掉。
func CodeOf(err error) ErrorCode {
	if err == nil {
		return CodeOK
	}
	var ae *AppError
	if errors.As(err, &ae) && ae != nil {
		return ae.Code
	}
	return CodeInternal
}

// IsCode 沿错误链判断是否是指定码，等价于 CodeOf 的比较但不需要构造 target。
func IsCode(err error, code ErrorCode) bool {
	return CodeOf(err) == code
}

// IsRetryable 判断这个错误是否值得重试。worker 与 llm 的重试循环只认这个函数。
func IsRetryable(err error) bool {
	if err == nil {
		return false
	}
	var ae *AppError
	if errors.As(err, &ae) && ae != nil {
		return ae.Code.Retryable()
	}
	// 未分类错误按不可重试处理：盲重试会把一个确定性 bug 变成 N 倍的资源消耗。
	return false
}

// AsAppError 把任意 error 提取成 *AppError，提取不到就包一个 CodeInternal。
//
// API 层的错误中间件用它做兜底：既保证响应体一定有 code，又保留原始 cause 供日志。
func AsAppError(err error) *AppError {
	if err == nil {
		return nil
	}
	var ae *AppError
	if errors.As(err, &ae) && ae != nil {
		return ae
	}
	// Message 用固定文案，不拼接 err.Error()——兜底路径最容易被塞进敏感原文。
	return Wrap(CodeInternal, err, "服务内部错误")
}
