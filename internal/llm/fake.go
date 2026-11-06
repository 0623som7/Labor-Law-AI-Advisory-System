package llm

import (
	"context"
	"strings"
	"time"

	"laborlaw/internal/port"
)

// 编译期断言：两种实现都必须满足 B 依赖的接口。
var (
	_ port.Client = (*Client)(nil)
	_ port.Client = (*FakeClient)(nil)
)

// FakeClient 是 port.Client 的确定性实现。
//
// 为什么必须有它：B 在没有 API key、没有网络、预算为零的情况下要能跑通整个 graph。
// 同时它也是 A 的测试替身——SSE 分帧、断线续传、内容拼接都可以用它验证，不必连真模型。
//
// 零值可用：Reply 为空时返回空串，Chunks 为空时按 Reply 切分。
type FakeClient struct {
	// Reply 是非流式返回的正文，也作为流式切分的素材。
	Reply string
	// Chunks 是流式依次吐出的片段；为空时自动按 Reply 切分。
	Chunks []string
	// Err 非 nil 时所有调用直接返回它，用于测试错误分支。
	Err error
	// Delay 是每个片段之间的等待，用于观察前端是否真的逐字渲染。
	Delay time.Duration
	// Usage 是伪造的计量，让计量与成本闸在本地也能被验证。
	Usage port.Usage
}

// Chat 返回预设正文。
func (f *FakeClient) Chat(_ context.Context, _ port.ChatRequest) (port.ChatResponse, error) {
	if f.Err != nil {
		return port.ChatResponse{}, f.Err
	}
	return port.ChatResponse{
		Content:   f.Reply,
		Usage:     f.Usage,
		LatencyMS: 1,
	}, nil
}

// ChatStream 逐片回调，并在 ctx 取消时立刻停止。
//
// 取消要真的停：如果这里直接 time.Sleep，用户关掉页面后 goroutine 还会把整段
// 内容吐完，压测时表现为"断开的请求仍在跑"。
func (f *FakeClient) ChatStream(ctx context.Context, _ port.ChatRequest, onDelta func(string) error) (port.ChatResponse, error) {
	if f.Err != nil {
		return port.ChatResponse{}, f.Err
	}

	chunks := f.Chunks
	if len(chunks) == 0 {
		chunks = splitForStream(f.Reply)
	}

	var built strings.Builder
	for _, chunk := range chunks {
		if f.Delay > 0 {
			timer := time.NewTimer(f.Delay)
			select {
			case <-ctx.Done():
				timer.Stop()
				return port.ChatResponse{}, port.Wrap(port.CodeUpstreamTimeout, ctx.Err(), "假客户端被取消")
			case <-timer.C:
			}
		}
		if err := onDelta(chunk); err != nil {
			return port.ChatResponse{}, err
		}
		built.WriteString(chunk)
	}

	return port.ChatResponse{
		Content:   built.String(),
		Usage:     f.Usage,
		LatencyMS: 1,
	}, nil
}

// splitForStream 把正文切成小片。
//
// 按 rune 切而不是按 byte：中文一个字三个字节，按字节切会把一个字劈成两半，
// 前端拼出来就是乱码。
func splitForStream(s string) []string {
	const chunkRunes = 8
	runes := []rune(s)
	if len(runes) == 0 {
		return nil
	}
	out := make([]string, 0, (len(runes)+chunkRunes-1)/chunkRunes)
	for start := 0; start < len(runes); start += chunkRunes {
		end := start + chunkRunes
		if end > len(runes) {
			end = len(runes)
		}
		out = append(out, string(runes[start:end]))
	}
	return out
}
