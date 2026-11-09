package main

import (
	"context"
	"database/sql"
	"log/slog"
	"net/url"

	"github.com/redis/go-redis/v9"

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
//
// 开关关闭时返回值与引入向量之前完全一致（同一个 *retrieval.Retriever），
// 不额外包一层 Hybrid：灰度关闭就该是零回归，包括类型与调用路径。
//
// 开着但向量库没灌、或索引建不起来时一律退回关键词检索，并在日志里喊一声。
// 向量召回是锦上添花，不能因为索引没准备好就让咨询接口起不来或答不了。
func newSearcher(ctx context.Context, db *sql.DB, rdb *redis.Client, cfg config.Config, logger *slog.Logger) searcher {
	if db == nil {
		return nil
	}
	statutes := store.NewStatutes(db)
	keyword := retrieval.New(statutes)
	if !cfg.Retrieval.Semantic || cfg.LLM.APIKey == "" {
		return keyword
	}

	embedder := llm.NewEmbeddingClient(cfg.LLM, llm.WithEmbeddingLogger(logger))
	index, err := retrieval.LoadSemanticIndex(ctx, statutes, "statute", cfg.LLM.ModelEmbedding)
	if err != nil {
		logger.Warn("语义索引构建失败，退回关键词检索", "err", err.Error())
		return keyword
	}
	if index.Len() == 0 {
		logger.Info("向量库为空，退回关键词检索")
		return keyword
	}
	logger.Info("语义检索已启用", "vectors", index.Len(), "model", cfg.LLM.ModelEmbedding)
	// 查询向量缓存：同一句话第二次不再走公网。没 Redis 时它自己退化成不缓存，
	// 不需要在这里判空——那会让"Redis 挂了"变成"检索不工作"。
	return retrieval.NewHybrid(keyword, index, embedder, statutes, cfg.LLM.ModelEmbedding, logger,
		retrieval.WithVectorCache(store.NewVectorCache(rdb, logger), embedScope(cfg.LLM)))
}

// embedScope 标识"哪一家的哪个模型"。只带模型名不够：同名同维不同空间的
// 向量混在一起时，余弦仍然算得出数，只是名次没有意义。
//
// 用 host 而不是完整 BaseURL：路径部分（/compatible-mode/v1 之类）只是同一个
// 服务商的不同入口，跟着改会让整库缓存白扔一次；host 变了才真的换了服务商。
func embedScope(c config.LLMConfig) string {
	host := c.BaseURL
	if u, err := url.Parse(c.BaseURL); err == nil && u.Host != "" {
		host = u.Host
	}
	return c.ModelEmbedding + "@" + host
}

// searcher 是 cmd/api 对检索器的完整要求：编排层只要 Search，
// 而 /statutes 接口要 SearchIn（带法律来源）。两个窄接口各在各的包里定义，
// 这里用一个组合接口让同一份实例同时喂给两边——否则 /statutes 会另造一个检索器，
// 改词表时只生效一半。
type searcher interface {
	agent.StatuteSearcher
	SearchIn(ctx context.Context, input, law string, topK int) ([]port.StatuteHit, error)
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

// newLLM 是全进程唯一的模型客户端构造点。
//
// 必须唯一：llm.Client 内部带令牌桶与日预算闸，一个进程建两个客户端，
// LLM_QPS 与 LLM_DAILY_BUDGET_YUAN 实际都会变成两倍，而配置上看不出来。
// 对话（Runner）与合同审查（Extractor）共用同一个实例，闸门才真的是闸门。
//
// usage 是计量落库口，接在这里而不是编排层：一次咨询会在 llm 层发出一次
// （未来可能多次）调用，只有那里拿得到真实的 token 数与上游耗时；
// 挂在编排层只能记到"这一轮答了多久"，对不上账。
//
// 密钥为空时这里不拦：llm.New 不校验 key，是否启用真模型由 newRunner 的
// nil 分支决定，构造点保持纯粹。
func newLLM(cfg config.Config, log *slog.Logger, usage llm.UsageRecorder) *llm.Client {
	opts := []llm.Option{llm.WithLogger(log)}
	if usage != nil {
		opts = append(opts, llm.WithUsageRecorder(usage))
	}
	return llm.New(cfg.LLM, opts...)
}

// newRunner 装配对话编排。
//
// client 为 nil 表示没有配 LLM_API_KEY：保留演示桩而不是直接上真 Runner——
// 真 Runner 在密钥为空时只会让每次问答都以上游错误收场，演示反而倒退。
// 降级必须在启动日志里喊出来，否则评审时会把桩回答当成模型效果来验收。
func newRunner(log *slog.Logger, client *llm.Client, statutes agent.StatuteSearcher, history agent.MessageHistory) port.Runner {
	if client == nil {
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
	return agent.New(client, opts...)
}
