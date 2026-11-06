package main

import (
	"context"
	"log/slog"
	"strings"
	"time"

	"laborlaw/internal/port"
)

// stubRunner 是 B 的 agent.Runner 交付前的临时实现。
//
// 它存在的意义只有一个：把 A 侧的链路（SSE 分帧、心跳、背压、断线、前端拼接）
// 先跑通，让 B 的 graph 可以独立开发。B 一交付就删掉本文件，装配处换成真实现。
//
// 行为必须和真 Runner 一样受 ctx 控制：如果它用 time.Sleep，用户关掉页面后
// 它还会把整段话吐完，压测时表现为"断开的请求仍在跑"。
type stubRunner struct {
	log *slog.Logger
	// delay 是每段之间的等待，用来肉眼确认"逐字出现"而不是一次性刷出。
	delay time.Duration
	// text 为空时使用内置文案。
	text string
}

// stubDefaultText 是一段结构接近真实回答的固定文案：先给结论、再给法条、最后给行动建议。
const stubDefaultText = "根据《劳动合同法》第三十八条，用人单位未及时足额支付劳动报酬的，劳动者可以解除劳动合同，并要求经济补偿。建议你先保留工资流水与考勤记录，再向当地劳动监察部门投诉或申请劳动仲裁。"

// newStubRunner 构造临时 Runner。40ms/段在本地看起来接近真模型的节奏。
func newStubRunner(log *slog.Logger) *stubRunner {
	return &stubRunner{log: log, delay: 40 * time.Millisecond}
}

// Run 按固定文案分段吐 delta，并在中途插一条 citation 与一条 tool_call，
// 目的是让前端的每种帧都有东西可渲染。
func (s *stubRunner) Run(ctx context.Context, req port.Request, emit func(port.Event) error) (port.Result, error) {
	text := s.text
	if text == "" {
		text = stubDefaultText
	}

	chunks := splitRunes(text, 6)

	var built strings.Builder
	for i, chunk := range chunks {
		if err := s.wait(ctx); err != nil {
			return port.Result{}, err
		}

		if i == 0 {
			// 过程状态：前端显示"正在检索法条…"。这类帧允许被背压丢弃。
			if err := emit(port.Event{
				Type: port.AgentEventToolCall,
				Tool: &port.ToolStatus{Tool: "search_statute", Status: port.ToolRunning, Detail: "检索相关法条"},
			}); err != nil {
				return port.Result{}, err
			}
		}

		if err := emit(port.Event{Type: port.AgentEventDelta, Text: chunk}); err != nil {
			return port.Result{}, err
		}
		built.WriteString(chunk)

		if i == 1 {
			// 引用卡片：正文必须来自库，这里用真实条文做示例。
			if err := emit(port.Event{
				Type: port.AgentEventCitation,
				Citation: &port.Citation{
					StatuteID: "LCL-38",
					Law:       "中华人民共和国劳动合同法",
					Article:   "第三十八条",
					Text:      "用人单位有下列情形之一的，劳动者可以解除劳动合同：（一）未按照劳动合同约定提供劳动保护或者劳动条件的；（二）未及时足额支付劳动报酬的；……",
					URL:       "https://flk.npc.gov.cn/",
				},
			}); err != nil {
				return port.Result{}, err
			}
		}
	}

	s.log.Debug("stub runner finished", "scene", req.Scene, "chunks", len(chunks))

	return port.Result{
		Content:      built.String(),
		FinishReason: "stop",
		Usage: port.Usage{
			PromptTokens:     120,
			CompletionTokens: len([]rune(text)),
			TotalTokens:      120 + len([]rune(text)),
		},
	}, nil
}

// wait 是 ctx 感知的等待。
func (s *stubRunner) wait(ctx context.Context) error {
	if s.delay <= 0 {
		return nil
	}
	timer := time.NewTimer(s.delay)
	defer timer.Stop()

	select {
	case <-ctx.Done():
		return port.Wrap(port.CodeUpstreamTimeout, ctx.Err(), "stub runner 已被取消")
	case <-timer.C:
		return nil
	}
}

// splitRunes 按 rune 切分，避免把一个汉字劈成两半。
func splitRunes(s string, size int) []string {
	if size <= 0 {
		size = 1
	}
	runes := []rune(s)
	if len(runes) == 0 {
		return nil
	}
	out := make([]string, 0, (len(runes)+size-1)/size)
	for start := 0; start < len(runes); start += size {
		end := start + size
		if end > len(runes) {
			end = len(runes)
		}
		out = append(out, string(runes[start:end]))
	}
	return out
}
