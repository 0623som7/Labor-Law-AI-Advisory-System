package port

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
)

// 错误码表的断言测试：HTTP status 与 retryable 是前后端两个系统的共同依赖，
// 手改一处就会让前端把"参数错"当"可重试"，所以逐条钉死。
func TestErrorCodeHTTPStatus(t *testing.T) {
	cases := []struct {
		name string
		code ErrorCode
		want int
	}{
		{"成功", CodeOK, 200},
		{"参数校验失败", CodeInvalidArgument, 400},
		{"未登录", CodeUnauthorized, 401},
		{"越权", CodeForbidden, 403},
		{"资源不存在", CodeNotFound, 404},
		{"状态冲突", CodeConflict, 409},
		{"文件超限", CodeFileTooLarge, 413},
		{"类型不支持", CodeUnsupportedMedia, 415},
		{"无文本层", CodeNoTextLayer, 422},
		{"限流", CodeRateLimited, 429},
		{"内部错误", CodeInternal, 500},
		{"规则引擎异常", CodeRuleEngine, 500},
		{"上游失败", CodeUpstream, 502},
		{"上游超时", CodeUpstreamTimeout, 503},
		{"任务超时", CodeTaskTimeout, 504},
		{"未登记的码折成500", ErrorCode(99999), 500},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := c.code.HTTPStatus(); got != c.want {
				t.Fatalf("HTTPStatus() = %d, want %d", got, c.want)
			}
		})
	}
}

func TestErrorCodeRetryable(t *testing.T) {
	cases := []struct {
		name string
		code ErrorCode
		want bool
	}{
		{"限流可重试", CodeRateLimited, true},
		{"内部错误可重试", CodeInternal, true},
		{"上游失败可重试", CodeUpstream, true},
		{"上游超时可重试", CodeUpstreamTimeout, true},
		{"任务超时可重试", CodeTaskTimeout, true},
		{"参数错不可重试", CodeInvalidArgument, false},
		{"鉴权错不可重试", CodeUnauthorized, false},
		{"越权不可重试", CodeForbidden, false},
		{"文件超限不可重试", CodeFileTooLarge, false},
		{"无文本层不可重试", CodeNoTextLayer, false},
		{"规则引擎异常不可重试", CodeRuleEngine, false},
		{"成功不算重试", CodeOK, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := c.code.Retryable(); got != c.want {
				t.Fatalf("Retryable() = %v, want %v", got, c.want)
			}
		})
	}
}

// Public() 是唯一能进响应体的出口，必须保证 cause 一滴不漏。
func TestAppErrorPublicDropsCause(t *testing.T) {
	const secret = "甲方：张三，身份证 441900199001011234"
	cause := errors.New(secret)
	err := Wrap(CodeNoTextLayer, cause, "pdf 第 %d 页无文本层", 2)

	pub := err.Public()
	if pub.Code != CodeNoTextLayer {
		t.Fatalf("Code = %d, want %d", pub.Code, CodeNoTextLayer)
	}
	if pub.Message != "pdf 第 2 页无文本层" {
		t.Fatalf("Message = %q", pub.Message)
	}
	if pub.Retryable {
		t.Fatal("42201 不应可重试")
	}
	// 整个响应视图序列化后都不许出现 cause 的任何片段。
	raw, merr := json.Marshal(pub)
	if merr != nil {
		t.Fatalf("PublicError 必须可序列化: %v", merr)
	}
	if strings.Contains(string(raw), "441900199001011234") || strings.Contains(pub.Message, "441900199001011234") {
		t.Fatalf("Public() 泄漏了 cause: %s", raw)
	}
	// 但 Error()（日志用）必须保留 cause，否则线上查不到根因。
	if !strings.Contains(err.Error(), "441900199001011234") {
		t.Fatal("Error() 应保留 cause 供日志使用")
	}
}

func TestCodeOf(t *testing.T) {
	wrapped := Wrap(CodeUpstream, errors.New("upstream 502"), "模型上游失败")
	twice := fmt.Errorf("agent 节点失败: %w", wrapped)

	cases := []struct {
		name string
		err  error
		want ErrorCode
	}{
		{"nil 视为成功", nil, CodeOK},
		{"裸 AppError", New(CodeNotFound, "不存在"), CodeNotFound},
		{"一层 wrap", wrapped, CodeUpstream},
		{"两层 wrap", twice, CodeUpstream},
		{"普通 error 折成内部错误", errors.New("boom"), CodeInternal},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := CodeOf(c.err); got != c.want {
				t.Fatalf("CodeOf() = %d, want %d", got, c.want)
			}
		})
	}
}

func TestIsCodeAndIsRetryable(t *testing.T) {
	err := Wrap(CodeUpstream, errors.New("502"), "上游失败")
	if !IsCode(err, CodeUpstream) {
		t.Fatal("IsCode 应命中 CodeUpstream")
	}
	if IsCode(err, CodeNotFound) {
		t.Fatal("IsCode 不该命中 CodeNotFound")
	}
	if !IsRetryable(err) {
		t.Fatal("50201 应可重试")
	}
	// 未分类错误盲重试会把确定性 bug 放大成 N 倍资源消耗，必须判为不可重试。
	if IsRetryable(errors.New("boom")) {
		t.Fatal("未分类错误不该判为可重试")
	}
	if IsRetryable(nil) {
		t.Fatal("nil 不该判为可重试")
	}
}

func TestAppErrorIsMatchesByCodeOnly(t *testing.T) {
	err := New(CodeForbidden, "无权访问")
	// 文案不同、码相同 → 必须匹配（文案会改，比对文案的测试一改错别字就挂）。
	if !errors.Is(err, &AppError{Code: CodeForbidden, Message: "另一个文案"}) {
		t.Fatal("errors.Is 应按 Code 匹配")
	}
	if errors.Is(err, &AppError{Code: CodeNotFound}) {
		t.Fatal("errors.Is 不该跨码匹配")
	}
}

func TestWrapNilError(t *testing.T) {
	err := Wrap(CodeInternal, nil, "无 cause")
	if err.Unwrap() != nil {
		t.Fatal("nil cause 时 Unwrap 应返回 nil")
	}
	if err.Error() == "" {
		t.Fatal("Error() 不应为空")
	}
}

// WithField 返回副本：并发校验分支共享同一个基础错误时不能互相串字段。
func TestWithFieldIsolatesCopy(t *testing.T) {
	base := New(CodeInvalidArgument, "参数校验失败")
	a := base.WithField("monthly_base", "必须大于 0")
	b := base.WithField("start", "格式应为 2006-01-02")

	if len(base.Fields) != 0 {
		t.Fatalf("原始错误被污染，Fields=%v", base.Fields)
	}
	if a.Fields["monthly_base"] == "" || len(b.Fields) != 1 {
		t.Fatalf("副本字段错: a=%v b=%v", a.Fields, b.Fields)
	}
	if _, ok := a.Fields["start"]; ok {
		t.Fatal("两个副本互相串字段")
	}
}

func TestAsAppError(t *testing.T) {
	if got := AsAppError(nil); got != nil {
		t.Fatalf("nil 应返回 nil，实际 %v", got)
	}
	inner := fmt.Errorf("db: connection refused")
	got := AsAppError(inner)
	if got.Code != CodeInternal {
		t.Fatalf("Code = %d, want %d", got.Code, CodeInternal)
	}
	if got.Message != "服务内部错误" {
		t.Fatalf("兜底 Message 必须是固定文案，实际 %q", got.Message)
	}
	if !errors.Is(got, inner) {
		t.Fatal("兜底必须保留 cause 供日志追溯")
	}
}
