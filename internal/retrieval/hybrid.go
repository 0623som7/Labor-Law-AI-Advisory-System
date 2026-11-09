package retrieval

import (
	"context"
	"log/slog"
	"sort"
	"strings"

	"laborlaw/internal/port"
)

// StatuteGetter 按 ID 回表取条文正文。声明在使用侧，理由同 Embedder：
// port 是两人共用的冻结契约，不为一处便利加方法。*store.Statutes 天然满足。
type StatuteGetter interface {
	Get(ctx context.Context, statuteID string) (*port.Statute, error)
}

// EmbeddingLoader 读全量向量，供启动时建索引。
type EmbeddingLoader interface {
	LoadEmbeddings(ctx context.Context, ownerType, model string) ([]port.Embedding, error)
}

// rrfK 是 RRF 的平滑常数。
//
// 60 是 RRF 原论文的取值：它让两个名次相邻的条目分差足够小，
// 从而"两路都排第二"能压过"一路第一、一路没有"，这正是融合想要的偏向。
const rrfK = 60

// Hybrid 是关键词召回与语义召回的融合检索。
//
// 两路候选各取 topK*2：融合会改变名次，候选只取 topK 的话，
// 某一路排第 4、另一路排第 1 的条文在融合后本该进榜，却在候选阶段就被截掉了。
type Hybrid struct {
	keyword  *Retriever
	semantic *Semantic
	embedder Embedder
	getter   StatuteGetter
	model    string
	log      *slog.Logger
	cache    VectorCache
	qScope   string
	reranker Reranker
}

// NewHybrid 构造融合检索。log 为 nil 时用 slog.Default()。
func NewHybrid(keyword *Retriever, semantic *Semantic, embedder Embedder, getter StatuteGetter, model string, log *slog.Logger, opts ...HybridOption) *Hybrid {
	if log == nil {
		log = slog.Default()
	}
	h := &Hybrid{
		keyword:  keyword,
		semantic: semantic,
		embedder: embedder,
		getter:   getter,
		model:    model,
		log:      log,
	}
	for _, opt := range opts {
		opt(h)
	}
	return h
}

// HybridOption 是融合检索的可选依赖。
//
// 用变参而不是改构造签名：NewHybrid 的调用点已有测试与 cmd 两处，
// 加一个必填参数会让所有调用点都得改一遍，而缓存本来就是可选的。
type HybridOption func(*Hybrid)

// WithVectorCache 注入查询向量缓存。传 nil 等于不缓存。
//
// scope 是缓存键的命名空间（服务商 + 模型），由装配点拼好传进来：
// 缓存层拿不到 config，也不该为了一行字符串去 import 它。
// 传空串时退回模型名，既有调用点因此不必全部改写。
func WithVectorCache(c VectorCache, scope string) HybridOption {
	return func(h *Hybrid) {
		if c != nil {
			h.cache = c
		}
		h.qScope = scope
	}
}

// scope 返回缓存键的命名空间。为空时退回模型名，保证既有测试与调用点行为不变。
func (h *Hybrid) scope() string {
	if h.qScope != "" {
		return h.qScope
	}
	return h.model
}

// Len 返回语义索引条数，供装配点判断向量库是否为空。
func (h *Hybrid) Len() int {
	if h == nil {
		return 0
	}
	return h.semantic.Len()
}

// Search 先用关键词召回，再叠加语义召回，按 RRF 融合后返回 topK 条。
//
// 语义这一侧任何失败都降级成"只用关键词结果"：向量化要过一次公网，
// 索引也可能还没建。这类失败不该让 /chat/completions 返 5xx——
// 用户问的是法律问题，少一条召回比整轮报错轻得多。
func (h *Hybrid) Search(ctx context.Context, input string, topK int) ([]port.StatuteHit, error) {
	if topK <= 0 {
		topK = 5
	}

	// 1. 关键词召回是主路，它失败才算真失败。
	keywordHits, err := h.keyword.Search(ctx, input, topK*2)
	if err != nil {
		return nil, err
	}

	// 2. 索引空或没有向量化能力时直接收工：这一轮省掉一次公网往返，
	//    在向量库还没灌的灰度期，每一次问答都会走这条分支。
	if h.semantic.Len() == 0 || h.embedder == nil {
		// 降级也要重排：语义那路没启用时，交叉编码器仍然是"例外条款翻盘"的唯一手段。
		return h.rerankHits(ctx, input, keywordHits, topK), nil
	}

	// 3. 查询向量：先查缓存，miss 才走公网。
	//    同一句话在彩排、试讲、正式演示里会被问很多遍，每一次都是一次公网往返。
	var queryVec []float32
	key := QueryVectorKey(h.scope(), input)
	if h.cache != nil {
		// 维度必须校验：旧模型留下的 1024 维向量喂给 1536 维索引会在点积里越界。
		if cached, ok := h.cache.GetVector(ctx, key); ok && len(cached) == h.semantic.Dims() {
			queryVec = cached
		}
	}
	if queryVec == nil {
		// 空切片与 error 同等对待：都表示这一轮拿不到查询向量。
		vecs, err := h.embedder.Embed(ctx, h.model, []string{input})
		if err != nil {
			h.log.Warn("语义检索降级，仅用关键词结果", "err", err.Error())
			return h.rerankHits(ctx, input, keywordHits, topK), nil
		}
		if len(vecs) == 0 || len(vecs[0]) == 0 {
			h.log.Warn("语义检索降级，仅用关键词结果", "reason", "向量化返回空结果")
			return h.rerankHits(ctx, input, keywordHits, topK), nil
		}
		queryVec = vecs[0]
		if h.cache != nil {
			h.cache.SetVector(ctx, key, queryVec)
		}
	}

	// 4. 向量召回。minScore 传 0：语义相似度没有跨模型可比的绝对阈值，
	//    这一层只负责排序，阈值只在显式调用点给。
	semanticHits, err := h.semantic.Search(queryVec, topK*2, 0)
	if err != nil {
		h.log.Warn("语义检索降级，仅用关键词结果", "err", err.Error())
		return h.rerankHits(ctx, input, keywordHits, topK), nil
	}

	// 5. RRF 融合：分数只由名次决定，不把余弦与关键词分相加——
	//    余弦在 [0,1]，关键词分是命中词数加权，量纲不同，
	//    相加等于让量级大的那一路单方面决定排序。
	score := make(map[string]float64, len(keywordHits)+len(semanticHits))
	found := make(map[string]port.StatuteHit, len(keywordHits)+len(semanticHits))
	for i, hit := range keywordHits {
		id := hit.Statute.ID
		score[id] += 1.0 / float64(rrfK+i+1)
		found[id] = hit
	}
	for i, hit := range semanticHits {
		id := hit.Statute.ID
		score[id] += 1.0 / float64(rrfK+i+1)
		if _, ok := found[id]; !ok {
			// 语义这路只带 ID 与 Score，正文等回表补。
			found[id] = hit
		}
	}

	ids := make([]string, 0, len(score))
	for id := range score {
		ids = append(ids, id)
	}
	sort.SliceStable(ids, func(i, j int) bool {
		if score[ids[i]] != score[ids[j]] {
			return score[ids[i]] > score[ids[j]]
		}
		// map 遍历顺序随机，同分必须有确定的末级比较，
		// 否则同一个问题两次提问给出的引用卡片顺序不一样。
		return ids[i] < ids[j]
	})
	// 候选窗口比 topK 宽：重排要能看到"本来排在 topK 之外、但正是用户问的那条例外"
	// 的条文，否则它没有翻盘机会。没开重排时这一步只是多补几条正文，最后仍截回 topK。
	if len(ids) > maxRerankCandidates {
		ids = ids[:maxRerankCandidates]
	}

	// 6. 回表补全：只补正文为空的（即向量独有条目）。
	//    关键词那路本来就带全文，逐条再查一次是白扔五次查询。
	out := make([]port.StatuteHit, 0, len(ids))
	for _, id := range ids {
		hit := found[id]
		if strings.TrimSpace(hit.Statute.Text) == "" {
			full, err := h.getter.Get(ctx, id)
			if err != nil {
				if port.IsCode(err, port.CodeNotFound) {
					// 向量表里有、法条表里没有：种子数据换过，静默丢弃即可。
					continue
				}
				h.log.Warn("回表补全法条失败，丢弃该条", "statute_id", id, "err", err.Error())
				continue
			}
			hit.Statute = *full
		}
		// 补完仍没有正文的也丢：宁可少一张卡片，也不能给前端一张空卡片。
		if strings.TrimSpace(hit.Statute.Text) == "" {
			continue
		}
		// Score 回填融合分：对外只有一个 score 字段，排序与展示必须同一个数。
		// 保留原始分会让调用方看到"0.63 排在 7.5 前面"。
		hit.Score = score[id]
		out = append(out, hit)
	}

	// 7. 交叉编码器重排，然后截到 topK。失败保留融合顺序。
	return h.rerankHits(ctx, input, out, topK), nil
}

// SearchIn 是 Search 的带法律来源版本，供 /statutes 接口复用同一个检索器。
//
// law 非空时直接退回关键词那一路：embeddings 表只有 owner_type/owner_id，
// 没有法律来源维度，向量这路筛不出"只要劳动合同法"，
// 融合进来就会把别的法律的条文混进一个声称按法律过滤的结果里。
func (h *Hybrid) SearchIn(ctx context.Context, input, law string, topK int) ([]port.StatuteHit, error) {
	if strings.TrimSpace(law) != "" {
		// 先按 law 过滤再重排：候选窗口同样放宽到 maxRerankCandidates，
		// 否则重排只在 topK 条里调顺序，等于没给例外条款留机会。
		hits, err := h.keyword.SearchIn(ctx, input, law, maxRerankCandidates)
		if err != nil {
			return nil, err
		}
		return h.rerankHits(ctx, input, hits, topK), nil
	}
	return h.Search(ctx, input, topK)
}

// clip 截断到 topK，nil 与空切片统一成空切片。
func clip(hits []port.StatuteHit, topK int) []port.StatuteHit {
	if len(hits) > topK {
		hits = hits[:topK]
	}
	if hits == nil {
		return []port.StatuteHit{}
	}
	return hits
}

// LoadSemanticIndex 启动时建索引。返回 nil, nil 表示"没有可用向量"，
// 调用方据此退回纯关键词检索——灰度期向量库没建不能算启动失败。
func LoadSemanticIndex(ctx context.Context, loader EmbeddingLoader, ownerType, model string) (*Semantic, error) {
	if loader == nil {
		return nil, nil
	}
	items, err := loader.LoadEmbeddings(ctx, ownerType, model)
	if err != nil {
		return nil, err
	}
	if len(items) == 0 {
		return nil, nil
	}
	// 维度不一致、零向量这些是数据问题，返回 error 让装配点决定降级还是失败；
	// 装配点选择降级（见 cmd/api/runner.go），这里不替它做主。
	return NewSemantic(items)
}
