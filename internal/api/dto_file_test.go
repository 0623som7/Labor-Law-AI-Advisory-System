package api

import (
	"encoding/json"
	"sort"
	"strings"
	"testing"

	"laborlaw/internal/port"
)

// 本文件守的是"解析结果接口的 JSON 字段名与 contract/openapi.yaml 一致"。
// 期望的字段集合是从 yaml 的 components.schemas.*.properties 逐字抄下来的：
// 没有 yaml 依赖（不引新包），所以这里只能人工对齐，改契约时两边一起改。

func sampleParsedDoc() *port.ParsedDoc {
	return &port.ParsedDoc{
		FileID:     "01K9ZZZZZZZZZZZZZZZZZZZZZZ",
		PageCount:  1,
		OCRUsed:    true,
		Confidence: 0.93,
		Pages: []port.ParsedPage{
			{
				Index: 0, Width: 595.32, Height: 841.92,
				Blocks: []port.TextBlock{
					{
						ID: "01K9ZZZZZZZZZZZZZZZZZZZZZZ-0-0", Page: 0,
						BBox: [4]float64{1, 2, 3, 4}, Text: "试用期三个月",
						CharStart: 0, CharEnd: 6,
					},
				},
			},
		},
	}
}

func mustMarshal(t *testing.T, v any) string {
	t.Helper()
	raw, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("json.Marshal 失败: %v", err)
	}
	return string(raw)
}

func TestParsedDocDTO(t *testing.T) {
	t.Run("bbox 序列化成四元数组", func(t *testing.T) {
		raw := mustMarshal(t, toParsedDocDTO(sampleParsedDoc()))
		if !strings.Contains(raw, `"bbox":[1,2,3,4]`) {
			t.Fatalf(`响应体不含 "bbox":[1,2,3,4]: %s`, raw)
		}
	})

	t.Run("不泄漏 port 的内部字段名", func(t *testing.T) {
		raw := mustMarshal(t, toParsedDocDTO(sampleParsedDoc()))
		for _, leaked := range []string{"BBox", "FileID", "CharStart", "CharEnd", "PageCount", "OCRUsed"} {
			if strings.Contains(raw, leaked) {
				t.Fatalf("响应体泄漏了内部字段名 %q: %s", leaked, raw)
			}
		}
	})

	t.Run("空 blocks 序列化成空数组", func(t *testing.T) {
		doc := &port.ParsedDoc{
			FileID:    "01K9ZZZZZZZZZZZZZZZZZZZZZZ",
			PageCount: 1,
			Pages:     []port.ParsedPage{{Index: 0, Width: 595.32, Height: 841.92}},
		}
		raw := mustMarshal(t, toParsedDocDTO(doc))
		if !strings.Contains(raw, `"blocks":[]`) {
			t.Fatalf(`空 blocks 应当序列化成 "blocks":[]: %s`, raw)
		}
		if strings.Contains(raw, "blocks\":null") {
			t.Fatalf("空 blocks 序列化成了 null: %s", raw)
		}
	})

	t.Run("nil doc 仍然给出 pages 空数组", func(t *testing.T) {
		raw := mustMarshal(t, toParsedDocDTO(nil))
		if !strings.Contains(raw, `"pages":[]`) {
			t.Fatalf(`nil doc 的 pages 应当是 []: %s`, raw)
		}
	})

	t.Run("字段名与 openapi 逐字一致", func(t *testing.T) {
		raw := mustMarshal(t, toParsedDocDTO(sampleParsedDoc()))

		var doc map[string]json.RawMessage
		if err := json.Unmarshal([]byte(raw), &doc); err != nil {
			t.Fatalf("Unmarshal 失败: %v", err)
		}
		wantDocKeys := []string{"confidence", "file_id", "ocr_used", "page_count", "pages"}
		if got := sortedKeys(doc); !equalStrings(got, wantDocKeys) {
			t.Fatalf("ParsedDoc 字段 = %v, want %v", got, wantDocKeys)
		}

		var pages []map[string]json.RawMessage
		if err := json.Unmarshal(doc["pages"], &pages); err != nil {
			t.Fatalf("解析 pages 失败: %v", err)
		}
		wantPageKeys := []string{"blocks", "height", "index", "width"}
		if got := sortedKeys(pages[0]); !equalStrings(got, wantPageKeys) {
			t.Fatalf("ParsedPage 字段 = %v, want %v", got, wantPageKeys)
		}

		var blocks []map[string]json.RawMessage
		if err := json.Unmarshal(pages[0]["blocks"], &blocks); err != nil {
			t.Fatalf("解析 blocks 失败: %v", err)
		}
		wantBlockKeys := []string{"bbox", "char_end", "char_start", "id", "page", "text"}
		if got := sortedKeys(blocks[0]); !equalStrings(got, wantBlockKeys) {
			t.Fatalf("TextBlock 字段 = %v, want %v", got, wantBlockKeys)
		}
	})
}

func sortedKeys(m map[string]json.RawMessage) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
