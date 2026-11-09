package retrieval

import (
	"context"
	"math"
	"sort"

	"laborlaw/internal/port"
)

// Embedder 是语义检索对向量化能力的最小需求。
//
// 声明在使用侧而不是 port：port 是两人共用的冻结契约，为一个只在 retrieval
// 内部用的能力加接口，等于让另一侧为它签字。*llm.EmbeddingClient 天然满足它。
type Embedder interface {
	Embed(ctx context.Context, model string, texts []string) ([][]float32, error)
}

// scorePrecision 是相似度的舍入位数（1e-6）。
//
// 点积在 float32 下会给出 1.0000001 与 1.0 这种本该相等的值，不做舍入的话
// 名次会因浮点噪声颠倒，同一份数据两次排序结果就不同。
const scorePrecision = 1e6

// Semantic 是进程内的向量索引：启动时把整张 embeddings 表读进来，检索时算余弦。
//
// 205 条 × 1024 维 × 4 字节 ≈ 840KB，全量载入比每次查库更简单也更快。
// 索引里只存 ID 与向量：存全文会让内存翻十倍，而正文本来就在 statutes 表里。
type Semantic struct {
	ids  []string    // 与 vecs 一一对应，取自 embeddings.owner_id
	vecs [][]float32 // 已归一化，余弦因此退化成点积
	dim  int
}

// NewSemantic 建索引，并把每条向量原地归一化。
//
// 空输入返回可用但为空的索引：灰度阶段向量库可能还没建，不能因此让服务起不来。
func NewSemantic(items []port.Embedding) (*Semantic, error) {
	s := &Semantic{
		ids:  make([]string, 0, len(items)),
		vecs: make([][]float32, 0, len(items)),
	}
	for i := range items {
		item := &items[i]
		if len(item.Vector) == 0 {
			return nil, port.New(port.CodeInvalidArgument, "向量为空：owner_id=%s", item.OwnerID)
		}
		if s.dim == 0 {
			s.dim = len(item.Vector)
		} else if len(item.Vector) != s.dim {
			// embeddings 表的主键是 (owner_type, owner_id, model)，没有任何约束
			// 保证同一 model 的行维度相同：换模型时只跑了一半，或者上游网关做过
			// pool 混用，库里就会混着两种维度。这一层不查，点积就得自己处理
			// 长度不等的向量（越界或静默截断，分数"看起来像那么回事"）。
			return nil, port.New(port.CodeInvalidArgument,
				"向量维度不一致：owner_id=%s 是 %d 维，首条是 %d 维", item.OwnerID, len(item.Vector), s.dim)
		}
		norm, err := normalizeInPlace(item.Vector)
		if err != nil {
			return nil, port.Wrap(port.CodeInvalidArgument, err, "向量无法归一化：owner_id=%s", item.OwnerID)
		}
		if norm == 0 {
			return nil, port.New(port.CodeInvalidArgument,
				"向量是零向量，余弦恒为 0 且会占着 topK 名额：owner_id=%s", item.OwnerID)
		}
		s.ids = append(s.ids, item.OwnerID)
		s.vecs = append(s.vecs, item.Vector)
	}
	return s, nil
}

// Len 返回索引条数。
func (s *Semantic) Len() int {
	if s == nil {
		return 0
	}
	return len(s.ids)
}

// Search 按余弦相似度返回 topK 条，score 降序、同分按 owner_id 升序。
//
// minScore <= 0 表示不过滤。语义相似度的绝对值没有跨模型可比性，设一个默认
// 阈值等于埋一个"换模型就失效"的暗坑；阈值只在显式调用点给。
//
// 返回的 StatuteHit 只填 Statute.ID 与 Score，其余字段（Law/Article/Text/URL）
// 由调用方回表补全——索引里没存正文。
func (s *Semantic) Search(query []float32, topK int, minScore float32) ([]port.StatuteHit, error) {
	if topK <= 0 {
		topK = 5
	}
	if s.Len() == 0 {
		return []port.StatuteHit{}, nil
	}
	if len(query) != s.dim {
		return nil, port.New(port.CodeInvalidArgument,
			"查询向量维度 %d 与索引维度 %d 不一致", len(query), s.dim)
	}

	q := make([]float32, len(query))
	copy(q, query)
	norm, err := normalizeInPlace(q)
	if err != nil {
		return nil, port.Wrap(port.CodeInvalidArgument, err, "查询向量无法归一化")
	}
	if norm == 0 {
		// 零向量归一化会除零得 NaN，而 NaN 参与比较的结果是"既不大于也不小于"，
		// 排序会在不同实现上给出不同顺序。
		return nil, port.New(port.CodeInvalidArgument, "查询向量是零向量")
	}

	// 205 条用全排序就够：可读性优先，n log n 在这个量级上是微秒级。
	hits := make([]port.StatuteHit, 0, len(s.ids))
	for i, vec := range s.vecs {
		score := roundScore(dot(q, vec))
		if minScore > 0 && score < minScore {
			continue
		}
		hits = append(hits, port.StatuteHit{
			Statute: port.Statute{ID: s.ids[i]},
			Score:   float64(score),
		})
	}

	sort.SliceStable(hits, func(i, j int) bool {
		if hits[i].Score != hits[j].Score {
			return hits[i].Score > hits[j].Score
		}
		// 末级兜底必须有：同分时没有它，两次查询的顺序取决于 sort 的实现细节。
		return hits[i].Statute.ID < hits[j].Statute.ID
	})
	if len(hits) > topK {
		hits = hits[:topK]
	}
	return hits, nil
}

// dot 是归一化向量的点积，即余弦相似度。
func dot(a, b []float32) float32 {
	var sum float32
	for i := range a {
		sum += a[i] * b[i]
	}
	return sum
}

// normalizeInPlace 把向量原地除以其模长，返回原来的模长。
//
// 返回模长而不是只返回 error：调用方要区分"零向量"（模长 0）与"数值坏了"
// （NaN/Inf），两者的处理不同——前者是数据问题，后者是上游给了畸形数据。
func normalizeInPlace(vec []float32) (float32, error) {
	var sum float64
	for _, v := range vec {
		sum += float64(v) * float64(v)
	}
	norm := float32(math.Sqrt(sum))
	if math.IsNaN(float64(norm)) || math.IsInf(float64(norm), 0) {
		return 0, port.New(port.CodeInvalidArgument, "向量含 NaN 或 Inf")
	}
	if norm == 0 {
		return 0, nil
	}
	for i := range vec {
		vec[i] = float32(float64(vec[i]) / float64(norm))
	}
	return norm, nil
}

// roundScore 把相似度按 1e-6 舍入，消掉点积的浮点噪声。
func roundScore(v float32) float32 {
	return float32(math.Round(float64(v)*scorePrecision) / scorePrecision)
}
