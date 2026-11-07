// Package agent 实现 port.Runner：把一次用户输入编排成一组 SSE 事件。
//
// 本包是 B 侧的核心交付物，但只依赖 port 与 prompt，不 import gin / store / stream。
// 这样 graph 可以在没有任何基础设施的情况下用 FakeClient 跑通全部单元测试，
// 也正是 port.Runner 注释里"B 不需要知道 HTTP 存在"的含义。
package agent

import (
	"context"
	"hash/fnv"
	"log/slog"
	"strings"
	"sync"
	"unicode/utf8"

	"laborlaw/internal/port"
)

// 场景名与 internal/llm 的场景常量保持一致。
//
// 这里重复定义而不是 import llm：agent 只能依赖 port.Client 接口，
// 一旦 import 具体实现，B 就没法在没有 API key 的环境里编译自己的测试。
const (
	SceneConsult = "consult"
	SceneQA      = "qa"
	SceneReview  = "review"
)

// maxConsultInputRunes 对齐 contract/openapi.yaml 里 ChatRequest.input 的 maxLength=4000。
// 按 rune 计而不是 len()：一个汉字是 3 字节，用字节数会把 4000 字的中文描述当成 12000 字拒掉。
const maxConsultInputRunes = 4000

// StatuteSearcher 是 agent 需要的最小检索能力：一句话进来，一组可引用的条文出去。
//
// 入参是用户原话而不是关键词，是有意的分层的：查询清洗、同义归一、多路召回与排序
// 都属于检索策略，应当能在不动编排的情况下独立演进（以后加向量召回也一样）。
// internal/retrieval.Retriever 满足本接口，装配时直接传入。
type StatuteSearcher interface {
	Search(ctx context.Context, input string, topK int) ([]port.StatuteHit, error)
}

// MessageHistory 是 agent 需要的最小历史读取能力。
type MessageHistory interface {
	ListBySession(ctx context.Context, sessionID string, limit int) ([]port.ChatMessage, error)
}

// 编译期断言：签名跑偏时编译失败，而不是等到 /chat/completions 返回 501 才发现。
var _ port.Runner = (*Runner)(nil)

// Runner 是 port.Runner 的默认实现。
type Runner struct {
	llm      port.Client
	history  MessageHistory
	statutes StatuteSearcher
	log      *slog.Logger

	historyLimit int
	retrieveTopK int
	maxTokens    int

	// locks 是按会话散列的条带锁，实现 port.Runner 要求的"同 SessionID 串行化"。
	//
	// 为什么不用 map[sessionID]*sync.Mutex：那个 map 会随会话数无限增长，
	// 而这台演示服务器只有 2G 内存。64 条带换来的是两个不同会话偶尔互相等待，
	// 对本项目的并发量完全没有影响。
	locks [64]sync.Mutex
}

// Option 是可选依赖与参数注入点。
type Option func(*Runner)

// WithLogger 注入结构化日志。
func WithLogger(l *slog.Logger) Option {
	return func(r *Runner) {
		if l != nil {
			r.log = l
		}
	}
}

// WithHistory 注入历史读取能力。不注入时每轮都是无状态问答。
func WithHistory(h MessageHistory) Option {
	return func(r *Runner) { r.history = h }
}

// WithStatutes 注入法条检索能力。不注入时不检索、不发 citation，
// 模型被提示词约束为"不知道就说不知道"，而不是自由发挥。
func WithStatutes(s StatuteSearcher) Option {
	return func(r *Runner) { r.statutes = s }
}

// WithHistoryLimit 设置最多装载多少条历史消息。
func WithHistoryLimit(n int) Option {
	return func(r *Runner) {
		if n > 0 {
			r.historyLimit = n
		}
	}
}

// WithRetrieveTopK 设置每次检索注入多少条法条。
func WithRetrieveTopK(n int) Option {
	return func(r *Runner) {
		if n > 0 {
			r.retrieveTopK = n
		}
	}
}

// WithMaxTokens 设置单次回答的输出上限。
func WithMaxTokens(n int) Option {
	return func(r *Runner) {
		if n > 0 {
			r.maxTokens = n
		}
	}
}

// New 构造 Runner。llm 为 nil 时所有场景返回 50001，而不是在首次请求时 panic。
func New(llm port.Client, opts ...Option) *Runner {
	r := &Runner{
		llm:          llm,
		log:          slog.Default(),
		historyLimit: 10,
		retrieveTopK: 3,
		maxTokens:    1200,
	}
	for _, opt := range opts {
		opt(r)
	}
	return r
}

// Run 按场景分发到具体编排。
//
// 不自己发 done：port.Runner 的契约是 A 在返回后补发，B 多发一次前端就会结束两次。
func (r *Runner) Run(ctx context.Context, req port.Request, emit func(port.Event) error) (port.Result, error) {
	if r.llm == nil {
		return port.Result{}, port.New(port.CodeInternal, "模型客户端未装配")
	}
	if emit == nil {
		return port.Result{}, port.New(port.CodeInvalidArgument, "emit 不能为 nil")
	}
	if strings.TrimSpace(req.Input) == "" {
		return port.Result{}, port.New(port.CodeInvalidArgument, "输入不能为空")
	}
	if n := utf8.RuneCountInString(req.Input); n > maxConsultInputRunes {
		return port.Result{}, port.New(port.CodeInvalidArgument, "input 超过 %d 字", maxConsultInputRunes)
	}

	unlock := r.lockSession(req.SessionID)
	defer unlock()

	switch req.Scene {
	case SceneConsult, SceneQA, "":
		return r.runConsult(ctx, req, emit)
	case SceneReview:
		// review 编排依赖规则引擎与抽取 schema，在它们落地前明确报错，
		// 而不是悄悄退回 consult 给出一段没有审查依据的闲聊。
		return port.Result{}, port.New(port.CodeInvalidArgument, "合同审查编排尚未接入")
	default:
		return port.Result{}, port.New(port.CodeInvalidArgument, "未知场景 %q", req.Scene)
	}
}

// lockSession 对同一会话串行化。
//
// 不串行的后果很具体：用户连点两次发送，两轮 Run 同时读历史、同时写回，
// 消息顺序就会交错，下一轮看到的上下文是乱的。
// 空 SessionID 表示无历史调用（如单次抽取），不需要锁。
func (r *Runner) lockSession(sessionID string) func() {
	if sessionID == "" {
		return func() {}
	}
	h := fnv.New32a()
	_, _ = h.Write([]byte(sessionID))
	m := &r.locks[h.Sum32()%uint32(len(r.locks))]
	m.Lock()
	return m.Unlock
}
