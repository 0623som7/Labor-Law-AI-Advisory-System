package api

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"

	"laborlaw/internal/config"
)

// discardLogger 让测试输出只保留断言结果，不被每条访问日志淹没。
func discardLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// testConfig 用 staging 而不是 dev，是为了让 gin 走 ReleaseMode：
// 测试输出里不该混进几十行 [GIN-debug] 路由注册日志。
func testConfig(devUser string) config.Config {
	return config.Config{
		App:  config.AppConfig{Env: config.EnvStaging, Name: "test", Version: "test"},
		HTTP: config.HTTPConfig{Addr: ":0"},
		Auth: config.AuthConfig{DevUser: devUser},
	}
}

func newTestAPI(t *testing.T, devUser string, checkers ...Checker) http.Handler {
	t.Helper()
	gin.SetMode(gin.TestMode)
	return New(testConfig(devUser), discardLogger(), checkers...).Handler()
}

// doRequest 发一个请求并解出包络。非 JSON 响应会在测试里直接失败，
// 因为前端一定按 JSON 解析——返回纯文本就等于前端崩。
func doRequest(t *testing.T, h http.Handler, method, path string, headers map[string]string) (*httptest.ResponseRecorder, Envelope) {
	t.Helper()

	req := httptest.NewRequest(method, path, nil)
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	var env Envelope
	if err := json.Unmarshal(rec.Body.Bytes(), &env); err != nil {
		t.Fatalf("%s %s 的响应不是 JSON: %v，body=%q", method, path, err, rec.Body.String())
	}
	return rec, env
}

type stubChecker struct {
	name string
	err  error
}

func (s stubChecker) Name() string                  { return s.name }
func (s stubChecker) Check(_ context.Context) error { return s.err }

func TestHealthz(t *testing.T) {
	h := newTestAPI(t, "")

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/healthz", nil))

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), `"status":"ok"`) {
		t.Fatalf("body = %q, want status ok", rec.Body.String())
	}
}

func TestReadyz(t *testing.T) {
	tests := []struct {
		name       string
		checkers   []Checker
		wantStatus int
		wantBody   string
	}{
		{
			name:       "没有依赖时直接就绪",
			checkers:   nil,
			wantStatus: http.StatusOK,
			wantBody:   `"status":"ok"`,
		},
		{
			name:       "依赖全部可用",
			checkers:   []Checker{stubChecker{name: "mysql"}, stubChecker{name: "redis"}},
			wantStatus: http.StatusOK,
			wantBody:   `"mysql":"up"`,
		},
		{
			name:       "单个依赖不可用",
			checkers:   []Checker{stubChecker{name: "mysql"}, stubChecker{name: "redis", err: errors.New("dial tcp: refused")}},
			wantStatus: http.StatusServiceUnavailable,
			wantBody:   `"status":"degraded"`,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			h := newTestAPI(t, "", tc.checkers...)
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/readyz", nil))

			if rec.Code != tc.wantStatus {
				t.Fatalf("status = %d, want %d", rec.Code, tc.wantStatus)
			}
			if !strings.Contains(rec.Body.String(), tc.wantBody) {
				t.Fatalf("body = %q, want 包含 %q", rec.Body.String(), tc.wantBody)
			}
		})
	}
}

func TestNotFoundReturnsEnvelope(t *testing.T) {
	h := newTestAPI(t, "")
	rec, env := doRequest(t, h, http.MethodGet, "/api/v1/no-such-route", nil)

	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", rec.Code)
	}
	if env.Code != 40401 {
		t.Fatalf("code = %d, want 40401", env.Code)
	}
	if env.TraceID == "" {
		t.Fatal("trace_id 为空：错误响应也必须带链路 ID")
	}
}

func TestMethodNotAllowedReturnsEnvelope(t *testing.T) {
	h := newTestAPI(t, "")
	// /auth/login 只注册了 POST，用 GET 打它应当拿到 405 而不是 404。
	rec, env := doRequest(t, h, http.MethodGet, "/api/v1/auth/login", nil)

	if rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("status = %d, want 405", rec.Code)
	}
	if env.Code != 40001 {
		t.Fatalf("code = %d, want 40001", env.Code)
	}
}

func TestProtectedRouteRequiresAuth(t *testing.T) {
	h := newTestAPI(t, "")
	rec, env := doRequest(t, h, http.MethodGet, "/api/v1/sessions", nil)

	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", rec.Code)
	}
	if env.Code != 40101 {
		t.Fatalf("code = %d, want 40101", env.Code)
	}
	if env.Retryable {
		t.Fatal("40101 不该标记为可重试：重试一万次也是 401")
	}
}

func TestDevUserReachesScaffold(t *testing.T) {
	h := newTestAPI(t, "dev-user-0001")
	rec, env := doRequest(t, h, http.MethodGet, "/api/v1/sessions", nil)

	if rec.Code != http.StatusNotImplemented {
		t.Fatalf("status = %d, want 501", rec.Code)
	}
	if !strings.Contains(env.Message, "尚未实现") {
		t.Fatalf("message = %q, want 包含 尚未实现", env.Message)
	}
}

func TestPanicRecovery(t *testing.T) {
	a := New(testConfig(""), nil)
	a.engine.GET("/boom", func(*gin.Context) {
		panic("数据库连接池空了")
	})

	rec := httptest.NewRecorder()
	a.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/boom", nil))

	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500", rec.Code)
	}
	if strings.Contains(rec.Body.String(), "数据库连接池") {
		t.Fatalf("响应体泄漏了 panic 详情: %q", rec.Body.String())
	}
	var env Envelope
	if err := json.Unmarshal(rec.Body.Bytes(), &env); err != nil {
		t.Fatalf("panic 后的响应不是 JSON: %v", err)
	}
	if env.Code != 50001 {
		t.Fatalf("code = %d, want 50001", env.Code)
	}
}

func TestTraceIDPropagation(t *testing.T) {
	h := newTestAPI(t, "")

	// 用包络接口（404）来断言 body 里的 trace_id：/healthz 是给探针用的
	// 极简 JSON，故意不套 Envelope。
	const envelopePath = "/api/v1/no-such-route"

	t.Run("上游给了就复用", func(t *testing.T) {
		const upstream = "abc123def456"
		rec, env := doRequest(t, h, http.MethodGet, envelopePath, map[string]string{"X-Request-ID": upstream})

		if got := rec.Header().Get("X-Request-ID"); got != upstream {
			t.Fatalf("响应头 X-Request-ID = %q, want %q", got, upstream)
		}
		if env.TraceID != upstream {
			t.Fatalf("body trace_id = %q, want %q", env.TraceID, upstream)
		}
	})

	t.Run("非法值被丢弃并重新生成", func(t *testing.T) {
		rec, env := doRequest(t, h, http.MethodGet, envelopePath, map[string]string{"X-Request-ID": "bad id with spaces"})

		if got := rec.Header().Get("X-Request-ID"); got == "bad id with spaces" {
			t.Fatal("非法 X-Request-ID 被原样回显，存在日志注入风险")
		}
		if len(env.TraceID) != 24 {
			t.Fatalf("重新生成的 trace_id 长度 = %d, want 24", len(env.TraceID))
		}
	})
}
