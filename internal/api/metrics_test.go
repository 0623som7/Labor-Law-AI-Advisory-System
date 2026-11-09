package api

import (
	"context"
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

// TestMetricsWithoutHubReportsZeroSubscribers 钉住"没有 Hub 也要有这两行"：
// 直接返回 0 是对的，缺行会让 Grafana 面板报 no data。
func TestMetricsWithoutHubReportsZeroSubscribers(t *testing.T) {
	gin.SetMode(gin.TestMode)
	h := New(testConfig(""), discardLogger()).Handler()

	code, ctype, body := scrape(t, h, nil)
	if code != http.StatusOK {
		t.Fatalf("GET /metrics 应返回 200，实际 %d，body=%q", code, body)
	}
	if !strings.Contains(ctype, "text/plain") {
		t.Fatalf("Content-Type = %q", ctype)
	}
	for _, want := range []string{"laborlaw_sse_subscribers 0", "laborlaw_sse_streams 0"} {
		if !strings.Contains(body, want) {
			t.Fatalf("缺 %q，body=\n%s", want, body)
		}
	}
}

// TestMetricsCountsLiveSubscribers 钉住订阅者计数会随 Attach 上涨，
// 而流数量走的是另一条路径（Hub 跟踪的流），两者不是同一个数。
func TestMetricsCountsLiveSubscribers(t *testing.T) {
	hub := stream.NewHub(stream.Options{HeartbeatInterval: time.Hour}, time.Minute, discardLogger())
	h := metricsAPI(t, WithHub(hub))

	_, _, before := scrape(t, h, nil)
	if !strings.Contains(before, "laborlaw_sse_subscribers 0") {
		t.Fatalf("挂连接前 Subscribers 应为 0，body=\n%s", before)
	}

	w, err := hub.Attach(context.Background(), "m1", 0, httptest.NewRecorder())
	if err != nil {
		t.Fatalf("Attach 返回错误: %v", err)
	}
	defer func() { _ = w.Close() }()

	_, _, during := scrape(t, h, nil)
	if !strings.Contains(during, "laborlaw_sse_subscribers 1") {
		t.Fatalf("挂上一条连接后 Subscribers 应为 1，body=\n%s", during)
	}

	if err := w.Close(); err != nil {
		t.Fatalf("Close 返回错误: %v", err)
	}
	_, _, after := scrape(t, h, nil)
	if !strings.Contains(after, "laborlaw_sse_subscribers 0") {
		t.Fatalf("关闭后 Subscribers 应回到 0，body=\n%s", after)
	}
	// 流还在 TTL 内，Streams 不会跟着掉——这正是两个指标分开的理由。
	if !strings.Contains(after, "laborlaw_sse_streams 1") {
		t.Fatalf("关闭后流应仍在 TTL 内（Streams=1），body=\n%s", after)
	}
}
