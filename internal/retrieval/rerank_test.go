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

// lawHit 造一条指定法律的命中，用于 diversify 的分桶断言。
func lawHit(id, law string) port.StatuteHit {
	return port.StatuteHit{Statute: port.Statute{ID: id, Law: law}, Score: 1}
}

func TestDiversifyCapsSameLaw(t *testing.T) {
	hits := []port.StatuteHit{lawHit("A-1", "法甲"), lawHit("A-2", "法甲"), lawHit("A-3", "法甲")}
	got := diversify(hits, 3, 2)
	if len(got) != len(hits) {
		t.Fatalf("长度 = %d，期望 %d", len(got), len(hits))
	}
	// 只有一部法时回填逃不掉：topK 里同法占三席是预期结果，这里只锁顺序、长度与集合。
	if ids := idsOf(got); ids[0] != "A-1" || ids[1] != "A-2" || ids[2] != "A-3" {
		t.Errorf("顺序 = %v，期望回填后仍是 A-1 A-2 A-3", ids)
	}
	seen := map[string]bool{}
	for _, h := range got {
		seen[h.Statute.ID] = true
	}
	if len(seen) != 3 {
		t.Errorf("集合变了，只剩 %d 条", len(seen))
	}
}

func TestDiversifyKeepsMixed(t *testing.T) {
	hits := []port.StatuteHit{lawHit("A-1", "法甲"), lawHit("B-1", "法乙"), lawHit("C-1", "法丙")}
	got := diversify(hits, 3, 2)
	ids := idsOf(got)
	if len(ids) != 3 || ids[0] != "A-1" || ids[1] != "B-1" || ids[2] != "C-1" {
		t.Errorf("顺序 = %v，期望与输入一致 [A-1 B-1 C-1]", ids)
	}
}

func TestDiversifyBackfills(t *testing.T) {
	hits := []port.StatuteHit{
		lawHit("A-1", "法甲"), lawHit("A-2", "法甲"), lawHit("A-3", "法甲"),
		lawHit("B-1", "法乙"), lawHit("B-2", "法乙"), lawHit("B-3", "法乙"),
	}
	got := diversify(hits, 3, 1)
	if len(got) != 6 {
		t.Fatalf("长度 = %d，期望 6（回填后与输入等长）", len(got))
	}
	ids := idsOf(got)
	// 前两席必须跨法：这是限流要的效果。
	if ids[0] != "A-1" || ids[1] != "B-1" {
		t.Errorf("前两席 = %s %s，期望 A-1 B-1", ids[0], ids[1])
	}
	seen := map[string]bool{}
	for _, id := range ids {
		seen[id] = true
	}
	if len(seen) != 6 {
		t.Errorf("集合变了，只剩 %d 条：%v", len(seen), ids)
	}
}

func TestDiversifyNoCapWhenSparse(t *testing.T) {
	hits := []port.StatuteHit{lawHit("A-1", "法甲"), lawHit("A-2", "法甲")}
	got := diversify(hits, 5, 2)
	if len(got) != 2 {
		t.Fatalf("长度 = %d，期望 2", len(got))
	}
	if ids := idsOf(got); ids[0] != "A-1" || ids[1] != "A-2" {
		t.Errorf("顺序 = %v，期望原样返回", ids)
	}
}
