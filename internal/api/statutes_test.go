package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/url"
	"testing"
	"time"

	"laborlaw/internal/port"
)

// fakeStatuteSearcher 记录检索器收到的参数，并回放预设结果。
//
// 断言参数而不只断言响应：/statutes 的价值全在"把 q、law、top_k 原样交给检索器"，
// 只检查 200 的话，law 被吞掉、top_k 被写死成 5 这两类错误一条都拦不住。
type fakeStatuteSearcher struct {
	hits []port.StatuteHit
	err  error

	gotQ   string
	gotLaw string
	gotK   int
	calls  int
}

func (f *fakeStatuteSearcher) SearchIn(_ context.Context, input, law string, topK int) ([]port.StatuteHit, error) {
	f.calls++
	f.gotQ, f.gotLaw, f.gotK = input, law, topK
	if f.err != nil {
		return nil, f.err
	}
	return f.hits, nil
}

func newStatuteAPI(t *testing.T, s statuteSearcher) http.Handler {
	t.Helper()
	return New(testConfig("dev-user-0001"), discardLogger(), WithStatutes(s)).Handler()
}

func TestListStatutes(t *testing.T) {
	f := &fakeStatuteSearcher{hits: []port.StatuteHit{{
		Statute: port.Statute{
			ID:          "LCL-31",
			Law:         "中华人民共和国劳动合同法",
			Article:     "第三十一条",
			Title:       "加班",
			Text:        "用人单位应当严格执行劳动定额标准，不得强迫或者变相强迫劳动者加班。",
			URL:         "https://example.test/lcl-31",
			EffectiveAt: time.Date(2008, 1, 1, 0, 0, 0, 0, time.UTC),
		},
		Score: 1.5,
	}}}
	h := newStatuteAPI(t, f)

	law := "中华人民共和国劳动合同法"
	path := "/api/v1/statutes?q=" + url.QueryEscape("加班费") + "&top_k=3&law=" + url.QueryEscape(law)
	rec, env := doRequest(t, h, http.MethodGet, path, nil)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200, body=%s", rec.Code, rec.Body.String())
	}
	if env.Code != int(port.CodeOK) {
		t.Fatalf("code = %d, want 0", env.Code)
	}
	if f.gotQ != "加班费" {
		t.Fatalf("检索器收到的 q = %q, want 加班费", f.gotQ)
	}
	if f.gotLaw != law {
		t.Fatalf("检索器收到的 law = %q, want %q", f.gotLaw, law)
	}
	if f.gotK != 3 {
		t.Fatalf("检索器收到的 top_k = %d, want 3", f.gotK)
	}

	// 字段名必须按契约的 snake_case 输出：port 结构没有 json tag，
	// 漏写 DTO 就会变成 EffectiveAt / ID，前端一行都取不到。
	var data []map[string]any
	raw, err := json.Marshal(env.Data)
	if err != nil {
		t.Fatalf("重新序列化 data 失败: %v", err)
	}
	if err := json.Unmarshal(raw, &data); err != nil {
		t.Fatalf("data 不是数组: %v, raw=%s", err, raw)
	}
	if len(data) != 1 {
		t.Fatalf("data 长度 = %d, want 1", len(data))
	}
	hit := data[0]
	if _, ok := hit["statute"]; !ok {
		t.Fatalf("缺少 statute 字段: %v", hit)
	}
	if _, ok := hit["score"]; !ok {
		t.Fatalf("缺少 score 字段: %v", hit)
	}
	st, _ := hit["statute"].(map[string]any)
	for _, key := range []string{"id", "law", "article", "title", "text", "url", "effective_at"} {
		if _, ok := st[key]; !ok {
			t.Fatalf("statute 缺少字段 %q: %v", key, st)
		}
	}
	if st["effective_at"] != "2008-01-01T00:00:00Z" {
		t.Fatalf("effective_at = %v, want RFC3339", st["effective_at"])
	}
}

// TestListStatutesDefaultsTopK 守住契约的 default: 5。
func TestListStatutesDefaultsTopK(t *testing.T) {
	f := &fakeStatuteSearcher{hits: []port.StatuteHit{}}
	h := newStatuteAPI(t, f)

	rec, env := doRequest(t, h, http.MethodGet, "/api/v1/statutes?q="+url.QueryEscape("辞退"), nil)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	if f.gotK != statuteTopKDefault {
		t.Fatalf("默认 top_k = %d, want %d", f.gotK, statuteTopKDefault)
	}
	// 空结果必须是 []，不能是 null：否则前端遍历前还得判空。
	if raw, _ := json.Marshal(env.Data); string(raw) != "[]" {
		t.Fatalf("空结果的 data = %s, want []", raw)
	}
}

func TestListStatutesRejectsBadParams(t *testing.T) {
	tests := []struct {
		name string
		path string
	}{
		{name: "缺少 q", path: "/api/v1/statutes"},
		{name: "q 全是空白", path: "/api/v1/statutes?q=%20%20"},
		{name: "q 超长", path: "/api/v1/statutes?q=" + url.QueryEscape(repeatRune('法', statuteQueryMaxRunes+1))},
		{name: "top_k 非数字", path: "/api/v1/statutes?q=a&top_k=abc"},
		{name: "top_k 为 0", path: "/api/v1/statutes?q=a&top_k=0"},
		{name: "top_k 超过上限", path: "/api/v1/statutes?q=a&top_k=21"},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			f := &fakeStatuteSearcher{}
			h := newStatuteAPI(t, f)
			rec, env := doRequest(t, h, http.MethodGet, tc.path, nil)

			if rec.Code != http.StatusBadRequest {
				t.Fatalf("status = %d, want 400, body=%s", rec.Code, rec.Body.String())
			}
			if env.Code != int(port.CodeInvalidArgument) {
				t.Fatalf("code = %d, want %d", env.Code, port.CodeInvalidArgument)
			}
			// 参数都没过校验，就不该去打数据库。
			if f.calls != 0 {
				t.Fatalf("参数非法时仍调用了检索器 %d 次", f.calls)
			}
		})
	}
}

// TestListStatutesWithoutSearcher 守住无存储模式：没接检索器时明确 501。
func TestListStatutesWithoutSearcher(t *testing.T) {
	h := newTestAPI(t, "dev-user-0001")
	rec, env := doRequest(t, h, http.MethodGet, "/api/v1/statutes?q=a", nil)

	if rec.Code != http.StatusNotImplemented {
		t.Fatalf("status = %d, want 501", rec.Code)
	}
	if env.Code != int(port.CodeInternal) {
		t.Fatalf("code = %d, want %d", env.Code, port.CodeInternal)
	}
}

func repeatRune(r rune, n int) string {
	out := make([]rune, n)
	for i := range out {
		out[i] = r
	}
	return string(out)
}
