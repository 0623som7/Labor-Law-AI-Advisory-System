package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"unicode/utf8"

	"laborlaw/internal/port"
)

const consultScene = "consult"

// maxConsultInputRunes 与 contract/openapi.yaml 的规则保持一致。
// 按 rune 计，一个汉字算 1 个符。
const maxConsultInputRunes = 4000

// Runner 是 B 提供给 A 的最小可用实现。
//
// 先做两条链路里最关键的 consult：问题 → 检索法条 → 拼 prompt → 流式回答。
// 其他场景后续按同样的入口扩展，避免一开始就给一个庞然大物。
type Runner struct {
	store  port.StatuteStore
	client port.Client
}

// NewRunner 构造一个 Runner。
func NewRunner(store port.StatuteStore, client port.Client) *Runner {
	return &Runner{store: store, client: client}
}

// Run 实现一次请求的编排。
func (r *Runner) Run(ctx context.Context, req port.Request, emit func(port.Event) error) (port.Result, error) {
	if err := validateInput(req.Scene, req.Input); err != nil {
		return port.Result{}, err
	}
	if r == nil || r.client == nil {
		return port.Result{}, port.New(port.CodeInternal, "缺少模型客户端")
	}

	hits, err := r.searchStatutes(ctx, emit, req.Input)
	if err != nil {
		return port.Result{}, port.Wrap(port.CodeRuleEngine, err, "法条检索失败")
	}

	if err := emitCitations(emit, hits); err != nil {
		return port.Result{}, err
	}

	messages := []port.Message{
		{Role: port.RoleSystem, Content: buildSystemPrompt(hits)},
		{Role: port.RoleUser, Content: req.Input},
	}

	var content strings.Builder
	response, err := r.client.ChatStream(ctx, port.ChatRequest{
		Scene:       "consult",
		Messages:    messages,
		Temperature: 0.2,
	}, func(delta string) error {
		if delta == "" {
			return nil
		}
		content.WriteString(delta)
		return emit(port.Event{Type: port.AgentEventDelta, Text: delta})
	})
	if err != nil {
		return port.Result{}, port.Wrap(port.CodeUpstream, err, "模型生成失败")
	}

	return port.Result{
		Content:      content.String(),
		Usage:        response.Usage,
		FinishReason: "stop",
	}, nil
}

func validateInput(scene, input string) error {
	if scene != consultScene {
		return port.New(port.CodeInvalidArgument, "暂不支持 %s 场景", scene)
	}
	if strings.TrimSpace(input) == "" {
		return port.New(port.CodeInvalidArgument, "input 不能为空")
	}
	if utf8.RuneCountInString(input) > maxConsultInputRunes {
		return port.New(port.CodeInvalidArgument, "input 超过 %d 字", maxConsultInputRunes)
	}
	return nil
}

func (r *Runner) searchStatutes(ctx context.Context, emit func(port.Event) error, input string) ([]port.StatuteHit, error) {
	if r.store == nil {
		return nil, errors.New("statute store is nil")
	}

	args, _ := json.Marshal(map[string]any{"query": input})
	if err := emitToolCall(emit, "statute_search", args, port.ToolRunning, "正在检索法条"); err != nil {
		return nil, err
	}

	hits, err := r.store.SearchText(ctx, input, "", 5)
	if err != nil {
		if toolErr := emitToolCall(emit, "statute_search", args, port.ToolFailed, "检索失败"); toolErr != nil {
			return nil, toolErr
		}
		return nil, err
	}

	detail := "命中 0 条法条"
	if len(hits) > 0 {
		detail = fmt.Sprintf("命中 %d 条法条", len(hits))
	}
	if err := emitToolCall(emit, "statute_search", args, port.ToolOK, detail); err != nil {
		return nil, err
	}
	return hits, nil
}

func buildSystemPrompt(hits []port.StatuteHit) string {
	var sb strings.Builder
	sb.WriteString("你是一个面向普通劳动者的法律助手，只能解释法条，不能编造新的法条。\n")
	if len(hits) == 0 {
		sb.WriteString("知识库暂未检索到相关法条，你可以先给出一般性说明，但不要伪装引用。")
		return sb.String()
	}
	sb.WriteString("答案请尽量结合以下法条：\n")
	for _, hit := range hits {
		sb.WriteString("- " + hit.Statute.Law + " " + hit.Statute.Article + "\n")
		sb.WriteString(hit.Statute.Text + "\n")
	}
	return sb.String()
}

func emitToolCall(emit func(port.Event) error, tool string, args []byte, status port.ToolRunStatus, detail string) error {
	if emit == nil {
		return nil
	}
	ev := port.Event{Type: port.AgentEventToolCall}
	ev.Tool = &port.ToolStatus{Tool: tool, Args: args, Status: status, Detail: detail}
	return emit(ev)
}

func emitCitations(emit func(port.Event) error, hits []port.StatuteHit) error {
	for _, hit := range hits {
		ev := port.Event{Type: port.AgentEventCitation}
		ev.Citation = &port.Citation{
			StatuteID: hit.Statute.ID,
			Law:       hit.Statute.Law,
			Article:   hit.Statute.Article,
			Text:      hit.Statute.Text,
			URL:       hit.Statute.URL,
		}
		if err := emit(ev); err != nil {
			return err
		}
	}
	return nil
}

