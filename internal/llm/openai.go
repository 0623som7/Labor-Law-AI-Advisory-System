package llm

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"time"

	"laborlaw/internal/config"
	"laborlaw/internal/port"
)

// 本文件只放"OpenAI 兼容协议"这一层的细节：请求体形状、SSE 解析、状态码映射。
// 上层（llm.go）不应该出现任何 json tag 或 HTTP 头——协议改动时只需要动这里。

// maxSSELineBytes 是单行 SSE 的上限。上游偶尔会把整段错误页塞进一行 data，
// bufio.Scanner 默认 64KB 会直接报 ErrTooLong，看起来像"流断了"。
const maxSSELineBytes = 1 << 20

// wireTool 是 OpenAI 的 tools 元素形状。
//
// port.ToolSpec 的 json tag 恰好与 function 对象一致，所以直接内嵌当值用，
// 不需要再抄一份字段名——抄一份就多一个"两边改名漏一边"的地方。
type wireTool struct {
	Type     string        `json:"type"`
	Function port.ToolSpec `json:"function"`
}

// streamOptions 只在流式时传，要求上游在最后一块里带 usage。
// 不带的话，流式调用永远算不出成本，预算闸就是摆设。
type streamOptions struct {
	IncludeUsage bool `json:"include_usage"`
}

// responseFormat 对应 JSONSchema 的结构化输出约束。
type responseFormat struct {
	Type       string          `json:"type"`
	JSONSchema json.RawMessage `json:"json_schema,omitempty"`
}

// chatCompletionRequest 是发往上游的请求体。
type chatCompletionRequest struct {
	Model          string          `json:"model"`
	Messages       []port.Message  `json:"messages"`
	Tools          []wireTool      `json:"tools,omitempty"`
	Temperature    float64         `json:"temperature,omitempty"`
	MaxTokens      int             `json:"max_tokens,omitempty"`
	Stream         bool            `json:"stream"`
	StreamOptions  *streamOptions  `json:"stream_options,omitempty"`
	ResponseFormat *responseFormat `json:"response_format,omitempty"`
}

// usagePayload 是上游返回的 token 计量。
type usagePayload struct {
	PromptTokens     int `json:"prompt_tokens"`
	CompletionTokens int `json:"completion_tokens"`
	TotalTokens      int `json:"total_tokens"`
}

// errorPayload 是上游在 200 里返回错误时的形状（部分网关这么做）。
type errorPayload struct {
	Message string `json:"message"`
	Type    string `json:"type"`
}

// chatCompletionResponse 是非流式响应。
type chatCompletionResponse struct {
	Model   string `json:"model"`
	Choices []struct {
		Message struct {
			Content   string          `json:"content"`
			ToolCalls []port.ToolCall `json:"tool_calls"`
		} `json:"message"`
		FinishReason string `json:"finish_reason"`
	} `json:"choices"`
	Usage usagePayload  `json:"usage"`
	Error *errorPayload `json:"error"`
}

// toolCallDelta 是流式响应里工具调用的增量片段。
//
// 上游按 index 分批下发：先给 id 与 name，再一段段给 arguments。必须按 index 合并，
// 不能看到 name 就新建——一个请求里可能有多个工具调用交替下发。
type toolCallDelta struct {
	Index    int    `json:"index"`
	ID       string `json:"id"`
	Function struct {
		Name      string `json:"name"`
		Arguments string `json:"arguments"`
	} `json:"function"`
}

// chatCompletionChunk 是流式响应的一块。
type chatCompletionChunk struct {
	Choices []struct {
		Delta struct {
			Content   string          `json:"content"`
			ToolCalls []toolCallDelta `json:"tool_calls"`
		} `json:"delta"`
		FinishReason string `json:"finish_reason"`
	} `json:"choices"`
	Usage *usagePayload `json:"usage"`
	Error *errorPayload `json:"error"`
}

// buildRequestBody 序列化一次调用的请求体。
func buildRequestBody(model string, req port.ChatRequest, stream bool) ([]byte, error) {
	body := chatCompletionRequest{
		Model:       model,
		Messages:    req.Messages,
		Temperature: req.Temperature,
		MaxTokens:   req.MaxTokens,
		Stream:      stream,
	}
	if stream {
		body.StreamOptions = &streamOptions{IncludeUsage: true}
	}
	if len(req.Tools) > 0 {
		body.Tools = make([]wireTool, 0, len(req.Tools))
		for _, t := range req.Tools {
			body.Tools = append(body.Tools, wireTool{Type: "function", Function: t})
		}
	}
	if req.JSONSchema != nil {
		schema, err := json.Marshal(struct {
			Name   string          `json:"name"`
			Schema json.RawMessage `json:"schema"`
			Strict bool            `json:"strict,omitempty"`
		}{Name: req.JSONSchema.Name, Schema: req.JSONSchema.Schema, Strict: req.JSONSchema.Strict})
		if err != nil {
			return nil, port.Wrap(port.CodeInvalidArgument, err, "json_schema 无法序列化")
		}
		body.ResponseFormat = &responseFormat{Type: "json_schema", JSONSchema: schema}
	}

	raw, err := json.Marshal(body)
	if err != nil {
		return nil, port.Wrap(port.CodeInvalidArgument, err, "请求体无法序列化")
	}
	return raw, nil
}

// endpoint 拼出 chat/completions 的完整地址。
func (c *Client) endpoint() (string, error) {
	if c.baseURL == nil {
		return "", port.New(port.CodeInvalidArgument, "LLM_BASE_URL 不是合法 URL")
	}
	return strings.TrimRight(c.baseURL.String(), "/") + "/chat/completions", nil
}

// newTransport 构造带超时的传输层。
//
// 每个超时都对应一类真实故障：Dial 5s 挡住"DNS 挂了/路由不通"，TLS 5s 挡住握手卡住，
// ResponseHeaderTimeout 挡住"上游接了连接但不回包"，而 ctx 超时兜住整个调用。
func newTransport(cfg config.LLMConfig) *http.Transport {
	return &http.Transport{
		Proxy: http.ProxyFromEnvironment,
		DialContext: (&net.Dialer{
			Timeout:   5 * time.Second,
			KeepAlive: 30 * time.Second,
		}).DialContext,
		ForceAttemptHTTP2:     true,
		MaxIdleConns:          20,
		MaxIdleConnsPerHost:   10,
		IdleConnTimeout:       90 * time.Second,
		TLSHandshakeTimeout:   5 * time.Second,
		ExpectContinueTimeout: time.Second,
		ResponseHeaderTimeout: cfg.Timeout,
	}
}

// streamTimeout 返回流式调用的整体上限。
//
// 单独用 StreamTimeout 而不是 Timeout：流式响应持续吐字，耗时随回答长度增长，
// 拿"单次往返"的 60s 当上限会在长回答上整段掐断（而且已吐过字就不能重试，
// 用户直接丢一次回答）。StreamTimeout 未配置时退回 Timeout，保证手工构造的
// 配置（测试、脚本）行为不变。
func (c *Client) streamTimeout() time.Duration {
	if c.cfg.StreamTimeout > 0 {
		return c.cfg.StreamTimeout
	}
	return c.cfg.Timeout
}

// doChat 执行一次非流式调用。
func (c *Client) doChat(ctx context.Context, body []byte, traceID string) (port.ChatResponse, error) {
	endpoint, err := c.endpoint()
	if err != nil {
		return port.ChatResponse{}, err
	}

	callCtx, cancel := context.WithTimeout(ctx, c.cfg.Timeout)
	defer cancel()

	httpReq, err := http.NewRequestWithContext(callCtx, http.MethodPost, endpoint, bytes.NewReader(body))
	if err != nil {
		return port.ChatResponse{}, port.Wrap(port.CodeInvalidArgument, err, "构造模型请求失败")
	}
	c.applyHeaders(httpReq, traceID)

	start := c.now()
	resp, err := c.http.Do(httpReq)
	if err != nil {
		return port.ChatResponse{}, c.transportError(err)
	}
	defer resp.Body.Close()

	raw, err := io.ReadAll(io.LimitReader(resp.Body, maxResponseBytes))
	if err != nil {
		return port.ChatResponse{}, port.Wrap(port.CodeUpstream, err, "读取模型响应失败")
	}
	if resp.StatusCode != http.StatusOK {
		return port.ChatResponse{}, c.statusError(resp.StatusCode, raw)
	}

	var parsed chatCompletionResponse
	if err := json.Unmarshal(raw, &parsed); err != nil {
		return port.ChatResponse{}, port.Wrap(port.CodeUpstream, err, "解析模型响应失败")
	}
	// 200 但 body 是错误 JSON 的情况真实存在；当成"空回答"会让 B 误判成检索没命中。
	if parsed.Error != nil {
		return port.ChatResponse{}, port.Wrap(port.CodeUpstream,
			errors.New(truncate(c.redactText(parsed.Error.Message), 300)), "模型返回错误")
	}
	if len(parsed.Choices) == 0 {
		return port.ChatResponse{}, port.New(port.CodeUpstream, "模型返回了空 choices")
	}

	choice := parsed.Choices[0]
	return port.ChatResponse{
		Content:   choice.Message.Content,
		ToolCalls: choice.Message.ToolCalls,
		Usage:     toUsage(parsed.Usage),
		LatencyMS: int(c.now().Sub(start).Milliseconds()),
	}, nil
}

// doChatStream 执行一次流式调用。
//
// onDelta 返回的 error 原样向上返回，不包装：调用方要能 errors.Is 到自己那个错误。
func (c *Client) doChatStream(ctx context.Context, body []byte, traceID string, onDelta func(string) error) (port.ChatResponse, error) {
	endpoint, err := c.endpoint()
	if err != nil {
		return port.ChatResponse{}, err
	}

	callCtx, cancel := context.WithTimeout(ctx, c.streamTimeout())
	defer cancel()

	httpReq, err := http.NewRequestWithContext(callCtx, http.MethodPost, endpoint, bytes.NewReader(body))
	if err != nil {
		return port.ChatResponse{}, port.Wrap(port.CodeInvalidArgument, err, "构造流式请求失败")
	}
	c.applyHeaders(httpReq, traceID)
	httpReq.Header.Set("Accept", "text/event-stream")

	start := c.now()
	resp, err := c.http.Do(httpReq)
	if err != nil {
		return port.ChatResponse{}, c.transportError(err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		raw, readErr := io.ReadAll(io.LimitReader(resp.Body, maxResponseBytes))
		if readErr != nil {
			return port.ChatResponse{}, port.Wrap(port.CodeUpstream, readErr, "读取错误响应失败")
		}
		return port.ChatResponse{}, c.statusError(resp.StatusCode, raw)
	}

	var (
		content   strings.Builder
		toolCalls []port.ToolCall
		usage     usagePayload
		sawUsage  bool
	)

	scanner := bufio.NewScanner(resp.Body)
	scanner.Buffer(make([]byte, 0, 64*1024), maxSSELineBytes)

	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" || strings.HasPrefix(line, ":") || strings.HasPrefix(line, "event:") {
			continue
		}
		if !strings.HasPrefix(line, "data:") {
			continue
		}
		payload := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
		if payload == "[DONE]" {
			break
		}

		var chunk chatCompletionChunk
		if err := json.Unmarshal([]byte(payload), &chunk); err != nil {
			return port.ChatResponse{}, port.Wrap(port.CodeUpstream, err, "解析流式片段失败")
		}
		if chunk.Error != nil {
			return port.ChatResponse{}, port.Wrap(port.CodeUpstream,
				errors.New(truncate(c.redactText(chunk.Error.Message), 300)), "模型流式返回错误")
		}
		if chunk.Usage != nil {
			usage = *chunk.Usage
			sawUsage = true
		}
		for _, choice := range chunk.Choices {
			if choice.Delta.Content != "" {
				if err := onDelta(choice.Delta.Content); err != nil {
					return port.ChatResponse{}, err
				}
				content.WriteString(choice.Delta.Content)
			}
			mergeToolCalls(&toolCalls, choice.Delta.ToolCalls)
		}
	}
	if err := scanner.Err(); err != nil {
		return port.ChatResponse{}, c.transportError(err)
	}

	result := port.ChatResponse{
		Content:   content.String(),
		ToolCalls: toolCalls,
		LatencyMS: int(c.now().Sub(start).Milliseconds()),
	}
	if sawUsage {
		result.Usage = toUsage(usage)
	} else if result.Content != "" {
		// 上游没给 usage 时不能记 0：那会让成本闸失真。按字符数粗估并在日志里留痕。
		result.Usage = estimateUsage(body, result.Content)
		c.log.Warn("上游未返回 usage，已按字符数粗估", "scene", "stream")
	}
	return result, nil
}

// applyHeaders 设置鉴权与链路头。
//
// Authorization 只在这里出现一次：任何日志、错误信息、响应体里都不许再出现它，
// 这是"key 不进日志"唯一可靠的落点。
func (c *Client) applyHeaders(req *http.Request, traceID string) {
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+c.cfg.APIKey)
	if traceID != "" {
		req.Header.Set("X-Request-ID", traceID)
	}
}

// transportError 把网络层错误翻译成业务码（映射见 errors_map.go）。
func (c *Client) transportError(err error) error {
	return mapTransportError(c.cfg, err)
}

// isTimeout 识别 transport 层超时（ctx 超时不会走到这里，但两者都判一次更稳）。
func isTimeout(err error) bool {
	var netErr net.Error
	return errors.As(err, &netErr) && netErr.Timeout()
}

// statusError 把非 200 映射成业务码（映射见 errors_map.go）。
func (c *Client) statusError(status int, raw []byte) error {
	return mapStatusError(c.cfg, status, raw)
}

// redact 把错误文本里可能出现的 API key 抹掉。
func (c *Client) redact(err error) error {
	if err == nil || c.cfg.APIKey == "" {
		return err
	}
	return errors.New(c.redactText(err.Error()))
}

// redactText 是 redact 的字符串版本，用于上游返回的错误正文。
//
// 有些网关会把整个 Authorization 头回显在错误里（"Bearer sk-... is invalid"），
// 这类文本一旦进 cause 就会被日志采集系统收走，等于把 key 写进了日志盘。
func (c *Client) redactText(s string) string {
	return redactKey(c.cfg, s)
}

// mergeToolCalls 按 index 合并流式工具调用片段。
func mergeToolCalls(dst *[]port.ToolCall, deltas []toolCallDelta) {
	for _, d := range deltas {
		if d.Index < 0 || d.Index > 64 {
			continue // 防御畸形响应，避免无限扩容
		}
		for len(*dst) <= d.Index {
			*dst = append(*dst, port.ToolCall{})
		}
		call := &(*dst)[d.Index]
		if d.ID != "" {
			call.ID = d.ID
		}
		if d.Function.Name != "" {
			call.Name += d.Function.Name
		}
		if d.Function.Arguments != "" {
			call.Arguments = append(call.Arguments, d.Function.Arguments...)
		}
	}
}

// toUsage 把上游计量转成 port 结构，并补齐 TotalTokens。
func toUsage(u usagePayload) port.Usage {
	total := u.TotalTokens
	if total == 0 {
		total = u.PromptTokens + u.CompletionTokens
	}
	return port.Usage{
		PromptTokens:     u.PromptTokens,
		CompletionTokens: u.CompletionTokens,
		TotalTokens:      total,
	}
}

// estimateUsage 在缺少 usage 时按字符数粗估。
//
// 中文大致 1 字 ≈ 1 token，英文 4 字符 ≈ 1 token；这里按"每 2 字节 1 token"取中，
// 只用于让成本闸不至于完全失真，日志里会明确标注这是估算值。
func estimateUsage(requestBody []byte, content string) port.Usage {
	prompt := len(requestBody) / 2
	completion := len(content) / 2
	return port.Usage{
		PromptTokens:     prompt,
		CompletionTokens: completion,
		TotalTokens:      prompt + completion,
	}
}

// truncate 截断超长文本，避免把整个错误页写进日志或错误链。
func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "..."
}

// String 便于在日志里安全打印请求摘要（不含消息正文与 key）。
func (r chatCompletionRequest) String() string {
	return fmt.Sprintf("model=%s messages=%d stream=%t", r.Model, len(r.Messages), r.Stream)
}
