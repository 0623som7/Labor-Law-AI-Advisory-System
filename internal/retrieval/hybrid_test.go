package retrieval

import (
	"context"
	"io"
	"log/slog"
	"math"
	"testing"

	"laborlaw/internal/port"
)

// fakeKeywordStore 按检索词返回预置命中，避免测试依赖真实 SQL 与词表。
type fakeKeywordStore struct {
	hits map[string][]port.StatuteHit
	err  error
}

func (f fakeKeywordStore) SearchText(_ context.Context, query, _ string, topK int) ([]port.StatuteHit, error) {
	if f.err != nil {
		return nil, f.err
	}
	hits := f.hits[query]
	if len(hits) > topK {
		hits = hits[:topK]
	}
	out := make([]port.StatuteHit, len(hits))
	copy(out, hits)
	return out, nil
}

type fakeEmbedder struct {
	vec    []float32
	err    error
	empty  bool
	calls  int
	models []string
}

func (f *fakeEmbedder) Embed(_ context.Context, model string, _ []string) ([][]float32, error) {
	f.calls++
	f.models = append(f.models, model)
	if f.err != nil {
		return nil, f.err
	}
	if f.empty {
		return [][]float32{}, nil
	}
	return [][]float32{f.vec}, nil
}

type fakeGetter struct {
	byID map[string]port.Statute
	err  error
	seen []string
}

func (f *fakeGetter) Get(_ context.Context, statuteID string) (*port.Statute, error) {
	f.seen = append(f.seen, statuteID)
	if f.err != nil {
		return nil, f.err
	}
	st, ok := f.byID[statuteID]
	if !ok {
		return nil, port.New(port.CodeNotFound, "法条不存在")
	}
	return &st, nil
}

func quietLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// statute 构造一条带正文的命中，正文非空是为了让"补全"逻辑跳过它。
func statuteHit(id, text string, score float64) port.StatuteHit {
	return port.StatuteHit{
		Statute: port.Statute{ID: id, Law: "中华人民共和国劳动合同法", Article: "第" + id + "条", Text: text},
		Score:   score,
	}
}

func embeddingsOf(vectors map[string][]float32) []port.Embedding {
	items := make([]port.Embedding, 0, len(vectors))
	for id, vec := range vectors {
		items = append(items, port.Embedding{OwnerType: "statute", OwnerID: id, Model: "m", Vector: vec})
	}
	return items
}

// newTestHybrid 用单检索词的输入构造融合检索：关键词这路只命中 "加班" 这一个词，
// 于是排序完全由假数据决定，不依赖词表里其他别名的存在与否。
func newTestHybrid(t *testing.T, keywordHits []port.StatuteHit, sem *Semantic, emb Embedder, getter StatuteGetter) *Hybrid {
	t.Helper()
	store := fakeKeywordStore{hits: map[string][]port.StatuteHit{"加班": keywordHits}}
	return NewHybrid(New(store), sem, emb, getter, "m", quietLogger())
}

func idsOf(hits []port.StatuteHit) []string {
	out := make([]string, len(hits))
	for i, h := range hits {
		out[i] = h.Statute.ID
	}
	return out
}

func mustSemantic(t *testing.T, vectors map[string][]float32) *Semantic {
	t.Helper()
	sem, err := NewSemantic(embeddingsOf(vectors))
	if err != nil {
		t.Fatalf("建索引失败：%v", err)
	}
	return sem
}

func TestHybridFusesBothPaths(t *testing.T) {
	keyword := []port.StatuteHit{statuteHit("LCL-31", "用人单位应当严格执行劳动定额标准", 3), statuteHit("LCL-85", "未依法支付劳动报酬", 2)}
	sem := mustSemantic(t, map[string][]float32{
		"LCL-31": {1, 0},
		"LL-44":  {0.6, 0.8},
	})
	emb := &fakeEmbedder{vec: []float32{1, 0}}
	getter := &fakeGetter{byID: map[string]port.Statute{"LL-44": {ID: "LL-44", Text: "延长工作时间应支付加班工资"}}}
	h := newTestHybrid(t, keyword, sem, emb, getter)

	hits, err := h.Search(context.Background(), "加班费", 5)
	if err != nil {
		t.Fatalf("Search 返回错误：%v", err)
	}
	// LCL-31 两路都在（1/61 + 1/61），排第一且只出现一次。
	// LCL-85 与 LL-44 都是 1/62（各自那一路的第 2 名），同分按 ID 升序，
	// LCL-85 在前——同分必须有确定的末级比较，否则两次提问的顺序会变。
	got := idsOf(hits)
	if len(got) != 3 || got[0] != "LCL-31" || got[1] != "LCL-85" || got[2] != "LL-44" {
		t.Fatalf("融合顺序不符，实际 %v", got)
	}
	if hits[2].Statute.Text == "" {
		t.Fatalf("向量独有条目应回表补全正文，实际 %+v", hits[2])
	}
	// 关键词那路已带正文，不该再回表。
	if len(getter.seen) != 1 || getter.seen[0] != "LL-44" {
		t.Fatalf("只应回表补向量独有条目，实际回表 %v", getter.seen)
	}
}

func TestHybridKeywordOnly(t *testing.T) {
	keyword := []port.StatuteHit{statuteHit("LCL-31", "正文甲", 3), statuteHit("LCL-85", "正文乙", 2)}
	emb := &fakeEmbedder{vec: []float32{1, 0}}
	getter := &fakeGetter{}
	h := newTestHybrid(t, keyword, nil, emb, getter)

	hits, err := h.Search(context.Background(), "加班费", 5)
	if err != nil {
		t.Fatalf("Search 返回错误：%v", err)
	}
	got := idsOf(hits)
	if len(got) != 2 || got[0] != "LCL-31" || got[1] != "LCL-85" {
		t.Fatalf("索引为空时应保持关键词顺序，实际 %v", got)
	}
	// 索引为空时不该白扔一次向量化调用。
	if emb.calls != 0 {
		t.Fatalf("索引为空时不应调用向量化，实际调用 %d 次", emb.calls)
	}
}

func TestHybridVectorOnly(t *testing.T) {
	sem := mustSemantic(t, map[string][]float32{"LL-44": {1, 0}})
	emb := &fakeEmbedder{vec: []float32{1, 0}}
	getter := &fakeGetter{byID: map[string]port.Statute{"LL-44": {ID: "LL-44", Text: "延长工作时间应支付加班工资"}}}
	h := newTestHybrid(t, nil, sem, emb, getter)

	hits, err := h.Search(context.Background(), "加班费", 5)
	if err != nil {
		t.Fatalf("Search 返回错误：%v", err)
	}
	if len(hits) != 1 || hits[0].Statute.ID != "LL-44" {
		t.Fatalf("期望只有向量命中的 LL-44，实际 %v", idsOf(hits))
	}
	if hits[0].Statute.Text != "延长工作时间应支付加班工资" {
		t.Fatalf("正文应来自回表，实际 %q", hits[0].Statute.Text)
	}
}

func TestHybridDegradesOnEmbedError(t *testing.T) {
	keyword := []port.StatuteHit{statuteHit("LCL-31", "正文甲", 3)}
	sem := mustSemantic(t, map[string][]float32{"LCL-31": {1, 0}})
	emb := &fakeEmbedder{err: port.New(port.CodeUpstreamTimeout, "上游超时")}
	h := newTestHybrid(t, keyword, sem, emb, &fakeGetter{})

	hits, err := h.Search(context.Background(), "加班费", 5)
	if err != nil {
		t.Fatalf("语义失败不应把错误抛给编排层：%v", err)
	}
	if got := idsOf(hits); len(got) != 1 || got[0] != "LCL-31" {
		t.Fatalf("降级后应只剩关键词结果，实际 %v", got)
	}
}

func TestHybridDegradesOnEmptyEmbedding(t *testing.T) {
	keyword := []port.StatuteHit{statuteHit("LCL-31", "正文甲", 3)}
	sem := mustSemantic(t, map[string][]float32{"LCL-31": {1, 0}})
	emb := &fakeEmbedder{empty: true}
	h := newTestHybrid(t, keyword, sem, emb, &fakeGetter{})

	hits, err := h.Search(context.Background(), "加班费", 5)
	if err != nil {
		t.Fatalf("空向量不应报错：%v", err)
	}
	if got := idsOf(hits); len(got) != 1 || got[0] != "LCL-31" {
		t.Fatalf("降级后应只剩关键词结果，实际 %v", got)
	}
}

func TestHybridDegradesOnDimensionMismatch(t *testing.T) {
	keyword := []port.StatuteHit{statuteHit("LCL-31", "正文甲", 3)}
	sem := mustSemantic(t, map[string][]float32{"LCL-31": {1, 0, 0}})
	emb := &fakeEmbedder{vec: []float32{1, 0}}
	h := newTestHybrid(t, keyword, sem, emb, &fakeGetter{})

	hits, err := h.Search(context.Background(), "加班费", 5)
	if err != nil {
		t.Fatalf("维度不一致应降级而不是报错：%v", err)
	}
	if got := idsOf(hits); len(got) != 1 || got[0] != "LCL-31" {
		t.Fatalf("降级后应只剩关键词结果，实际 %v", got)
	}
}

func TestHybridDropsMissingStatute(t *testing.T) {
	sem := mustSemantic(t, map[string][]float32{
		"GONE-1": {1, 0},
		"LL-44":  {0.9, 0.1},
	})
	emb := &fakeEmbedder{vec: []float32{1, 0}}
	getter := &fakeGetter{byID: map[string]port.Statute{"LL-44": {ID: "LL-44", Text: "延长工作时间应支付加班工资"}}}
	h := newTestHybrid(t, nil, sem, emb, getter)

	hits, err := h.Search(context.Background(), "加班费", 5)
	if err != nil {
		t.Fatalf("回表 404 不应报错：%v", err)
	}
	if got := idsOf(hits); len(got) != 1 || got[0] != "LL-44" {
		t.Fatalf("种子换过导致的缺失条目应被丢弃，实际 %v", got)
	}
}

func TestHybridDropsEmptyText(t *testing.T) {
	sem := mustSemantic(t, map[string][]float32{
		"EMPTY-1": {1, 0},
		"LL-44":   {0.9, 0.1},
	})
	emb := &fakeEmbedder{vec: []float32{1, 0}}
	getter := &fakeGetter{byID: map[string]port.Statute{
		"LL-44":   {ID: "LL-44", Text: "延长工作时间应支付加班工资"},
		"EMPTY-1": {ID: "EMPTY-1", Text: "   "},
	}}
	h := newTestHybrid(t, nil, sem, emb, getter)

	hits, err := h.Search(context.Background(), "加班费", 5)
	if err != nil {
		t.Fatalf("Search 返回错误：%v", err)
	}
	if got := idsOf(hits); len(got) != 1 || got[0] != "LL-44" {
		t.Fatalf("正文为空的条目应被丢弃，实际 %v", got)
	}
}

func TestHybridTopKZeroDefaultsToFive(t *testing.T) {
	vectors := map[string][]float32{}
	for _, id := range []string{"A-1", "A-2", "A-3", "A-4", "A-5", "A-6"} {
		vectors[id] = []float32{1, 0}
	}
	sem := mustSemantic(t, vectors)
	emb := &fakeEmbedder{vec: []float32{1, 0}}
	byID := map[string]port.Statute{}
	for id := range vectors {
		byID[id] = port.Statute{ID: id, Text: "正文"}
	}
	h := newTestHybrid(t, nil, sem, emb, &fakeGetter{byID: byID})

	hits, err := h.Search(context.Background(), "加班费", 0)
	if err != nil {
		t.Fatalf("topK=0 不应报错：%v", err)
	}
	if len(hits) != 5 {
		t.Fatalf("topK=0 应按 5 处理，实际 %d 条", len(hits))
	}
}

func TestLoadSemanticIndexEmpty(t *testing.T) {
	sem, err := LoadSemanticIndex(context.Background(), emptyLoader{}, "statute", "m")
	if err != nil {
		t.Fatalf("空向量库不应报错：%v", err)
	}
	if sem != nil {
		t.Fatalf("空向量库应返回 nil 索引，实际 %+v", sem)
	}
}

type emptyLoader struct{}

func (emptyLoader) LoadEmbeddings(context.Context, string, string) ([]port.Embedding, error) {
	return []port.Embedding{}, nil
}

func TestHybridLen(t *testing.T) {
	sem := mustSemantic(t, map[string][]float32{"A-1": {1, 0}, "A-2": {0, 1}})
	h := newTestHybrid(t, nil, sem, &fakeEmbedder{}, &fakeGetter{})
	if h.Len() != 2 {
		t.Fatalf("Len 期望 2，实际 %d", h.Len())
	}
	var nilHybrid *Hybrid
	if nilHybrid.Len() != 0 {
		t.Fatalf("nil Hybrid 的 Len 应为 0")
	}
}

// 对外只有一个 score 字段：排序用的是 RRF 分，展示也必须是它，
// 否则调用方会看到"余弦 0.63 排在关键词 7.5 前面"。
func TestHybridScoreIsFusedScore(t *testing.T) {
	keyword := []port.StatuteHit{
		statuteHit("LCL-31", "用人单位应当严格执行劳动定额标准", 3),
		statuteHit("LCL-85", "未依法支付劳动报酬", 2),
	}
	sem := mustSemantic(t, map[string][]float32{
		"LCL-31": {1, 0},
		"LL-44":  {0.6, 0.8},
	})
	emb := &fakeEmbedder{vec: []float32{1, 0}}
	getter := &fakeGetter{byID: map[string]port.Statute{"LL-44": {ID: "LL-44", Text: "延长工作时间应支付加班工资"}}}
	h := newTestHybrid(t, keyword, sem, emb, getter)

	hits, err := h.Search(context.Background(), "加班费", 5)
	if err != nil {
		t.Fatalf("Search 返回错误：%v", err)
	}
	if len(hits) != 3 {
		t.Fatalf("期望 3 条，实际 %d", len(hits))
	}

	// LCL-31 两路都是第 1 名：1/61 + 1/61；另两条各是某一路的第 2 名：1/62。
	want := map[string]float64{
		"LCL-31": 1.0/61 + 1.0/61,
		"LCL-85": 1.0 / 62,
		"LL-44":  1.0 / 62,
	}
	for _, hit := range hits {
		expect, ok := want[hit.Statute.ID]
		if !ok {
			t.Fatalf("出现意料外的条目 %s", hit.Statute.ID)
		}
		if math.Abs(hit.Score-expect) > 1e-12 {
			t.Fatalf("%s 的 Score = %v，期望融合分 %v", hit.Statute.ID, hit.Score, expect)
		}
	}

	// 融合分必须严格降序，且不能残留任何原始分（关键词 3/2、余弦 1/0.8）。
	if !(hits[0].Score > hits[1].Score && hits[1].Score >= hits[2].Score) {
		t.Fatalf("Score 未严格降序：%v %v %v", hits[0].Score, hits[1].Score, hits[2].Score)
	}
	for _, hit := range hits {
		if hit.Score > 1 {
			t.Fatalf("%s 的 Score = %v，看起来是原始分没被融合分覆盖", hit.Statute.ID, hit.Score)
		}
	}
}
