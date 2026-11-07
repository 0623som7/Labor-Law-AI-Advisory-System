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

// newHistory 装配会话历史读取。
//
// 与 newSearcher 同理：没有 MySQL 就返回 nil 而不是一个每次读都失败的实现。
// Runner 对 nil 历史的处理是"每轮都是无状态问答"，那是能力降级；
// 非 nil 的坏实现会让每轮问答都带一条读取失败告警，那是故障。
func newHistory(db *sql.DB) agent.MessageHistory {
	if db == nil {
		return nil
	}
	return store.NewMessages(db)
}

// newRunner 装配对话编排。
//
// 没有 API key 时保留演示桩而不是直接上真 Runner：真 Runner 在密钥为空时
// 只会让每次问答都以上游错误收场，演示反而倒退。降级必须在启动日志里喊出来，
// 否则评审时会把桩回答当成模型效果来验收。
//
// usage 是计量落库口，接在 llm.Client 上而不是 Runner 上：一次咨询会在 llm 层
// 发出一次（未来可能多次）调用，只有那里拿得到真实的 token 数与上游耗时；
// 挂在编排层只能记到"这一轮答了多久"，对不上账。
func newRunner(cfg config.Config, log *slog.Logger, statutes agent.StatuteSearcher, history agent.MessageHistory, usage llm.UsageRecorder) port.Runner {
	if cfg.LLM.APIKey == "" {
		log.Warn("LLM_API_KEY 为空，对话接口使用演示桩回答，不是真实模型输出")
		return newStubRunner(log)
	}

	opts := []agent.Option{agent.WithLogger(log)}
	if statutes != nil {
		opts = append(opts, agent.WithStatutes(statutes))
	}
	if history != nil {
		opts = append(opts, agent.WithHistory(history))
	}
	llmOpts := []llm.Option{llm.WithLogger(log)}
	if usage != nil {
		llmOpts = append(llmOpts, llm.WithUsageRecorder(usage))
	}
	return agent.New(llm.New(cfg.LLM, llmOpts...), opts...)
}
