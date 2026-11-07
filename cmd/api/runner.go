package main

import (
	"log/slog"

	"laborlaw/internal/agent"
	"laborlaw/internal/config"
	"laborlaw/internal/llm"
	"laborlaw/internal/port"
)

// newRunner 装配对话编排。
//
// 没有 API key 时保留演示桩而不是直接上真 Runner：真 Runner 在密钥为空时
// 只会让每次问答都以上游错误收场，演示反而倒退。降级必须在启动日志里喊出来，
// 否则评审时会把桩回答当成模型效果来验收。
func newRunner(cfg config.Config, log *slog.Logger) port.Runner {
	if cfg.LLM.APIKey == "" {
		log.Warn("LLM_API_KEY 为空，对话接口使用演示桩回答，不是真实模型输出")
		return newStubRunner(log)
	}

	// WithStatutes / WithHistory 等 Statute 与 Message 两个 DAL 落地后再接：
	// 现在接上只会让每次问答多一条 degraded 告警，不会多出任何能力。
	return agent.New(
		llm.New(cfg.LLM, llm.WithLogger(log)),
		agent.WithLogger(log),
	)
}
