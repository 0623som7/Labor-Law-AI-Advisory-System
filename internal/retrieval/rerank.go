package retrieval

import (
	"context"
	"sort"

	"laborlaw/internal/port"
)

// Reranker 是交叉编码器重排的依赖。声明在使用侧，理由同 Embedder：
// port 是两人共用的冻结契约，不为一处便利往里加方法。*llm.RerankClient 天然满足。
type Reranker interface {
	Rerank(ctx context.Context, query string, docs []string) ([]float64, error)
}

// 候选数量窗口。
//
// 下限：候选只有三条时，重排最多把顺序调一调，而"该进来的那条根本没进候选"
// 才是真正的瓶颈——这个数以下省掉一次 HTTP 往返。
//
// 上限：文档建议 50~100 条，但我们只有 205 条法条、topK 通常 3~5。20 条已经是
// 召回结果的前四分之一，再往后排的条文由同一句口语命中并翻盘的可能性极低，
// 而 20 条要发一次 ~2s 的请求，条数再涨首字延迟会明显顶不住。
const (
	minRerankCandidates = 4
	maxRerankCandidates = 20
)

// WithReranker 注入交叉编码器重排。传 nil 等于不重排，行为与之前逐字一致。
func WithReranker(r Reranker) HybridOption {
	return func(h *Hybrid) {
		if r != nil {
			h.reranker = r
		}
	}
}

// rerankHits 用交叉编码器给候选重新打分并排序，然后截到 topK。
//
// 失败一律返回原顺序（连同原分数），只留一条 Warn：重排是可选增强，
// 上游欠费、限流、超时都不该让整轮问答 5xx——用户问的是法律问题，
// 少一层精度比拿不到回答轻得多。
//
// 分数不做绝对阈值：relevance_score 是"请求内相对分"，0.7 在长候选里算高、
// 在三候选里算中等，跨请求不可比。它只用来排序。
func (h *Hybrid) rerankHits(ctx context.Context, input string, hits []port.StatuteHit, topK int) []port.StatuteHit {
	if h.reranker == nil || len(hits) < minRerankCandidates {
		if h.reranker != nil {
			h.log.Debug("候选过少，跳过重排", "candidates", len(hits), "min", minRerankCandidates)
		}
		return clip(hits, topK)
	}

	docs := make([]string, len(hits))
	for i, hit := range hits {
		docs[i] = StatuteDocument(hit.Statute)
	}

	scores, err := h.reranker.Rerank(ctx, input, docs)
	if err != nil {
		h.log.Warn("重排失败，保留融合顺序", "err", err.Error(), "candidates", len(hits))
		return clip(hits, topK)
	}
	if len(scores) != len(hits) {
		// 理论上 Rerank 自己会拦，这里再守一道：少一条分数就会有一条拿到 0 分，
		// 排序结果看着像模型判断，实际是下标没对上。
		h.log.Warn("重排分数条数与候选不一致，保留融合顺序",
			"scores", len(scores), "candidates", len(hits))
		return clip(hits, topK)
	}

	out := make([]port.StatuteHit, len(hits))
	copy(out, hits)
	for i := range out {
		// 对外只有一个 score 字段，排序与展示必须同一个数：这里换成重排分，
		// 保留 RRF 分会让调用方看到"0.032 排在 0.7 前面"。
		out[i].Score = scores[i]
	}
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].Score != out[j].Score {
			return out[i].Score > out[j].Score
		}
		// 同分必须有确定的末级比较，否则同一个问题两次提问的引用卡片顺序不一样。
		return out[i].Statute.ID < out[j].Statute.ID
	})
	return clip(out, topK)
}
