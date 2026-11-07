package main

import (
	"database/sql"
	"log/slog"

	"laborlaw/internal/agent"
	"laborlaw/internal/config"
	"laborlaw/internal/llm"
	"laborlaw/internal/port"
	"laborlaw/internal/retrieval"
	"laborlaw/internal/store"
)

// newSearcher 装配法条检索。
//
// 没有 MySQL 就返回 nil 而不是返回一个每次查询都失败的检索器：
// Runner 对 nil 检索器的处理是跳过检索并明说"本次没有可引用的法条"，
// 这属于能力降级；而后者会让每次问答都带一条检索失败，属于故障。
func newSearcher(db *sql.DB) *retrieval.Retriever {
	if db == nil {
		return nil
	}
	return retrieval.New(store.NewStatutes(db))
}

// newRunner 装配对话编排。
//
// 没有 API key 时保留演示桩而不是直接上真 Runner：真 Runner 在密钥为空时
// 只会让每次问答都以上游错误收场，演示反而倒退。降级必须在启动日志里喊出来，
// 否则评审时会把桩回答当成模型效果来验收。
func newRunner(cfg config.Config, log *slog.Logger, statutes agent.StatuteSearcher) port.Runner {
	if cfg.LLM.APIKey == "" {
		log.Warn("LLM_API_KEY 为空，对话接口使用演示桩回答，不是真实模型输出")
		return newStubRunner(log)
	}

	opts := []agent.Option{agent.WithLogger(log)}
	if statutes != nil {
		opts = append(opts, agent.WithStatutes(statutes))
	}
	// WithHistory 等 Message DAL 落地后再接：会话历史现在还没落库，
	// 接上只会让每次问答多一条 degraded 告警，不会多出任何能力。
	return agent.New(llm.New(cfg.LLM, llm.WithLogger(log)), opts...)
}
