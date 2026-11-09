package retrieval

import (
	"context"
	"reflect"
	"sync/atomic"
	"testing"

	"laborlaw/internal/port"
)

// countingEmbedder 数一数"到底打了几次公网"。
type countingEmbedder struct {
	calls int32
	vec   []float32
}

func (c *countingEmbedder) Embed(_ context.Context, _ string, texts []string) ([][]float32, error) {
	atomic.AddInt32(&c.calls, 1)
	out := make([][]float32, len(texts))
	for i := range texts {
		out[i] = append([]float32(nil), c.vec...)
	}
	return out, nil
}

func (c *countingEmbedder) count() int { return int(atomic.LoadInt32(&c.calls)) }

// mapCache 是一个只活在测试里的内存缓存。
type mapCache struct {
	data  map[string][]float32
	gets  int
	sets  int
	panic bool
}

func newMapCache() *mapCache { return &mapCache{data: map[string][]float32{}} }

func (m *mapCache) GetVector(_ context.Context, key string) ([]float32, bool) {
	m.gets++
	if m.panic {
		panic("cache exploded")
	}
	vec, ok := m.data[key]
	return vec, ok
}

func (m *mapCache) SetVector(_ context.Context, key string, vec []float32) {
	m.sets++
	m.data[key] = vec
}

// TestHybridUsesCachedQueryVector 是这一轮的核心：同一个问题第二次不再走公网。
func TestHybridUsesCachedQueryVector(t *testing.T) {
	cache := newMapCache()
	emb := &countingEmbedder{vec: []float32{0, 1, 0, 0, 0, 0, 0, 0}}
	h := newCacheTestHybrid(t, emb, cache)

	first, err := h.Search(context.Background(), "加班费", 3)
	if err != nil {
		t.Fatalf("第一次检索失败: %v", err)
	}
	if emb.count() != 1 {
		t.Fatalf("第一次应当打一次公网，实际 %d", emb.count())
	}
	second, err := h.Search(context.Background(), "加班费", 3)
	if err != nil {
		t.Fatalf("第二次检索失败: %v", err)
	}
	if emb.count() != 1 {
		t.Fatalf("第二次应当命中缓存、不再打公网，实际调用 %d 次", emb.count())
	}
	if !reflect.DeepEqual(first, second) {
		t.Fatalf("命中缓存与首次结果不一致:\n%v\n%v", first, second)
	}
	if cache.sets != 1 {
		t.Fatalf("只应写入一次缓存，实际 %d", cache.sets)
	}
}

func TestHybridCachesDifferentInputsSeparately(t *testing.T) {
	cache := newMapCache()
	emb := &countingEmbedder{vec: []float32{0, 1, 0, 0, 0, 0, 0, 0}}
	h := newCacheTestHybrid(t, emb, cache)

	if _, err := h.Search(context.Background(), "加班费", 3); err != nil {
		t.Fatalf("检索失败: %v", err)
	}
	if _, err := h.Search(context.Background(), "经济补偿", 3); err != nil {
		t.Fatalf("检索失败: %v", err)
	}
	if emb.count() != 2 {
		t.Fatalf("两个不同问题应各打一次公网，实际 %d", emb.count())
	}
	if len(cache.data) != 2 {
		t.Fatalf("缓存里应有 2 个键，实际 %d", len(cache.data))
	}
}

// TestHybridIgnoresCachedVectorWithWrongDims 钉住维度校验。
//
// 旧模型留下的 1024 维向量喂给 1536 维索引，会在 dot 里越界 panic——
// 缓存读出来的东西必须先验长度再用。
func TestHybridIgnoresCachedVectorWithWrongDims(t *testing.T) {
	cache := newMapCache()
	emb := &countingEmbedder{vec: []float32{0, 1, 0, 0, 0, 0, 0, 0}}
	h := newCacheTestHybrid(t, emb, cache)

	cache.data[QueryVectorKey(h.model, "加班费")] = []float32{1, 0, 0}

	if _, err := h.Search(context.Background(), "加班费", 3); err != nil {
		t.Fatalf("维度不符时应重新向量化，而不是报错: %v", err)
	}
	if emb.count() != 1 {
		t.Fatalf("维度不符必须当 miss，实际公网调用 %d 次", emb.count())
	}
	if got := len(cache.data[QueryVectorKey(h.model, "加班费")]); got != 8 {
		t.Fatalf("缓存应被新向量覆盖成 8 维，实际 %d 维", got)
	}
}

// TestHybridSurvivesCacheFailure 钉住"缓存挂了不许影响检索"。
func TestHybridSurvivesCacheFailure(t *testing.T) {
	emb := &countingEmbedder{vec: []float32{0, 1, 0, 0, 0, 0, 0, 0}}
	h := newCacheTestHybrid(t, emb, &failingCache{})

	hits, err := h.Search(context.Background(), "加班费", 3)
	if err != nil {
		t.Fatalf("缓存失败不该让检索失败: %v", err)
	}
	if len(hits) == 0 {
		t.Fatal("缓存失败时仍应返回关键词+语义结果")
	}
	if emb.count() != 1 {
		t.Fatalf("缓存未命中就要打一次公网，实际 %d", emb.count())
	}
}

// failingCache 两个方法都不能正常服务：Get 一律 miss，Set 什么也不做。
type failingCache struct{}

func (failingCache) GetVector(context.Context, string) ([]float32, bool) {
	return nil, false
}
func (failingCache) SetVector(context.Context, string, []float32) {}

func TestQueryVectorKeyChangesWithModel(t *testing.T) {
	a := QueryVectorKey("text-embedding-v3", "加班费")
	b := QueryVectorKey("text-embedding-v4", "加班费")
	if a == b {
		t.Fatalf("换模型必须换键，实际相同: %s", a)
	}
	if a != QueryVectorKey("text-embedding-v3", "  加班费  ") {
		t.Fatal("前后空白应当被 TrimSpace 掉")
	}
	if a != QueryVectorKey("text-embedding-v3", "加班费 ") {
		t.Fatal("尾部空白应当被 TrimSpace 掉")
	}
	if a == QueryVectorKey("text-embedding-v3", "加班费？") {
		t.Fatal("标点不同就是不同的问题，缓存不该归一化")
	}
	if len(a) <= len("retrieval:qvec:") || a[:len("retrieval:qvec:")] != "retrieval:qvec:" {
		t.Fatalf("键缺少前缀: %s", a)
	}
}

// newCacheTestHybrid 组一个 8 维索引 + 带缓存的 Hybrid。
// 假 store 只对"加班"返回命中，排序完全由假数据决定。
func newCacheTestHybrid(t *testing.T, emb Embedder, cache VectorCache) *Hybrid {
	t.Helper()
	store := &fakeKeywordStore{hits: map[string][]port.StatuteHit{
		"加班": {statuteHit("LCL-31", "用人单位应当严格执行劳动定额标准。", 3.0)},
	}}
	index, err := NewSemantic(embeddingsOf(map[string][]float32{
		"LCL-31": {1, 0, 0, 0, 0, 0, 0, 0},
		"LCL-85": {0, 1, 0, 0, 0, 0, 0, 0},
	}))
	if err != nil {
		t.Fatalf("建索引失败: %v", err)
	}
	getter := &fakeGetter{byID: map[string]port.Statute{
		"LCL-85": {ID: "LCL-85", Text: "用人单位有下列情形之一的，由劳动行政部门责令限期支付。"},
	}}
	return NewHybrid(New(store), index, emb, getter, "text-embedding-v3", quietLogger(), WithVectorCache(cache))
}
