package port

import (
	"context"
	"encoding/json"
)

// Role 是对话角色。用自定义类型而不是裸 string，是为了让 B 拼错 "assisant" 时编译不过。
type Role string

// 对话角色枚举。
const (
	// RoleSystem 是系统提示词，只能出现在 Messages 首位。
	RoleSystem Role = "system"
	// RoleUser 是用户输入。
	RoleUser Role = "user"
	// RoleAssistant 是模型的上一轮输出，多轮与工具循环里必须回填。
	RoleAssistant Role = "assistant"
	// RoleTool 是工具执行结果，必须带 ToolCallID 与产生它的 ToolCall 配对。
	RoleTool Role = "tool"
)

// Client 是 B 唯一允许使用的模型出口。
//
// 提供方：A。消费方：B。
// 并发安全：方法是并发安全的，B 可以在多个 graph 节点里同时调用。
// 错误：CodeRateLimited（可重试）、CodeUpstream（可重试 ≤2 次）、CodeUpstreamTimeout（可重试）、
// CodeInvalidArgument（请求本身不合法，不可重试，重试只会再花一次 token）。
// 变更：签名只在"新增可选字段"级别变化（往 ChatRequest 加字段属于加，改方法签名属于改）。
//
// 为什么不让 B 自己写 HTTP：重试、超时、限流、token 计费、按场景路由模型这五件事必须只有一个
// 地方做，散到 Agent 的十个节点里就再也对不上账，而项目后半段预算只剩三位数。
type Client interface {
	// Chat 发起一次非流式调用，同步拿完整结果。
	//
	// ctx 必须由调用方控制超时；本层会在 ctx 之外再叠一层 config 的超时，
	// 两层取更短的那个。ctx 取消时返回 CodeUpstreamTimeout。
	Chat(ctx context.Context, req ChatRequest) (ChatResponse, error)

	// ChatStream 发起流式调用，每个增量片段交给 onDelta。
	//
	// onDelta 返回非 nil error 时必须立刻终止上游连接并把这个 error 原样返回——
	// 不许吞掉、不许包成别的码，因为 B 用它实现"发现引用无源立即掐断生成"。
	// onDelta 在同一个 goroutine 里串行调用，B 不需要自己加锁，但不得阻塞超过 100ms。
	ChatStream(ctx context.Context, req ChatRequest, onDelta func(delta string) error) (ChatResponse, error)
}

// Message 是一条对话消息。
//
// ToolCalls 与 ToolCallID 只在工具循环里用：assistant 消息带 ToolCalls 表示模型要求调工具，
// 随后必须补一条 role=tool 且 ToolCallID 相同的消息回填结果，否则上游会 400。
type Message struct {
	// Role 是说话人。
	Role Role `json:"role"`
	// Content 是文本正文。工具调用消息可以为空串。
	Content string `json:"content,omitempty"`
	// ToolCalls 只在 Role 为 assistant 时有值，表示模型发起的工具调用请求。
	ToolCalls []ToolCall `json:"tool_calls,omitempty"`
	// ToolCallID 只在 Role 为 tool 时有值，必须与对应 ToolCall.ID 一致。
	ToolCallID string `json:"tool_call_id,omitempty"`
}

// ToolSpec 是工具定义，由 B 编写，A 原样透传给上游不做任何解析。
//
// Parameters 用 json.RawMessage 而不是结构化类型：各厂商对 JSON Schema 的支持有差异，
// A 层一旦解析再序列化就会吃掉 draft 关键字（如 $ref、anyOf），导致上游报参数不合法。
type ToolSpec struct {
	// Name 是工具名，必须在同一次请求内唯一。
	Name string `json:"name"`
	// Description 给模型看，决定它会不会选这个工具，写得越具体越好。
	Description string `json:"description"`
	// Parameters 是该工具入参的 JSON Schema 原文。
	Parameters json.RawMessage `json:"parameters"`
}

// ToolCall 是模型发起的一次工具调用。A 只回传，执行者是 B。
type ToolCall struct {
	// ID 由上游生成，回填 tool 消息时必须原样带回。
	ID string `json:"id"`
	// Name 对应 ToolSpec.Name。
	Name string `json:"name"`
	// Arguments 是模型生成的实参 JSON 原文。可能不合法，校验与兜底是 B 的责任。
	Arguments json.RawMessage `json:"arguments"`
}

// JSONSchema 用于要求模型按结构输出（合同要素抽取必须用它，否则 5% 的场次会返回散文）。
//
// Schema 同样是 json.RawMessage：A 层不做校验，只透传。
type JSONSchema struct {
	// Name 是给上游的 schema 名，出现在部分厂商的 tool 定义里。
	Name string
	// Schema 是 JSON Schema 原文。
	Schema json.RawMessage
	// Strict 为 true 时要求上游严格按 schema 输出，不支持该开关的上游忽略此项。
	Strict bool
}

// Usage 是一次调用的 token 计量，A 负责写库与成本估算，B 不需要关心。
type Usage struct {
	// PromptTokens 是输入 token 数。
	PromptTokens int `json:"prompt_tokens"`
	// CompletionTokens 是输出 token 数。
	CompletionTokens int `json:"completion_tokens"`
	// TotalTokens 是两者之和，上游给了就用上游的，没给由本层相加。
	TotalTokens int `json:"total_tokens"`
	// CostEst 是按 config 里的单价估算的人民币金额，用于日成本闸。
	CostEst float64 `json:"cost_est"`
}

// ChatRequest 是一次模型调用的完整入参。
//
// 字段全部由 B 填，A 只在 Model 为空时按 Scene 路由，并注入 TraceID。
type ChatRequest struct {
	// Scene 决定路由到哪个模型、走哪条限流桶。取值见 config 的 scene 表。
	// 常见值："consult"（强模型）、"qa"（中）、"review.extract"（便宜）、"review.advise"（强）。
	Scene string
	// Model 留空表示交给 A 按 Scene 路由；填了就按填的来（仅 dev 调试与压测使用）。
	Model string
	// Messages 是上下文，长度由 B 控制，A 不做裁剪。
	Messages []Message
	// Tools 由 B 定义，A 原样透传。
	Tools []ToolSpec
	// Temperature 采样温度，0 表示用上游默认。
	Temperature float64
	// MaxTokens 输出上限，0 表示用上游默认。
	MaxTokens int
	// JSONSchema 非空时要求结构化输出。
	JSONSchema *JSONSchema
	// TraceID 由 A 自动注入，B 填了也会被覆盖。
	TraceID string
}

// ChatResponse 是一次调用结果。
//
// 即使上游返回 200 但 body 是错误 JSON，也必须是 error 而不能是"空 Content 的成功"，
// 否则 B 会把空字符串当成模型答不出来，误判成检索没命中。
type ChatResponse struct {
	// Content 是文本正文，可能为空（纯工具调用轮次）。
	Content string
	// ToolCalls 是模型要求执行的工具，B 负责执行。
	ToolCalls []ToolCall
	// Usage 是本轮计量，A 已异步落库。
	Usage Usage
	// LatencyMS 是上游耗时，不含本层重试的等待。
	LatencyMS int
}
