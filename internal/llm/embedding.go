package llm

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"time"

	"laborlaw/internal/config"
	"laborlaw/internal/port"
)

// maxEmbedBatch 是单次请求的文本条数上限。
//
// DashScope 的 text-embedding-v3 单请求最多 10 条。写死在客户端里而不是暴露成
// 参数：调用方各切各的批，切错就是静默丢一批向量，而向量库里少几条只有等
// 检索召回不上来才会发现。
const maxEmbedBatch = 10

// EmbeddingClient 调用 OpenAI 兼容的 /embeddings 端点。
//
// 不复用 Client：Client 带令牌桶、日预算、重试与流式，这些对"把 205 条法条嵌
// 一遍"这种离线批处理没有意义，反而会让预算闸把离线任务当成用户流量扣。
type EmbeddingClient struct {
	cfg     config.LLMConfig
	http    *http.Client
	log     *slog.Logger
	baseURL *url.URL
}

// EmbeddingOption 是 EmbeddingClient 的可选依赖注入点。
//
// 不复用 Option：那是 func(*Client)，套不到这个类型上。测试只需要换传输层与
// 日志，两个选项足够。
type EmbeddingOption func(*EmbeddingClient)

// WithEmbeddingLogger 注入结构化日志。
func WithEmbeddingLogger(l *slog.Logger) EmbeddingOption {
	return func(e *EmbeddingClient) {
		if l != nil {
			e.log = l
		}
	}
}

// WithEmbeddingHTTPClient 注入 HTTP 客户端，测试用它指向 httptest 服务器。
func WithEmbeddingHTTPClient(h *http.Client) EmbeddingOption {
	return func(e *EmbeddingClient) {
		if h != nil {
			e.http = h
		}
	}
}

// NewEmbeddingClient 构造向量化客户端。
func NewEmbeddingClient(cfg config.LLMConfig, opts ...EmbeddingOption) *EmbeddingClient {
	// 零值 Timeout 会被 context.WithTimeout 当成"立刻到期"，请求必然失败，
	// 而失败又会被检索层吃成关键词降级——只在日志里留一条 Warn，排查成本极高。
	// 构造函数兜底，别让每个调用方各自记得填。
	if cfg.Timeout <= 0 {
		cfg.Timeout = 60 * time.Second
	}
	e := &EmbeddingClient{
		cfg:  cfg,
		http: &http.Client{Transport: newTransport(cfg)},
		log:  slog.Default(),
	}
	for _, opt := range opts {
		opt(e)
	}
	// 空串必须单独拦：url.Parse("") 会"成功"返回一个空 URL，endpoint() 于是拼出
	// "/embeddings"，请求在传输层才炸成 50201——错在配置，报错却指向网络。
	// 这里不填 baseURL，留给 endpoint() 报 40001。
	if strings.TrimSpace(cfg.BaseURL) != "" {
		if u, err := url.Parse(cfg.BaseURL); err == nil {
			e.baseURL = u
		}
	}
	return e
}

// embeddingResponse 是 /embeddings 的响应体。
//
// Data 里的 Index 必须用上：DashScope 与部分自建网关在批量请求里会乱序返回，
// 直接按数组下标对齐会把 A 的向量存成 B 的。
type embeddingItem struct {
	Index     int       `json:"index"`
	Embedding []float32 `json:"embedding"`
}

type embeddingResponse struct {
	Data  []embeddingItem `json:"data"`
	Usage struct {
		PromptTokens int `json:"prompt_tokens"`
		TotalTokens  int `json:"total_tokens"`
	} `json:"usage"`
}

// embedRequest 是 /embeddings 的请求体。
type embedRequest struct {
	Model          string   `json:"model"`
	Input          []string `json:"input"`
	EncodingFormat string   `json:"encoding_format"`
}

// Embed 把一批文本嵌成向量，返回顺序与入参严格一一对应。
//
// 不重试：离线建库失败就重跑，比在客户端内部退避更好排查，也不会让一次卡住的
// 批量任务把进程挂在那里。不记 usage_records：离线向量化不该计入用户日预算。
func (e *EmbeddingClient) Embed(ctx context.Context, model string, texts []string) ([][]float32, error) {
	if strings.TrimSpace(model) == "" {
		return nil, port.New(port.CodeInvalidArgument, "向量化模型名为空")
	}
	if len(texts) == 0 {
		return [][]float32{}, nil
	}
	for i, t := range texts {
		if t == "" {
			// 空串嵌出来是垃圾向量（模型返回全零或与句首无关的方向），
			// 混进索引后它在余弦里恒为 0，永远排不上来，却占着 topK 的名额。
			return nil, port.New(port.CodeInvalidArgument, "第 %d 条文本为空，不能向量化", i+1)
		}
	}

	out := make([][]float32, len(texts))
	for start := 0; start < len(texts); start += maxEmbedBatch {
		end := start + maxEmbedBatch
		if end > len(texts) {
			end = len(texts)
		}
		vecs, err := e.embedBatch(ctx, model, texts[start:end])
		if err != nil {
			return nil, err
		}
		copy(out[start:end], vecs)
	}
	return out, nil
}

// embedBatch 发一批（≤ maxEmbedBatch 条）并按下标还原顺序。
func (e *EmbeddingClient) embedBatch(ctx context.Context, model string, texts []string) ([][]float32, error) {
	endpoint, err := e.endpoint()
	if err != nil {
		return nil, err
	}

	body, err := json.Marshal(embedRequest{Model: model, Input: texts, EncodingFormat: "float"})
	if err != nil {
		return nil, port.Wrap(port.CodeInvalidArgument, err, "向量化请求体无法序列化")
	}

	callCtx, cancel := context.WithTimeout(ctx, e.cfg.Timeout)
	defer cancel()

	req, err := http.NewRequestWithContext(callCtx, http.MethodPost, endpoint, bytes.NewReader(body))
	if err != nil {
		return nil, port.Wrap(port.CodeInternal, err, "构造向量化请求失败")
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+e.cfg.APIKey)

	resp, err := e.http.Do(req)
	if err != nil {
		return nil, mapTransportError(e.cfg, err)
	}
	defer func() { _ = resp.Body.Close() }()

	raw, readErr := io.ReadAll(io.LimitReader(resp.Body, maxResponseBytes))
	if resp.StatusCode != http.StatusOK {
		return nil, mapStatusError(e.cfg, resp.StatusCode, raw)
	}
	if readErr != nil {
		return nil, port.Wrap(port.CodeUpstream, mapReadErr(e.cfg, readErr), "读取向量化响应失败")
	}

	var parsed embeddingResponse
	if err := json.Unmarshal(raw, &parsed); err != nil {
		return nil, port.Wrap(port.CodeUpstream, mapReadErr(e.cfg, err), "向量化响应不是合法 JSON")
	}
	return reorderEmbeddings(parsed.Data, len(texts))
}

// reorderEmbeddings 校验条数与下标，并按 Index 还原成入参顺序。
func reorderEmbeddings(data []embeddingItem, want int) ([][]float32, error) {
	if len(data) != want {
		return nil, port.New(port.CodeUpstream, "向量条数与请求不一致：请求 %d 条，返回 %d 条", want, len(data))
	}
	out := make([][]float32, want)
	seen := make([]bool, want)
	for _, item := range data {
		if item.Index < 0 || item.Index >= want {
			return nil, port.New(port.CodeUpstream, "向量下标越界：%d 不在 [0,%d)", item.Index, want)
		}
		if seen[item.Index] {
			return nil, port.New(port.CodeUpstream, "向量下标重复：%d", item.Index)
		}
		if len(item.Embedding) == 0 {
			return nil, port.New(port.CodeUpstream, "第 %d 条向量为空", item.Index)
		}
		seen[item.Index] = true
		out[item.Index] = item.Embedding
	}
	return out, nil
}

// endpoint 拼出 /embeddings 的完整地址。
func (e *EmbeddingClient) endpoint() (string, error) {
	if e.baseURL == nil {
		return "", port.New(port.CodeInvalidArgument, "LLM_BASE_URL 不是合法 URL")
	}
	// 用户常把 LLM_BASE_URL 写成带尾斜杠的形式，TrimRight 挡住 "//embeddings"。
	return strings.TrimRight(e.baseURL.String(), "/") + "/embeddings", nil
}

// mapReadErr 给读取/解析错误套上脱敏，避免上游把 key 回显在正文里。
func mapReadErr(cfg config.LLMConfig, err error) error {
	return &redactedError{text: redactKey(cfg, err.Error())}
}

// redactedError 是一个只保留脱敏后文本的 error，用来把上游原文塞进 cause。
type redactedError struct{ text string }

func (r *redactedError) Error() string { return r.text }
