// Package stream 负责把 Agent 吐出的事件变成浏览器能消费的 SSE 帧。
//
// 它解决四件事，每一件都对应一个真实的线上故障：
//
//  1. 分帧：`id` / `event` / `data` 三行加空行。格式错一个字节，浏览器就永远
//     停在 loading——而服务端日志里一切正常。
//  2. 心跳：15s 没有事件就发一行 `: ping` 注释帧。中间任何一层代理（Nginx、
//     公司网关、手机热点）都会掐掉长时间静默的连接，注释帧不触发任何前端逻辑。
//  3. 背压：慢客户端不许拖住整个进程。队列满时丢弃 tool_call，但 delta 与 done
//     一个都不能丢——丢 done 会让界面永远转圈。
//  4. 续传：按 message_id 保留最近若干帧，断线重连带 Last-Event-ID 从下一帧接着推。
//
// 本包不 import gin：只认 http.ResponseWriter + http.Flusher，
// 这样 handler 用什么都行，测试也能直接用 httptest.ResponseRecorder。
package stream

import (
	"encoding/json"
	"fmt"

	"laborlaw/internal/port"
)

// frame 是一条已编码的 SSE 帧。
type frame struct {
	seq  int
	typ  port.AgentEventType
	data []byte
}

// wireEvent 是 SSE `data:` 里的 JSON 形状，与 contract/openapi.yaml 的
// x-sse-events 段逐字对齐。
//
// 为什么不直接序列化 port.Event：它没有 json tag，直接 Marshal 会输出
// {"Type":"delta","Seq":2,"Text":"..."} —— 大写字段名，前端按契约写的是
// `event.seq` / `event.text`，一个都取不到。port 是两人共用的契约，不能为了
// 序列化方便去改它的 tag，所以映射放在本层：HTTP 的形状由 HTTP 层负责。
type wireEvent struct {
	Seq      int           `json:"seq"`
	Text     string        `json:"text,omitempty"`
	Citation *wireCitation `json:"citation,omitempty"`
	Tool     *wireTool     `json:"tool,omitempty"`
	Code     int           `json:"code,omitempty"`
	Message  string        `json:"message,omitempty"`
}

// wireCitation 是引用卡片载荷。
type wireCitation struct {
	StatuteID string `json:"statute_id"`
	Law       string `json:"law"`
	Article   string `json:"article"`
	Text      string `json:"text"`
	URL       string `json:"url,omitempty"`
}

// wireTool 是工具调用状态载荷。
type wireTool struct {
	Tool   string `json:"tool"`
	Status string `json:"status"`
	Detail string `json:"detail,omitempty"`
}

// toWire 把内部事件映射成线上形状。
func toWire(ev port.Event) wireEvent {
	out := wireEvent{Seq: ev.Seq, Text: ev.Text}

	if ev.Citation != nil {
		out.Citation = &wireCitation{
			StatuteID: ev.Citation.StatuteID,
			Law:       ev.Citation.Law,
			Article:   ev.Citation.Article,
			Text:      ev.Citation.Text,
			URL:       ev.Citation.URL,
		}
	}
	if ev.Tool != nil {
		out.Tool = &wireTool{
			Tool:   ev.Tool.Tool,
			Status: string(ev.Tool.Status),
			Detail: ev.Tool.Detail,
		}
	}
	if ev.Type == port.AgentEventError {
		if ev.Err != nil {
			out.Code = int(ev.Err.Code)
			out.Message = ev.Err.Message
		} else {
			out.Code = int(port.CodeInternal)
			out.Message = "服务内部错误"
		}
	}
	return out
}

// encodeFrame 把事件编码成 SSE 帧。
//
// JSON 由 encoding/json 生成，保证是单行（没有换行），所以 data 只需要一个前缀，
// 不需要按行拆分——多行 data 的拼接规则是 SSE 实现里第二常见的错。
func encodeFrame(ev port.Event) (frame, error) {
	payload, err := json.Marshal(toWire(ev))
	if err != nil {
		return frame{}, fmt.Errorf("编码事件 %s: %w", ev.Type, err)
	}

	var buf []byte
	buf = append(buf, "id: "...)
	buf = append(buf, fmt.Sprintf("%d", ev.Seq)...)
	buf = append(buf, "\nevent: "...)
	buf = append(buf, string(ev.Type)...)
	buf = append(buf, "\ndata: "...)
	buf = append(buf, payload...)
	buf = append(buf, "\n\n"...)

	return frame{seq: ev.Seq, typ: ev.Type, data: buf}, nil
}

// droppable 判断队列满时该事件是否可以丢弃。
//
// 只有 tool_call 可以丢：它纯粹是"正在检索…"这类过程状态，丢了顶多少一个提示。
// meta / delta / citation / done / error 都不能丢——丢 meta 前端建不出气泡，
// 丢 done 界面永远转圈，丢 error 用户看到的是"无响应"。
func droppable(t port.AgentEventType) bool {
	return t == port.AgentEventToolCall
}

// frameBuffer 是按字节数限制的帧环形缓冲，用于断线续传。
//
// 按字节而不是按条数限制：一条 citation 帧可能只有 200 字节，一条 delta 帧可能
// 几 KB，按条数限制会让内存占用完全不可预测。
type frameBuffer struct {
	limit  int
	size   int
	frames []frame
}

func newFrameBuffer(limit int) *frameBuffer {
	if limit <= 0 {
		limit = 256 << 10
	}
	return &frameBuffer{limit: limit}
}

// add 追加一帧，超出上限时从最旧的开始丢。
func (b *frameBuffer) add(f frame) {
	b.frames = append(b.frames, f)
	b.size += len(f.data)
	for b.size > b.limit && len(b.frames) > 1 {
		b.size -= len(b.frames[0].data)
		b.frames = b.frames[1:]
	}
}

// from 返回 seq 大于 after 的全部帧。
//
// 复制一份返回：调用方（写协程）会在锁外发送，直接暴露内部切片会被并发追加改坏。
func (b *frameBuffer) from(after int) []frame {
	out := make([]frame, 0, len(b.frames))
	for _, f := range b.frames {
		if f.seq > after {
			out = append(out, f)
		}
	}
	return out
}

// lastSeq 返回缓冲里最大的 seq；空缓冲返回 0。
func (b *frameBuffer) lastSeq() int {
	if len(b.frames) == 0 {
		return 0
	}
	return b.frames[len(b.frames)-1].seq
}
