package api

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"

	"laborlaw/internal/stream"
)

// fakeLLMStats 固定返回一份花费与上限，用来验证格式而不是计算。
type fakeLLMStats struct {
	spent float64
	limit float64
}

func (f fakeLLMStats) Budget() (float64, float64) { return f.spent, f.limit }

// metricsAPI 建一个挂了 Hub 的路由树：不挂 Hub 时 sse_streams 恒为 0，
// 测出来的"有值"与"没值"分不清。
func metricsAPI(t *testing.T, opts ...Option) http.Handler {
	t.Helper()
	gin.SetMode(gin.TestMode)
	base := []Option{WithHub(stream.NewHub(stream.Options{HeartbeatInterval: time.Hour}, time.Minute, discardLogger()))}
	return New(testConfig(""), discardLogger(), append(base, opts...)...).Handler()
}

// scrape 发一次 GET /metrics，返回状态码、Content-Type 与正文。
func scrape(t *testing.T, h http.Handler, headers map[string]string) (int, string, string) {
	t.Helper()

	req := httptest.NewRequest(http.MethodGet, "/metrics", nil)
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec.Code, rec.Header().Get("Content-Type"), rec.Body.String()
}

func TestMetricsExposesCoreGauges(t *testing.T) {
	code, ctype, body := scrape(t, metricsAPI(t), nil)

	if code != http.StatusOK {
		t.Fatalf("GET /metrics 应返回 200，实际 %d，body=%q", code, body)
	}
	if !strings.Contains(ctype, "text/plain") {
		if !strings.Contains(ctype, "text/plain; version=0.0.4") {
			t.Fatalf("Content-Type 应是 Prometheus 文本格式，实际 %q", ctype)
		}
	}
	for _, want := range []string{
		"laborlaw_uptime_seconds ",
		"laborlaw_sse_streams ",
		"laborlaw_sse_dropped_total ",
		"laborlaw_goroutines ",
	} {
		if !strings.Contains(body, want) {
			t.Fatalf("指标缺 %q，body=\n%s", want, body)
		}
	}
	// 每行指标都要有 HELP 与 TYPE，否则 Prometheus 抓取时会报格式错误。
	if strings.Count(body, "# TYPE ") != strings.Count(body, "# HELP ") {
		t.Fatalf("HELP 与 TYPE 数量不一致，body=\n%s", body)
	}
}

func TestMetricsOmitsLLMWhenAbsent(t *testing.T) {
	_, _, body := scrape(t, metricsAPI(t), nil)

	if strings.Contains(body, "laborlaw_llm_spent_yuan") || strings.Contains(body, "laborlaw_llm_budget_yuan") {
		t.Fatalf("没注入 llmStats 时不应出现费用指标，body=\n%s", body)
	}
}

func TestMetricsIncludesBudgetWhenPresent(t *testing.T) {
	_, _, body := scrape(t, metricsAPI(t, WithLLMStats(fakeLLMStats{spent: 1.5, limit: 20})), nil)

	for _, want := range []string{"laborlaw_llm_spent_yuan 1.5000", "laborlaw_llm_budget_yuan 20.00"} {
		if !strings.Contains(body, want) {
			t.Fatalf("缺 %q，body=\n%s", want, body)
		}
	}
}

func TestMetricsRequiresNoAuth(t *testing.T) {
	// 带一个明显无效的令牌：如果路由错挂到 requireAuth 上，这里会返回 40101。
	code, _, body := scrape(t, metricsAPI(t, WithLLMStats(fakeLLMStats{spent: 0, limit: 20})),
		map[string]string{"Authorization": "Bearer not-a-real-token"})

	if code != http.StatusOK {
		t.Fatalf("/metrics 不应鉴权，实际 %d，body=%q", code, body)
	}
	if strings.Contains(body, "40101") {
		t.Fatalf("/metrics 被鉴权中间件拦了，body=%q", body)
	}
}
