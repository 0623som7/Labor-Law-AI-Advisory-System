package retrieval

import (
	"math"
	"testing"

	"laborlaw/internal/port"
)

// emb 造一条测试向量。
func emb(id string, vec ...float32) port.Embedding {
	return port.Embedding{OwnerType: "test", OwnerID: id, Model: "m1", Vector: vec}
}

func TestSemanticSearchRanksByCosine(t *testing.T) {
	s, err := NewSemantic([]port.Embedding{
		emb("LCL-1", 1, 0, 0),
		emb("LCL-2", 0, 1, 0),
		emb("LCL-3", 1, 1, 0),
	})
	if err != nil {
		t.Fatalf("NewSemantic 返回错误: %v", err)
	}
	if s.Len() != 3 {
		t.Fatalf("Len = %d, want 3", s.Len())
	}

	hits, err := s.Search([]float32{1, 0, 0}, 3, 0)
	if err != nil {
		t.Fatalf("Search 返回错误: %v", err)
	}
	if len(hits) != 3 {
		t.Fatalf("返回 %d 条，want 3", len(hits))
	}
	wantIDs := []string{"LCL-1", "LCL-3", "LCL-2"}
	for i, id := range wantIDs {
		if hits[i].Statute.ID != id {
			t.Fatalf("第 %d 名 = %s, want %s（全量：%v）", i+1, hits[i].Statute.ID, id, ids(hits))
		}
	}
	if math.Abs(hits[0].Score-1) > 1e-6 {
		t.Fatalf("同向向量相似度 = %v, want 1", hits[0].Score)
	}
	if math.Abs(hits[1].Score-math.Sqrt2/2) > 1e-5 {
		t.Fatalf("45 度相似度 = %v, want ≈0.7071", hits[1].Score)
	}
	if math.Abs(hits[2].Score) > 1e-6 {
		t.Fatalf("正交向量相似度 = %v, want 0", hits[2].Score)
	}
	// 只填 ID 与 Score，其余由调用方回表补全。
	if hits[0].Statute.Article != "" || hits[0].Statute.Text != "" {
		t.Fatalf("Search 不该回表：%+v", hits[0].Statute)
	}
}

func TestSemanticRejectsZeroVector(t *testing.T) {
	_, err := NewSemantic([]port.Embedding{emb("LCL-0", 0, 0, 0)})
	if err == nil {
		t.Fatal("期望报错，实际成功")
	}
	if got := port.CodeOf(err); got != port.CodeInvalidArgument {
		t.Fatalf("错误码 = %s, want %s", got, port.CodeInvalidArgument)
	}
}

func TestSemanticRejectsDimensionMismatch(t *testing.T) {
	_, err := NewSemantic([]port.Embedding{
		emb("LCL-3", 1, 0, 0),
		emb("LCL-4-NINE", 1, 0, 0, 0),
	})
	if err == nil {
		t.Fatal("期望报错，实际成功")
	}
	if got := port.CodeOf(err); got != port.CodeInvalidArgument {
		t.Fatalf("错误码 = %s, want %s", got, port.CodeInvalidArgument)
	}
	if !contains(err.Error(), "LCL-4-NINE") {
		t.Fatalf("错误信息里没有出问题的 owner_id：%q", err.Error())
	}
}

func TestSemanticRejectsEmptyVector(t *testing.T) {
	_, err := NewSemantic([]port.Embedding{emb("LCL-empty")})
	if err == nil {
		t.Fatal("期望报错，实际成功")
	}
	if got := port.CodeOf(err); got != port.CodeInvalidArgument {
		t.Fatalf("错误码 = %s, want %s", got, port.CodeInvalidArgument)
	}
}

func TestSemanticEmptyIndexIsUsable(t *testing.T) {
	s, err := NewSemantic(nil)
	if err != nil {
		t.Fatalf("空索引不该报错: %v", err)
	}
	if s.Len() != 0 {
		t.Fatalf("Len = %d, want 0", s.Len())
	}
	hits, err := s.Search([]float32{1, 0}, 5, 0)
	if err != nil {
		t.Fatalf("空索引 Search 不该报错: %v", err)
	}
	if hits == nil {
		t.Fatal("空索引应返回空切片，不能是 nil")
	}
	if len(hits) != 0 {
		t.Fatalf("空索引返回 %d 条", len(hits))
	}
}

func TestSemanticSearchIsDeterministic(t *testing.T) {
	s, err := NewSemantic([]port.Embedding{
		emb("test-b", 1, 1),
		emb("test-a", 1, 1),
		emb("test-c", 1, 0),
	})
	if err != nil {
		t.Fatalf("NewSemantic 返回错误: %v", err)
	}

	first := ids(mustSearch(t, s, []float32{1, 1}, 3, 0))
	for i := 0; i < 8; i++ {
		got := ids(mustSearch(t, s, []float32{1, 1}, 3, 0))
		for j := range first {
			if got[j] != first[j] {
				t.Fatalf("第 %d 次顺序 = %v，与首次 %v 不一致", i+1, got, first)
			}
		}
	}
	// 两条完全相同的向量同分，按 ID 升序兜底。
	if first[0] != "test-a" || first[1] != "test-b" {
		t.Fatalf("同分兜底顺序 = %v, want [test-a test-b ...]", first)
	}
}

func TestSemanticSearchMinScoreFilters(t *testing.T) {
	s, err := NewSemantic([]port.Embedding{
		emb("LCL-same", 1, 0),
		emb("LCL-orthogonal", 0, 1),
	})
	if err != nil {
		t.Fatalf("NewSemantic 返回错误: %v", err)
	}

	hits, err := s.Search([]float32{1, 0}, 5, 0.9)
	if err != nil {
		t.Fatalf("Search 返回错误: %v", err)
	}
	if len(hits) != 1 || hits[0].Statute.ID != "LCL-same" {
		t.Fatalf("阈值 0.9 下应只剩同向那条，实际 %v", ids(hits))
	}
}

func TestSemanticSearchRejectsZeroQuery(t *testing.T) {
	s, err := NewSemantic([]port.Embedding{emb("LCL-1", 1, 0)})
	if err != nil {
		t.Fatalf("NewSemantic 返回错误: %v", err)
	}

	_, err = s.Search([]float32{0, 0}, 5, 0)
	if err == nil {
		t.Fatal("零向量查询应报错，而不是返回 NaN 分数")
	}
	if got := port.CodeOf(err); got != port.CodeInvalidArgument {
		t.Fatalf("错误码 = %s, want %s", got, port.CodeInvalidArgument)
	}
}

func TestSemanticSearchRejectsQueryDimension(t *testing.T) {
	s, err := NewSemantic([]port.Embedding{emb("LCL-1", 1, 0, 0)})
	if err != nil {
		t.Fatalf("NewSemantic 返回错误: %v", err)
	}

	_, err = s.Search([]float32{1, 0}, 5, 0)
	if err == nil {
		t.Fatal("维度不一致应报错")
	}
	if !contains(err.Error(), "3") || !contains(err.Error(), "2") {
		t.Fatalf("错误信息应写清两边维度：%q", err.Error())
	}
}

func TestSemanticTopKClamping(t *testing.T) {
	s, err := NewSemantic([]port.Embedding{
		emb("LCL-1", 1, 0),
		emb("LCL-2", 0.9, 0.1),
	})
	if err != nil {
		t.Fatalf("NewSemantic 返回错误: %v", err)
	}

	// topK 超过索引条数：截到 Len()，不能 panic 也不能返回空。
	hits, err := s.Search([]float32{1, 0}, 99, 0)
	if err != nil {
		t.Fatalf("Search 返回错误: %v", err)
	}
	if len(hits) != 2 {
		t.Fatalf("topK 超界时返回 %d 条，want 2", len(hits))
	}

	// topK <= 0 取默认 5，同样受 Len 限制。
	hits, err = s.Search([]float32{1, 0}, 0, 0)
	if err != nil {
		t.Fatalf("Search 返回错误: %v", err)
	}
	if len(hits) != 2 {
		t.Fatalf("topK=0 时返回 %d 条，want 2", len(hits))
	}
}

// ids 抽出命中 ID，失败信息里直接可读。
func ids(hits []port.StatuteHit) []string {
	out := make([]string, len(hits))
	for i, h := range hits {
		out[i] = h.Statute.ID
	}
	return out
}

func mustSearch(t *testing.T, s *Semantic, q []float32, topK int, minScore float32) []port.StatuteHit {
	t.Helper()
	hits, err := s.Search(q, topK, minScore)
	if err != nil {
		t.Fatalf("Search 返回错误: %v", err)
	}
	return hits
}

func contains(s, sub string) bool {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}
