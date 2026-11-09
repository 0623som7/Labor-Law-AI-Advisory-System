package api

import (
	"fmt"
	"net/http"
	"runtime"
	"strconv"
	"strings"
	"time"

	"github.com/gin-gonic/gin"

	"laborlaw/internal/stream"
)

// llmStats 是 /metrics 需要的模型计量能力，*llm.Client 满足它。
//
// 声明窄接口而不直接依赖 *llm.Client：api 包一旦 import llm，
// 测试就得为每个 handler 拖一个真实客户端进来。
type llmStats interface {
	// Budget 返回今日已花与今日上限（元）。
	Budget() (spent, limit float64)
}

// handleMetrics 输出 Prometheus 文本格式。
//
// 不用 Envelope：Prometheus 只认 `name value` 的裸文本，包一层 JSON 就没法抓。
// 只暴露"这台机器现在健不健康、花了多少钱"这四类，不暴露任何用户数据——
// 指标一旦带上 user_id 之类的维度，就等于把业务数据复制到一个没有鉴权的地方。
func (a *API) handleMetrics(c *gin.Context) {
	var b strings.Builder

	writeMetric := func(name, help, typ, value string) {
		fmt.Fprintf(&b, "# HELP %s %s\n# TYPE %s %s\n%s %s\n", name, help, name, typ, name, value)
	}

	uptime := time.Since(a.startedAt).Seconds()
	writeMetric("laborlaw_uptime_seconds", "进程启动至今的秒数", "gauge", strconv.FormatFloat(uptime, 'f', 0, 64))

	streams := 0
	if a.hub != nil {
		streams = a.hub.Streams()
	}
	writeMetric("laborlaw_sse_streams", "当前存活的 SSE 流数量", "gauge", strconv.Itoa(streams))
	writeMetric("laborlaw_sse_dropped_total", "因背压丢弃的帧总数", "counter", strconv.FormatInt(stream.DroppedTotal(), 10))
	writeMetric("laborlaw_goroutines", "当前 goroutine 数", "gauge", strconv.Itoa(runtime.NumGoroutine()))

	// 没配模型时不写这两行：报 0 会让人以为"今天一分钱没花"，
	// 而真实情况是"这台机器根本没接模型"。
	if a.llmStats != nil {
		spent, limit := a.llmStats.Budget()
		writeMetric("laborlaw_llm_spent_yuan", "今日已花的模型费用（元）", "gauge", strconv.FormatFloat(spent, 'f', 4, 64))
		writeMetric("laborlaw_llm_budget_yuan", "今日模型费用上限（元）", "gauge", strconv.FormatFloat(limit, 'f', 2, 64))
	}

	c.Data(http.StatusOK, "text/plain; version=0.0.4; charset=utf-8", []byte(b.String()))
}
