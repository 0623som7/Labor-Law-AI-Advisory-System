package retrieval

import (
	"context"
	"errors"
	"strings"
	"testing"

	"laborlaw/internal/port"
)

// fakeReranker 按 ID 给分，便于断言"顺序变了"而不是"顺序碰巧一样"。
type fakeReranker struct {
	scores map[string]float64
	err    error
	calls  int
	got    []string
	query  string
}

func (f *fakeReranker) Rerank(_ context.Context, query string, docs []string) ([]float64, error) {
	f.calls++
	f.query = query
	f.got = docs
	if f.err != nil {
		return nil, f.err
	}
	out := make([]float64, len(docs))
	for i, doc := range docs {
		for id, score := range f.scores {
			// 文档是 StatuteDocument 拼出来的，条号里带着 ID，按子串匹配最省事。
			if strings.Contains(doc, id) {
				out[i] = score
			}
		}
	}
	return out, nil
}

// rerankHybrid 造一个"关键词命中四条、向量空"的融合检索，带指定 reranker。
//
// 四条的分数依次递减，所以不开重排时顺序就是 kw-1..kw-4。
func rerankHybrid(t *testing.T, r Reranker) *Hybrid {
	t.Helper()
	hits := []port.StatuteHit{
		statuteHit("LCL-1", "第一条 通则", 4),
		statuteHit("LCL-2", "第二条 通则", 3),
		statuteHit("LCL-3", "第三条 通则", 2),
		statuteHit("LCL-4", "第四条 例外", 1),
	}
	h := newTestHybrid(t, hits, mustSemantic(t, map[string][]float32{"LCL-1": {1, 0}}), &fakeEmbedder{}, nil)
	if r != nil {
		h.reranker = r
	}
	return h
}

func TestHybridRerankReorders(t *testing.T) {
	// 把原来排最后的 LCL-4 顶到第一：这正是"例外条款排在通则后面"的场景。
	r := &fakeReranker{scores: map[string]float64{
		"LCL-1": 0.11, "LCL-2": 0.22, "LCL-3": 0.33, "LCL-4": 0.99,
	}}
	h := rerankHybrid(t, r)

	got, err := h.Search(context.Background(), "加班费", 4)
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if r.calls != 1 {
		t.Fatalf("重排调用次数 = %d, want 1", r.calls)
	}
	if r.query != "加班费" {
		t.Errorf("重排 query = %q", r.query)
	}
	ids := idsOf(got)
	want := []string{"LCL-4", "LCL-3", "LCL-2", "LCL-1"}
	for i := range want {
		if ids[i] != want[i] {
			t.Fatalf("顺序 = %v, want %v", ids, want)
		}
	}
}

func TestHybridRerankErrorKeepsOrder(t *testing.T) {
	base := rerankHybrid(t, nil)
	want, err := base.Search(context.Background(), "加班费", 4)
	if err != nil {
		t.Fatalf("基线 Search: %v", err)
	}

	h := rerankHybrid(t, &fakeReranker{err: errors.New("上游欠费")})
	got, err := h.Search(context.Background(), "加班费", 4)
	if err != nil {
		t.Fatalf("重排失败不该让检索返回错误: %v", err)
	}
	gotIDs, wantIDs := idsOf(got), idsOf(want)
	for i := range wantIDs {
		if gotIDs[i] != wantIDs[i] {
			t.Fatalf("顺序 = %v, want %v（应与不开重排完全一致）", gotIDs, wantIDs)
		}
	}
}

func TestHybridRerankSkippedWhenFewCandidates(t *testing.T) {
	// 只有 3 条候选（< minRerankCandidates）时不该白发一次请求。
	hits := []port.StatuteHit{
		statuteHit("LCL-1", "第一条", 3),
		statuteHit("LCL-2", "第二条", 2),
		statuteHit("LCL-3", "第三条", 1),
	}
	h := newTestHybrid(t, hits, mustSemantic(t, map[string][]float32{"LCL-1": {1, 0}}), &fakeEmbedder{}, nil)
	r := &fakeReranker{scores: map[string]float64{"LCL-1": 0.1}}
	h.reranker = r

	if _, err := h.Search(context.Background(), "加班费", 3); err != nil {
		t.Fatalf("Search: %v", err)
	}
	if r.calls != 0 {
		t.Errorf("重排调用次数 = %d, want 0", r.calls)
	}
}

func TestHybridRerankScoreIsRerankScore(t *testing.T) {
	r := &fakeReranker{scores: map[string]float64{
		"LCL-1": 0.11, "LCL-2": 0.22, "LCL-3": 0.33, "LCL-4": 0.99,
	}}
	h := rerankHybrid(t, r)

	got, err := h.Search(context.Background(), "加班费", 2)
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("条数 = %d, want 2", len(got))
	}
	if got[0].Statute.ID != "LCL-4" || got[0].Score != 0.99 {
		t.Errorf("第一条 = %s/%v，期望 LCL-4/0.99", got[0].Statute.ID, got[0].Score)
	}
	if got[1].Score != 0.33 {
		t.Errorf("第二条分数 = %v，期望 0.33", got[1].Score)
	}
}

func TestHybridRerankSearchInFiltersFirst(t *testing.T) {
	// 带 law 的路径不能走向量，但重排要生效。
	hits := []port.StatuteHit{
		statuteHit("LCL-1", "第一条", 4),
		statuteHit("LCL-2", "第二条", 3),
		statuteHit("LCL-3", "第三条", 2),
		statuteHit("LCL-4", "第四条 例外", 1),
	}
	h := newTestHybrid(t, hits, mustSemantic(t, map[string][]float32{"LCL-1": {1, 0}}), &fakeEmbedder{}, nil)
	r := &fakeReranker{scores: map[string]float64{
		"LCL-1": 0.1, "LCL-2": 0.2, "LCL-3": 0.3, "LCL-4": 0.95,
	}}
	h.reranker = r

	got, err := h.SearchIn(context.Background(), "加班费", "中华人民共和国劳动合同法", 4)
	if err != nil {
		t.Fatalf("SearchIn: %v", err)
	}
	if r.calls != 1 {
		t.Fatalf("重排调用次数 = %d, want 1", r.calls)
	}
	if ids := idsOf(got); ids[0] != "LCL-4" {
		t.Errorf("首条 = %s，期望 LCL-4", ids[0])
	}
}
