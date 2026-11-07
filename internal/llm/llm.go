// Package llm 是 port.Client 的唯一实现：模型路由、限流、重试、计量与成本闸。
//
// 为什么所有模型调用都必须从这里出去：重试、超时、限流、token 计费、按场景路由
// 这五件事只能有一个地方做。散到 Agent 的十几个节点里，账永远对不上，而项目的
// LLM 预算只剩三位数。
//
// 本包只 import port 与 config，不碰 gin / eino / store：B 给自己写测试 graph 时，
// 不需要把 HTTP 层和数据库一起拖进来。
package llm

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"time"

	"laborlaw/internal/config"
	"laborlaw/internal/port"
)

// 场景常量。取值同时是 config 里 scene → model 的键，也是 usage_records.scene 的落库值；
// 写成常量而不是裸字符串，是因为 B 拼错 "review.extract" 时应该编译不过，而不是线上
// 悄悄落到默认模型上多烧一倍钱。
const (
	// SceneConsult 是劳动法咨询，用最强模型。
	SceneConsult = "consult"
	// SceneQA 是简单问答，与 consult 共用模型但可单独限流。
	SceneQA = "qa"
	// SceneReviewExtract 是合同要素抽取，用便宜模型。
	SceneReviewExtract = "review.extract"
	// SceneReviewAdvise 是条款建议生成，用强模型。
	SceneReviewAdvise = "review.advise"
)

// maxBackoff 是重试等待的上限。
//
// 写死在代码里而不是配置：它是防呆而不是业务参数——真让 LLM_RETRY_BACKOFF 配成
// 30s，三次重试就是 90s，接口会先被上游超时打断，日志里还看不出是谁慢。
const maxBackoff = 8 * time.Second

// maxResponseBytes 是单次响应体的硬上限，防上游返回一个巨大的错误页把内存打爆。
const maxResponseBytes = 8 << 20

// Client 实现 port.Client。
//
// 字段全不导出：限流桶与预算必须只有一个实例，否则并发下各自记账，限额形同虚设。
// 需要换 HTTP 客户端或注入 recorder 时走 Option。
type Client struct {
	cfg     config.LLMConfig
	http    *http.Client
	log     *slog.Logger
	rec     UsageRecorder
	limit   *tokenBucket
	budget  *budget
	baseURL *url.URL
	now     func() time.Time
}

// Option 是 Client 的可选依赖注入点。生产代码只用默认值，测试用它换掉时钟与传输层。
type Option func(*Client)

// WithLogger 注入结构化日志。为 nil 时保留默认 logger。
func WithLogger(l *slog.Logger) Option {
	return func(c *Client) {
		if l != nil {
			c.log = l
		}
	}
}

// WithUsageRecorder 注入计量落库实现。
//
// 不注入时只累加进程内预算，不落库：这是给单元测试与本地 FakeClient 用的，
// 生产必须注入 store 实现，否则成本账随重启清零。
func WithUsageRecorder(r UsageRecorder) Option {
	return func(c *Client) { c.rec = r }
}

// WithHTTPClient 注入 HTTP 客户端，测试用它指向 httptest 服务器。
func WithHTTPClient(h *http.Client) Option {
	return func(c *Client) {
		if h != nil {
			c.http = h
		}
	}
}

// New 构造客户端。
//
// 不返回 error：配置的合法性在 config.Validate 里已经查过，这里再返回一次 error
// 只会让每个调用点写一遍 if err != nil。URL 解析失败会推迟到首次调用时以
// CodeInvalidArgument 暴露，日志里有明确原因。
func New(cfg config.LLMConfig, opts ...Option) *Client {
	c := &Client{
		cfg:    cfg,
		http:   &http.Client{Transport: newTransport(cfg)},
		log:    slog.Default(),
		limit:  newTokenBucket(cfg.QPS),
		budget: newBudget(cfg.DailyBudgetYuan),
		now:    time.Now,
	}
	for _, opt := range opts {
		opt(c)
	}
	if u, err := url.Parse(cfg.BaseURL); err == nil {
		c.baseURL = u
	}
	return c
}

// Chat 发起一次非流式调用。
//
// 重试只发生在"重试可能得到不同结果"的错误上（限流、5xx、超时），判定统一走
// port.ErrorCode.Retryable；参数错与鉴权错重试一万次也是同样结果，直接把 token 烧掉。
func (c *Client) Chat(ctx context.Context, req port.ChatRequest) (port.ChatResponse, error) {
	if err := validateRequest(req); err != nil {
		return port.ChatResponse{}, err
	}
	if err := c.limit.Wait(ctx); err != nil {
		return port.ChatResponse{}, err
	}

	model := c.route(req.Scene, req.Model)
	body, err := buildRequestBody(model, req, false)
	if err != nil {
		return port.ChatResponse{}, err
	}

	for attempt := 0; ; attempt++ {
		resp, callErr := c.doChat(ctx, body, req.TraceID)
		if callErr == nil {
			c.afterSuccess(ctx, req, model, resp)
			return resp, nil
		}
		if !c.canRetry(callErr, attempt) {
			return port.ChatResponse{}, callErr
		}
		c.log.Warn("llm retry", "scene", req.Scene, "attempt", attempt+1, "code", port.CodeOf(callErr).String())
		if waitErr := sleepCtx(ctx, c.backoff(attempt)); waitErr != nil {
			return port.ChatResponse{}, waitErr
		}
	}
}

// ChatStream 发起一次流式调用，每段增量交给 onDelta。
//
// 两条硬规则：
//  1. onDelta 返回的 error 必须原样返回（不包装、不换码）——B 用它实现
//     "发现引用无源立即掐断生成"，换码会让调用方认不出来。
//  2. 一旦已经吐出过任何一个字符，就不再重试。重试会把前半段内容推两遍，
//     用户看到的是重复句子，而日志里一切正常。
func (c *Client) ChatStream(ctx context.Context, req port.ChatRequest, onDelta func(delta string) error) (port.ChatResponse, error) {
	if err := validateRequest(req); err != nil {
		return port.ChatResponse{}, err
	}
	if onDelta == nil {
		return port.ChatResponse{}, port.New(port.CodeInvalidArgument, "onDelta 不能为 nil")
	}
	if err := c.limit.Wait(ctx); err != nil {
		return port.ChatResponse{}, err
	}

	model := c.route(req.Scene, req.Model)
	body, err := buildRequestBody(model, req, true)
	if err != nil {
		return port.ChatResponse{}, err
	}

	emitted := false
	emit := func(delta string) error {
		emitted = true
		return onDelta(delta)
	}

	for attempt := 0; ; attempt++ {
		resp, callErr := c.doChatStream(ctx, body, req.TraceID, emit)
		if callErr == nil {
			c.afterSuccess(ctx, req, model, resp)
			return resp, nil
		}
		if !c.canRetry(callErr, attempt) || emitted {
			return port.ChatResponse{}, callErr
		}
		c.log.Warn("llm stream retry", "scene", req.Scene, "attempt", attempt+1, "code", port.CodeOf(callErr).String())
		if waitErr := sleepCtx(ctx, c.backoff(attempt)); waitErr != nil {
			return port.ChatResponse{}, waitErr
		}
	}
}

// route 决定本次调用打到哪个模型。
//
// 显式指定 Model 时按指定的来（只给压测与调试用）。日预算超限后，除抽取外的场景
// 一律降级到便宜档：保住"能答"比"答得最好"重要，超预算直接 500 才是真正的失败。
func (c *Client) route(scene, explicit string) string {
	if explicit != "" {
		return explicit
	}
	downgrade := c.budget.exceeded()
	switch scene {
	case SceneConsult, SceneQA:
		if downgrade {
			return c.cfg.ModelExtract
		}
		return c.cfg.ModelConsult
	case SceneReviewExtract:
		return c.cfg.ModelExtract
	case SceneReviewAdvise:
		if downgrade {
			return c.cfg.ModelExtract
		}
		return c.cfg.ModelAdvise
	default:
		return c.cfg.ModelConsult
	}
}

// Cost 估算一次用量的金额（元）。导出是为了让测试与报表用同一套算法，
// 而不是各自乘一遍单价。
func (c *Client) Cost(u port.Usage) float64 {
	return float64(u.PromptTokens)/1000*c.cfg.PriceInputPer1K +
		float64(u.CompletionTokens)/1000*c.cfg.PriceOutputPer1K
}

// Budget 返回今日已花与上限，供 /metrics 与日报使用。
func (c *Client) Budget() (spent, limit float64) {
	return c.budget.snapshot()
}

// afterSuccess 记账：预算累加、成本写进 Usage、异步持久化。
//
// 落库失败不返回给用户：模型已经答完了，把"记账失败"变成用户侧 500 是错误的取舍。
// 但必须打日志，否则成本闸会在无人察觉的情况下失效。
func (c *Client) afterSuccess(ctx context.Context, req port.ChatRequest, model string, resp port.ChatResponse) {
	cost := c.Cost(resp.Usage)
	c.budget.add(cost)

	if c.rec == nil {
		return
	}
	rec := port.UsageRecord{
		// trace_id 是 usage_records 与 messages 之间唯一的连接键：
		// 表里不存 user_id，按人归集只能靠它。
		TraceID:          req.TraceID,
		Scene:            req.Scene,
		Model:            model,
		PromptTokens:     resp.Usage.PromptTokens,
		CompletionTokens: resp.Usage.CompletionTokens,
		LatencyMS:        resp.LatencyMS,
		CostEst:          cost,
		CreatedAt:        c.now(),
	}
	if err := c.rec.Record(ctx, rec); err != nil {
		c.log.Error("记录模型用量失败，成本账可能不准", "err", err.Error(), "scene", req.Scene)
	}
}

// canRetry 判定是否还值得再试一次。
func (c *Client) canRetry(err error, attempt int) bool {
	if attempt >= c.cfg.MaxRetries {
		return false
	}
	// 调用方主动取消不是上游故障，重试只会拖住请求。
	if errors.Is(err, context.Canceled) {
		return false
	}
	return port.IsRetryable(err)
}

// backoff 返回第 attempt 次重试前的等待（从 0 计）。
func (c *Client) backoff(attempt int) time.Duration {
	d := c.cfg.RetryBackoff << attempt
	if d <= 0 || d > maxBackoff {
		return minDuration(d, maxBackoff)
	}
	return d
}

func minDuration(d, cap time.Duration) time.Duration {
	if d <= 0 {
		return 0
	}
	if d > cap {
		return cap
	}
	return d
}

// sleepCtx 等待 d，但 ctx 取消时立刻返回。
//
// 裸 time.Sleep 会让"用户已断开"的请求继续占着 goroutine 与预算，
// 压测时表现为取消后仍有大量在途调用。
func sleepCtx(ctx context.Context, d time.Duration) error {
	if d <= 0 {
		return nil
	}
	timer := time.NewTimer(d)
	defer timer.Stop()

	select {
	case <-ctx.Done():
		return port.Wrap(port.CodeUpstreamTimeout, ctx.Err(), "等待重试时请求已取消")
	case <-timer.C:
		return nil
	}
}

// validateRequest 检查调用方必须提供的字段。
func validateRequest(req port.ChatRequest) error {
	if len(req.Messages) == 0 {
		return port.New(port.CodeInvalidArgument, "messages 不能为空")
	}
	if strings.TrimSpace(req.Scene) == "" {
		return port.New(port.CodeInvalidArgument, "scene 不能为空")
	}
	return nil
}
