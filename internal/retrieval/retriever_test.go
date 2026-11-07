package retrieval

import (
	"context"
	"errors"
	"reflect"
	"testing"

	"laborlaw/internal/port"
)

func TestExtractKeywordsNormalizesSpokenLanguage(t *testing.T) {
	cases := []struct {
		name  string
		input string
		want  []string
	}{
		{"辞退归一到解除", "我被老板辞退了", []string{"解除"}},
		{"N+1 归一到经济补偿", "公司只给 n+1 合理吗", []string{"经济补偿"}},
		{"欠薪归一到工资", "老板拖欠工资三个月", []string{"工资"}},
		{"无领域词", "今天天气怎么样", []string{}},
		{"空输入", "   ", []string{}},
		{"同一归一词不重复", "辞退和开除是一回事吗", []string{"解除"}},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := ExtractKeywords(tc.input)
			if len(tc.want) == 0 {
				if len(got) != 0 {
					t.Errorf("关键词 = %v, want 空", got)
				}
				return
			}
			// 只断言包含关系与顺序无关的部分：位置排序另有专门用例。
			for _, w := range tc.want {
				found := false
				for _, g := range got {
					if g == w {
						found = true
					}
				}
				if !found {
					t.Errorf("关键词 = %v, 缺少 %q", got, w)
				}
			}
		})
	}
}

func TestExtractKeywordsOrdersByFirstMention(t *testing.T) {
	got := ExtractKeywords("试用期被辞退有没有补偿")
	// 用户先说试用期，检索就该先围绕试用期，而不是先围绕解除。
	if len(got) < 2 {
		t.Fatalf("关键词 = %v", got)
	}
	if got[0] != "试用期" {
		t.Errorf("首个关键词 = %q, want 试用期", got[0])
	}
}

func TestExtractKeywordsCapsCount(t *testing.T) {
	got := ExtractKeywords("劳动合同 试用期 工资 加班费 社保 工伤 仲裁 竞业限制 年假")
	if len(got) != maxKeywords {
		t.Errorf("关键词数 = %d, want %d", len(got), maxKeywords)
	}
}

// fakeStore 按关键词返回预设结果，模拟 LIKE 检索的多路召回。
type fakeStore struct {
	byQuery map[string][]port.StatuteHit
	errFor  map[string]error
	queries []string
}

func (f *fakeStore) SearchText(_ context.Context, query, _ string, _ int) ([]port.StatuteHit, error) {
	f.queries = append(f.queries, query)
	if err := f.errFor[query]; err != nil {
		return nil, err
	}
	return f.byQuery[query], nil
}

func hit(id string, score float64) port.StatuteHit {
	return port.StatuteHit{Statute: port.Statute{ID: id, Law: "劳动合同法", Text: "原文" + id}, Score: score}
}

func TestSearchMergesAndRanksByKeywordCoverage(t *testing.T) {
	store := &fakeStore{byQuery: map[string][]port.StatuteHit{
		"解除":   {hit("LCL-39", 0.9), hit("LCL-46", 0.5)},
		"经济补偿": {hit("LCL-46", 0.6), hit("LCL-47", 0.4)},
	}}
	r := New(store)

	hits, err := r.Search(context.Background(), "辞退有没有补偿金", 3)
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if len(hits) == 0 {
		t.Fatal("没有命中")
	}
	// LCL-46 同时命中两个关键词，尽管它的单条分数不是最高，也应排第一。
	if hits[0].Statute.ID != "LCL-46" {
		t.Errorf("首条 = %q, want LCL-46", hits[0].Statute.ID)
	}
	seen := map[string]bool{}
	for _, h := range hits {
		if seen[h.Statute.ID] {
			t.Errorf("结果重复: %q", h.Statute.ID)
		}
		seen[h.Statute.ID] = true
	}
}

func TestSearchSkipsDatabaseWithoutKeywords(t *testing.T) {
	store := &fakeStore{}
	r := New(store)

	hits, err := r.Search(context.Background(), "今天天气怎么样", 3)
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if len(store.queries) != 0 {
		t.Errorf("无领域词时不应访问数据库，实际查询 %v", store.queries)
	}
	if hits == nil {
		t.Error("空结果必须返回空切片而不是 nil，否则 JSON 里是 null")
	}
}

func TestSearchPartialFailureStillReturnsHits(t *testing.T) {
	store := &fakeStore{
		byQuery: map[string][]port.StatuteHit{"经济补偿": {hit("LCL-46", 1)}},
		errFor:  map[string]error{"解除": errors.New("连接断开")},
	}
	r := New(store)

	hits, err := r.Search(context.Background(), "辞退有没有补偿金", 3)
	if err != nil {
		t.Fatalf("部分关键词失败不应让整次检索失败: %v", err)
	}
	if len(hits) != 1 || hits[0].Statute.ID != "LCL-46" {
		t.Errorf("结果 = %+v", hits)
	}
}

func TestSearchAllFailuresReturnError(t *testing.T) {
	store := &fakeStore{errFor: map[string]error{
		"解除":   errors.New("连接断开"),
		"经济补偿": errors.New("连接断开"),
	}}
	r := New(store)

	_, err := r.Search(context.Background(), "辞退有没有补偿金", 3)
	if err == nil {
		t.Error("全部关键词失败时必须返回错误，让编排层发出 tool_call=failed")
	}
}

func TestSearchRespectsTopK(t *testing.T) {
	store := &fakeStore{byQuery: map[string][]port.StatuteHit{
		"解除": {hit("LCL-36", 1), hit("LCL-37", 0.9), hit("LCL-38", 0.8), hit("LCL-39", 0.7)},
	}}
	r := New(store)

	hits, err := r.Search(context.Background(), "辞退", 2)
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if len(hits) != 2 {
		t.Errorf("结果数 = %d, want 2", len(hits))
	}
	if hits[0].Statute.ID != "LCL-36" || hits[1].Statute.ID != "LCL-37" {
		t.Errorf("排序 = %v", hits)
	}
}

func TestSearchDeterministicAcrossRuns(t *testing.T) {
	store := &fakeStore{byQuery: map[string][]port.StatuteHit{
		"解除": {hit("LCL-39", 0.5), hit("LCL-38", 0.5), hit("LCL-40", 0.5)},
	}}
	r := New(store)

	first, err := r.Search(context.Background(), "辞退", 5)
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	for i := 0; i < 8; i++ {
		got, err := r.Search(context.Background(), "辞退", 5)
		if err != nil {
			t.Fatalf("Search: %v", err)
		}
		if !reflect.DeepEqual(first, got) {
			t.Fatalf("同分结果顺序不稳定:\n第一次 %+v\n第 %d 次 %+v", first, i+2, got)
		}
	}
}

func TestSearchNilStore(t *testing.T) {
	r := New(nil)
	hits, err := r.Search(context.Background(), "辞退", 3)
	if err != nil || hits == nil {
		t.Errorf("nil store 应返回空切片与 nil 错误，实际 %v / %v", hits, err)
	}
}
