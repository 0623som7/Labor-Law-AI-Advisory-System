package api

import (
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"

	"laborlaw/internal/port"
)

// newFailRouter 把 Fail 挂在一个最小路由上，避免每个用例都起一遍完整路由树。
func newFailRouter() *gin.Engine {
	gin.SetMode(gin.TestMode)
	r := gin.New()
	r.Use(traceID(), accessLog(slog.New(slog.NewTextHandler(io.Discard, nil))))
	return r
}

func TestOKEnvelope(t *testing.T) {
	r := newFailRouter()
	r.GET("/ok", func(c *gin.Context) { OK(c, map[string]string{"hello": "world"}) })

	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/ok", nil))

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	var env Envelope
	if err := json.Unmarshal(rec.Body.Bytes(), &env); err != nil {
		t.Fatalf("响应不是 JSON: %v", err)
	}
	if env.Code != 0 || env.Message != "ok" {
		t.Fatalf("code/message = %d/%q, want 0/ok", env.Code, env.Message)
	}
	if env.TraceID == "" {
		t.Fatal("trace_id 为空")
	}
}

func TestFailMapping(t *testing.T) {
	tests := []struct {
		name       string
		err        error
		wantStatus int
		wantCode   int
		wantFields bool
	}{
		{
			name:       "参数错带字段",
			err:        port.New(port.CodeInvalidArgument, "手机号格式不对").WithField("phone", "必须是 11 位"),
			wantStatus: http.StatusBadRequest,
			wantCode:   40001,
			wantFields: true,
		},
		{
			name:       "未登录",
			err:        port.New(port.CodeUnauthorized, "请先登录"),
			wantStatus: http.StatusUnauthorized,
			wantCode:   40101,
		},
		{
			name:       "越权",
			err:        port.New(port.CodeForbidden, "无权访问"),
			wantStatus: http.StatusForbidden,
			wantCode:   40301,
		},
		{
			name:       "不存在",
			err:        port.New(port.CodeNotFound, "资源不存在"),
			wantStatus: http.StatusNotFound,
			wantCode:   40401,
		},
		{
			name:       "冲突",
			err:        port.New(port.CodeConflict, "任务已完成"),
			wantStatus: http.StatusConflict,
			wantCode:   40901,
		},
		{
			name:       "文件过大",
			err:        port.New(port.CodeFileTooLarge, "超过 5MB"),
			wantStatus: http.StatusRequestEntityTooLarge,
			wantCode:   41301,
		},
		{
			name:       "类型不支持",
			err:        port.New(port.CodeUnsupportedMedia, "只接受 PDF"),
			wantStatus: http.StatusUnsupportedMediaType,
			wantCode:   41501,
		},
		{
			name:       "无文本层",
			err:        port.New(port.CodeNoTextLayer, "这是扫描件"),
			wantStatus: http.StatusUnprocessableEntity,
			wantCode:   42201,
		},
		{
			name:       "限流",
			err:        port.New(port.CodeRateLimited, "请求过于频繁"),
			wantStatus: http.StatusTooManyRequests,
			wantCode:   42901,
		},
		{
			name:       "上游超时",
			err:        port.New(port.CodeUpstreamTimeout, "模型超时"),
			wantStatus: http.StatusServiceUnavailable,
			wantCode:   50301,
		},
		{
			name:       "未分类错误折成 50001",
			err:        errors.New("mysql: connection refused"),
			wantStatus: http.StatusInternalServerError,
			wantCode:   50001,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			r := newFailRouter()
			r.GET("/fail", func(c *gin.Context) { Fail(c, tc.err) })

			rec := httptest.NewRecorder()
			r.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/fail", nil))

			if rec.Code != tc.wantStatus {
				t.Fatalf("status = %d, want %d", rec.Code, tc.wantStatus)
			}
			var env Envelope
			if err := json.Unmarshal(rec.Body.Bytes(), &env); err != nil {
				t.Fatalf("响应不是 JSON: %v", err)
			}
			if env.Code != tc.wantCode {
				t.Fatalf("code = %d, want %d", env.Code, tc.wantCode)
			}
			if tc.wantFields && len(env.Fields) == 0 {
				t.Fatal("期望带 fields，实际为空")
			}
			if !tc.wantFields && len(env.Fields) != 0 {
				t.Fatalf("不该带 fields，实际 %v", env.Fields)
			}
		})
	}
}

// TestFailDoesNotLeakCause 是防止敏感信息出网的最后一道闸。
//
// 合同原文、身份证号、上游报文经常在 cause 里；只要有人把 err.Error() 拼进
// Message，这里就会红。
func TestFailDoesNotLeakCause(t *testing.T) {
	secret := "110101199003074512"
	inner := errors.New("parse contract: 甲方 " + secret + " 违约")

	r := newFailRouter()
	r.GET("/fail", func(c *gin.Context) {
		Fail(c, port.Wrap(port.CodeNoTextLayer, inner, "合同解析失败"))
	})

	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/fail", nil))

	if strings.Contains(rec.Body.String(), secret) {
		t.Fatalf("响应体泄漏了 cause 中的敏感信息: %q", rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "合同解析失败") {
		t.Fatalf("响应体丢掉了面向用户的文案: %q", rec.Body.String())
	}
}
