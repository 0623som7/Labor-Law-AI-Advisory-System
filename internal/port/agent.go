package port

import (
	"context"
	"encoding/json"
)

// AgentEventType 是 SSE 事件类型的闭集，只有六种。
//
// 定成六种而不是"随便加"，是因为前端 SSE 解析器由 AI 生成，多一种事件它一定走到
// default 分支静默丢弃，而丢掉 done 会让界面永远转圈。
type AgentEventType string

// 事件类型枚举，与 contract/openapi.yaml 的 x-sse-events 段逐字对齐。
const (
	// AgentEventMeta 是首帧，客户端据此建占位气泡。
	AgentEventMeta AgentEventType = "meta"
	// AgentEventDelta 是增量文本，客户端 append，绝不能丢。
	AgentEventDelta AgentEventType = "delta"
	// AgentEventCitation 是法条/判例引用卡片，可与 delta 交错推送。
	AgentEventCitation AgentEventType = "citation"
	// AgentEventToolCall 是工具调用状态，背压时允许丢弃。
	AgentEventToolCall AgentEventType = "tool_call"
	// AgentEventDone 是终止帧，必达，客户端据此结束 loading。
	AgentEventDone AgentEventType = "done"
	// AgentEventError 是异常终止帧，之后不再有事件。
	AgentEventError AgentEventType = "error"
)

// Runner 是 B 提供、A 调用的唯一入口。
//
// 提供方：B。消费方：A。
// 并发安全：实现必须允许不同请求并发调用 Run；同一个 SessionID 的并发 Run 由 B 自行串行化。
// 错误：CodeInvalidArgument（入参缺失）、CodeUpstream（模型失败，可重试）、
// CodeRuleEngine（单条规则异常，甲门降级）、CodeInternal（B 的缺陷）。
// 变更：签名极难变；Options 的语义扩展由 B 在变更单里说明。
//
// A 对 B 的承诺：emit 的 Event 会被原样序列化成 SSE 帧，seq / 心跳 / 背压 / 断线续传都在 A 层做，
// B 不需要知道 HTTP 存在。
type Runner interface {
	// Run 执行一次编排，通过 emit 推事件、通过返回值给终态。
	//
	// emit 的约束：单次调用阻塞不超过 100ms；背压时 A 可能丢弃 tool_call，
	// 但绝不丢 delta 与 done。emit 返回 error 时必须立即停止生成并把该 error 返回。
	// done 事件由 A 在 Run 返回后补发，B 不必自己发 done——除非 B 想指定 finish_reason。
	Run(ctx context.Context, req Request, emit func(Event) error) (Result, error)
}

// Request 是一次编排的入参。
type Request struct {
	// Scene 是编排场景："consult" / "qa" / "review"。
	Scene string
	// SessionID 用于取历史上下文，空串表示不挂历史（如单次抽取）。
	SessionID string
	// Input 是用户原话，原样传入，A 不做清洗。
	Input string
	// FileID 是 review 场景的合同文件 ID。
	FileID string
	// Parsed 是 A 解析好的文本块，B 直接消费，不需要也不允许再碰 PDF。
	// review 场景必非 nil；其余场景为 nil。
	Parsed *ParsedDoc
	// Options 是透传的业务选项（county、lang、need_suggestion 等），A 不解释其含义。
	Options map[string]any
}

// Event 是一条流式事件。字段与 SSE 帧的 data 载荷一一对应。
type Event struct {
	// Type 是六种事件之一。
	Type AgentEventType
	// Seq 由 A 统一分配并覆写：B 填的值会被忽略，保证单调递增且断线续传可比对。
	Seq int
	// Text 是 delta 事件的增量文本；meta 事件为空。
	Text string
	// Citation 只在 citation 事件里有值。
	Citation *Citation
	// Tool 只在 tool_call 事件里有值。
	Tool *ToolStatus
	// Err 只在 error 事件里有值，必须是 AppError，不许是裸 error。
	Err *AppError
}

// Citation 是一条可溯源的引用。前端拿它渲染引用卡片，点击跳法条原文。
type Citation struct {
	// StatuteID 是法条唯一 ID，如 "LCL-38"。
	StatuteID string
	// Law 是法律名称，如 "劳动合同法"。
	Law string
	// Article 是条号，如 "第三十八条"。
	Article string
	// Text 是条文原文。必须来自库，不许 B 现场生成——这是"AI 不许直写法条原文"铁律的技术落点。
	Text string
	// URL 是原文链接，空串表示没有可点的官方来源。
	URL string
}

// ToolRunStatus 是工具调用的对外状态。
type ToolRunStatus string

// 工具状态枚举。前端据此显示"正在检索法条…"与结果条数。
const (
	// ToolRunning 表示工具正在执行。
	ToolRunning ToolRunStatus = "running"
	// ToolOK 表示工具执行成功。
	ToolOK ToolRunStatus = "ok"
	// ToolFailed 表示工具执行失败，前端应淡化展示而不是报错。
	ToolFailed ToolRunStatus = "failed"
)

// ToolStatus 是工具调用过程的可视化数据。
//
// 背压时这类事件优先丢弃，所以前端不能把它当状态机用——真正的结果在 delta 与 citation 里。
type ToolStatus struct {
	// Tool 是工具名，对应 ToolSpec.Name。
	Tool string
	// Args 是实参摘要，用于展示，可能被截断。禁止包含用户原话中的敏感信息。
	Args json.RawMessage
	// Status 是当前状态。
	Status ToolRunStatus
	// Detail 是一句人话说明，如 "命中 5 条法条"。
	Detail string
}

// Result 是 Run 的返回值，用于组装 done 帧与落库。
//
// TODO(确认): 方案 §3.2 只给了 Runner 签名，没给 Result 字段。以下是最小集合，
// 待 B 签字后可加字段（加字段不算改签名）。MessageID 由 A 生成回填，B 不必填。
type Result struct {
	// Content 是完整回答正文，用于落库与断线后重放。
	Content string
	// MessageID 由 A 生成并回填，B 填了会被覆盖。
	MessageID string
	// Usage 是本轮累计 token 计量。
	Usage Usage
	// FinishReason 是终止原因："stop" / "length" / "cancelled" / "error"。
	FinishReason string
}
