package retrieval

import (
	"context"
	"sort"

	"laborlaw/internal/port"
)

// Store 是检索需要的最小存储能力，port.StatuteStore 天然满足它。
type Store interface {
	SearchText(ctx context.Context, query, law string, topK int) ([]port.StatuteHit, error)
}

// Retriever 是面向编排层的检索入口：一句话进来，一组可引用的条文出去。
type Retriever struct {
	store       Store
	perQueryK   int
	maxKeywords int
}

// Option 调整检索参数。
type Option func(*Retriever)

// WithPerQueryTopK 设置每个关键词取多少条候选。
//
// 取少了会在多词合并时丢掉真正相关的那条；取多了只是把排序工作挪到内存里，
// 对 500 条规模的法条库，5 是一个够用的折中。
func WithPerQueryTopK(n int) Option {
	return func(r *Retriever) {
		if n > 0 {
			r.perQueryK = n
		}
	}
}

// New 构造检索器。store 为 nil 时所有检索返回空结果，编排层据此走"没有法条"的分支。
func New(store Store, opts ...Option) *Retriever {
	r := &Retriever{store: store, perQueryK: 5, maxKeywords: maxKeywords}
	for _, opt := range opts {
		opt(r)
	}
	return r
}

// 编译期断言：存储域的完整接口必须能直接当检索后端用，
// 否则装配时要多写一层只有一行转发的适配器。
var _ Store = (port.StatuteStore)(nil)

// Search 提取关键词、逐词检索、合并去重后按相关度排序。
//
// 排序先看"命中了几个不同的关键词"，再看存储给的相关度分数：
// 用户问"辞退有没有经济补偿"时，同时命中"解除"与"经济补偿"的条文
// 几乎一定比只命中"解除"的条文更贴合，而 LIKE 的分数反映不出这种组合关系。
func (r *Retriever) Search(ctx context.Context, input string, topK int) ([]port.StatuteHit, error) {
	if topK <= 0 {
		topK = 3
	}
	if r.store == nil {
		return []port.StatuteHit{}, nil
	}

	keywords := extractKeywords(input)
	if len(keywords) == 0 {
		return []port.StatuteHit{}, nil
	}

	type aggregate struct {
		hit     port.StatuteHit
		matched int
	}
	merged := make(map[string]*aggregate)
	var lastErr error

	for _, kw := range keywords {
		hits, err := r.store.SearchText(ctx, kw.term, "", kw.topK(r.perQueryK))
		if err != nil {
			// 单个关键词失败不拖垮整次检索：多词本来就是冗余召回，
			// 只要还有一个词查到了，就值得把结果给用户。
			lastErr = err
			continue
		}
		for _, h := range hits {
			id := h.Statute.ID
			agg, ok := merged[id]
			if !ok {
				agg = &aggregate{hit: h}
				merged[id] = agg
			}
			agg.matched++
			// 加权累加而不是取最大值：一条同时被"解除"和"经济补偿"命中的条文，
			// 比只被其中一个命中的更贴合，而单看最大分看不出这种组合。
			agg.hit.Score += kw.weight * h.Score
		}
	}

	if len(merged) == 0 {
		if lastErr != nil {
			return nil, lastErr
		}
		return []port.StatuteHit{}, nil
	}

	out := make([]*aggregate, 0, len(merged))
	for _, agg := range merged {
		out = append(out, agg)
	}
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].matched != out[j].matched {
			return out[i].matched > out[j].matched
		}
		if out[i].hit.Score != out[j].hit.Score {
			return out[i].hit.Score > out[j].hit.Score
		}
		// map 遍历顺序是随机的，必须有一个确定性的末级比较，
		// 否则同一个问题两次提问给出的引用卡片顺序不一样，用户会以为结果在变。
		return out[i].hit.Statute.ID < out[j].hit.Statute.ID
	})

	if len(out) > topK {
		out = out[:topK]
	}
	hits := make([]port.StatuteHit, 0, len(out))
	for _, agg := range out {
		hits = append(hits, agg.hit)
	}
	return hits, nil
}
