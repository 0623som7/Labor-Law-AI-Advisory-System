package agent

import (
	"context"
	"encoding/json"
	"strconv"
	"strings"

	"laborlaw/internal/port"
	"laborlaw/internal/prompt"
)

// runConsult 是劳动法咨询的编排：检索 → 组装上下文 → 流式生成。
//
// 三步的顺序不能换：检索结果要进系统提示词，而系统提示词必须是 Messages 的第一条；
// 如果边生成边检索，模型会先凭记忆说出条号，后面再用检索结果纠正已经来不及了。
func (r *Runner) runConsult(ctx context.Context, req port.Request, emit func(port.Event) error) (port.Result, error) {
	hits, err := r.retrieve(ctx, req.Input, emit)
	if err != nil {
		return port.Result{}, err
	}

	messages := make([]port.Message, 0, r.historyLimit+2)
	messages = append(messages, port.Message{
		Role:    port.RoleSystem,
		Content: prompt.ConsultWithStatutes(hits),
	})
	messages = append(messages, r.loadHistory(ctx, req.SessionID)...)
	messages = append(messages, port.Message{Role: port.RoleUser, Content: req.Input})

	var built strings.Builder
	resp, err := r.llm.ChatStream(ctx, port.ChatRequest{
		Scene:     consultScene(req.Scene),
		Messages:  messages,
		MaxTokens: r.maxTokens,
	}, func(delta string) error {
		// 先转发再累积：emit 失败意味着连接已断，继续累积只是在给没人看的内容记账。
		if err := emit(port.Event{Type: port.AgentEventDelta, Text: delta}); err != nil {
			return err
		}
		built.WriteString(delta)
		return nil
	})
	if err != nil {
		// 原样返回，不包装不换码：emit 的错误是 A 层用来判断"客户端断开"的，
		// 上游的错误码决定前端要不要重试，两者都不能在这里被改写。
		return port.Result{}, err
	}

	content := resp.Content
	if content == "" {
		content = built.String()
	}

	return port.Result{
		Content:      content,
		Usage:        resp.Usage,
		FinishReason: "stop",
	}, nil
}

// consultScene 把空场景归一到 consult，保证限流桶与计量落库的键是稳定的。
func consultScene(scene string) string {
	if scene == "" {
		return SceneConsult
	}
	return scene
}

// retrieve 检索相关法条并推送 tool_call 与 citation 事件。
//
// 检索失败不中断回答：没有法条时模型会按提示词说"需要核对原文"，
// 这比直接返回一个错误页对用户更有用，也不会让 MySQL 抖动升级成全站不可用。
// 未装配检索能力时直接返回 nil，且不发 tool_call —— 前端不该看到一个没跑过的工具状态。
func (r *Runner) retrieve(ctx context.Context, query string, emit func(port.Event) error) ([]port.StatuteHit, error) {
	if r.statutes == nil {
		return nil, nil
	}

	// Args 只放检索参数，不放用户原话：ToolStatus 会进 SSE 帧与前端日志，
	// 而用户输入里可能有姓名、公司名、身份证号。
	args, _ := json.Marshal(map[string]any{"topK": r.retrieveTopK})
	if err := emit(port.Event{
		Type: port.AgentEventToolCall,
		Tool: &port.ToolStatus{Tool: "search_statute", Args: args, Status: port.ToolRunning, Detail: "检索相关法条"},
	}); err != nil {
		return nil, err
	}

	hits, err := r.statutes.Search(ctx, query, r.retrieveTopK)
	if err != nil {
		r.log.Warn("statute search degraded", "err", err.Error())
		if emitErr := emit(port.Event{
			Type: port.AgentEventToolCall,
			Tool: &port.ToolStatus{Tool: "search_statute", Args: args, Status: port.ToolFailed, Detail: "法条检索暂不可用"},
		}); emitErr != nil {
			return nil, emitErr
		}
		return nil, nil
	}

	for _, h := range hits {
		s := h.Statute
		// Citation.Text 直接来自库，一个字都不经过模型 —— 这是"AI 不许直写法条原文"
		// 铁律的技术落点：模型只能决定引哪条，不能决定条文长什么样。
		if err := emit(port.Event{
			Type: port.AgentEventCitation,
			Citation: &port.Citation{
				StatuteID: s.ID,
				Law:       s.Law,
				Article:   s.Article,
				Text:      s.Text,
				URL:       s.URL,
			},
		}); err != nil {
			return nil, err
		}
	}

	if err := emit(port.Event{
		Type: port.AgentEventToolCall,
		Tool: &port.ToolStatus{
			Tool:   "search_statute",
			Args:   args,
			Status: port.ToolOK,
			Detail: "命中 " + strconv.Itoa(len(hits)) + " 条法条",
		},
	}); err != nil {
		return nil, err
	}
	return hits, nil
}

// loadHistory 读取会话历史并转成模型消息。
//
// 读不到历史时降级为无状态问答而不是报错：历史是增强项，
// 因为一次存储抖动就让用户完全问不了问题，是本末倒置。
func (r *Runner) loadHistory(ctx context.Context, sessionID string) []port.Message {
	if r.history == nil || sessionID == "" {
		return nil
	}

	records, err := r.history.ListBySession(ctx, sessionID, r.historyLimit)
	if err != nil {
		r.log.Warn("history load degraded", "session_id", sessionID, "err", err.Error())
		return nil
	}

	out := make([]port.Message, 0, len(records))
	for _, m := range records {
		// 只回放用户与助手消息：历史表里的 system 是旧版本提示词，回放会覆盖当前口径；
		// tool 消息缺少配对的 ToolCalls，直接发给上游会被 400 拒掉。
		if m.Role != port.RoleUser && m.Role != port.RoleAssistant {
			continue
		}
		if strings.TrimSpace(m.Content) == "" {
			continue
		}
		out = append(out, port.Message{Role: m.Role, Content: m.Content})
	}
	return out
}
