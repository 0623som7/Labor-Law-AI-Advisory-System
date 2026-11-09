package llm

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"laborlaw/internal/config"
	"laborlaw/internal/port"
)

// rerankBody 是假上游读到的请求体。
type rerankBody struct {
	Model string `json:"model"`
	Input struct {
		Query     string   `json:"query"`
		Documents []string `json:"documents"`
	} `json:"input"`
	Parameters struct {
		Instruct string `json:"instruct"`
	} `json:"parameters"`
}

func testRerankConfig() config.LLMConfig {
	return config.LLMConfig{
		APIKey:      "sk-test-key-do-not-leak",
		RerankModel: "qwen3-rerank",
		Timeout:     5 * time.Second,
	}
}

// rerankServer 起一个假重排上游，reply 收到请求体，返回 (status, body)。
func rerankServer(t *testing.T, reply func(body rerankBody) (int, string)) (*httptest.Server, *int32, *[]rerankBody) {
	t.Helper()
	var count int32
	bodies := make([]rerankBody, 0, 2)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("Authorization"); got != "Bearer sk-test-key-do-not-leak" {
			t.Errorf("Authorization = %q", got)
		}
		var body rerankBody
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Errorf("请求体不是合法 JSON: %v", err)
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		bodies = append(bodies, body)
		atomic.AddInt32(&count, 1)
		status, out := reply(body)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_, _ = w.Write([]byte(out))
	}))
	t.Cleanup(srv.Close)
	return srv, &count, &bodies
}

func newTestRerankClient(srv *httptest.Server) *RerankClient {
	return NewRerankClient(testRerankConfig(), srv.URL, WithRerankHTTPClient(srv.Client()))
}

// results 拼一个上游响应，items 按给定顺序输出。
func rerankResults(items ...rerankItem) string {
	payload := map[string]any{"output": map[string]any{"results": items}}
	raw, _ := json.Marshal(payload)
	return string(raw)
}

func TestRerankAlignsByIndex(t *testing.T) {
	// 上游按分数排好序返回（2,0,1），回填后必须是"第 i 个分数对应第 i 条文档"。
	srv, _, _ := rerankServer(t, func(body rerankBody) (int, string) {
		return http.StatusOK, rerankResults(
			rerankItem{Index: 2, RelevanceScore: 0.9},
			rerankItem{Index: 0, RelevanceScore: 0.5},
			rerankItem{Index: 1, RelevanceScore: 0.1},
		)
	})
	scores, err := newTestRerankClient(srv).Rerank(context.Background(), "兼职有试用期吗",
		[]string{"doc-0", "doc-1", "doc-2"})
	if err != nil {
		t.Fatalf("Rerank: %v", err)
	}
	want := []float64{0.5, 0.1, 0.9}
	for i := range want {
		if scores[i] != want[i] {
			t.Errorf("scores[%d] = %v, want %v", i, scores[i], want[i])
		}
	}
}

func TestRerankRejectsLengthMismatch(t *testing.T) {
	srv, _, _ := rerankServer(t, func(body rerankBody) (int, string) {
		return http.StatusOK, rerankResults(
			rerankItem{Index: 0, RelevanceScore: 0.5},
			rerankItem{Index: 1, RelevanceScore: 0.4},
		)
	})
	_, err := newTestRerankClient(srv).Rerank(context.Background(), "q", []string{"a", "b", "c"})
	if port.CodeOf(err) != port.CodeUpstream {
		t.Fatalf("err = %v, want 50201", err)
	}
}

func TestRerankRejectsDuplicateIndex(t *testing.T) {
	srv, _, _ := rerankServer(t, func(body rerankBody) (int, string) {
		return http.StatusOK, rerankResults(
			rerankItem{Index: 1, RelevanceScore: 0.5},
			rerankItem{Index: 1, RelevanceScore: 0.4},
		)
	})
	_, err := newTestRerankClient(srv).Rerank(context.Background(), "q", []string{"a", "b"})
	if port.CodeOf(err) != port.CodeUpstream {
		t.Fatalf("err = %v, want 50201", err)
	}
}

func TestRerankSurfacesErrorCode(t *testing.T) {
	// DashScope 的鉴权失败会回 200 + code 字段，不能只看 status。
	srv, _, _ := rerankServer(t, func(body rerankBody) (int, string) {
		return http.StatusOK, `{"code":"InvalidApiKey","message":"Invalid API-key provided."}`
	})
	_, err := newTestRerankClient(srv).Rerank(context.Background(), "q", []string{"a"})
	if port.CodeOf(err) != port.CodeUpstream {
		t.Fatalf("err = %v, want 50201", err)
	}
	appErr := port.AsAppError(err)
	if appErr == nil || !contains(appErr.Message, "InvalidApiKey") {
		t.Fatalf("message 应含 InvalidApiKey，实际 %q", appErr.Message)
	}
}

func TestRerankRedactsAPIKey(t *testing.T) {
	srv, _, _ := rerankServer(t, func(body rerankBody) (int, string) {
		return http.StatusOK, `{"code":"InvalidApiKey","message":"bad key sk-test-key-do-not-leak"}`
	})
	_, err := newTestRerankClient(srv).Rerank(context.Background(), "q", []string{"a"})
	if err == nil {
		t.Fatal("应当返回错误")
	}
	if contains(port.AsAppError(err).Message, "sk-test-key-do-not-leak") {
		t.Fatalf("错误信息里漏出了 API key: %q", port.AsAppError(err).Message)
	}
}

func TestRerankEmptyDocuments(t *testing.T) {
	srv, calls, _ := rerankServer(t, func(body rerankBody) (int, string) {
		t.Error("空文档不应发请求")
		return http.StatusOK, rerankResults()
	})
	scores, err := newTestRerankClient(srv).Rerank(context.Background(), "q", nil)
	if err != nil {
		t.Fatalf("Rerank: %v", err)
	}
	if scores == nil {
		t.Fatal("空切片不能是 nil")
	}
	if len(scores) != 0 {
		t.Errorf("len = %d", len(scores))
	}
	if atomic.LoadInt32(calls) != 0 {
		t.Errorf("调用次数 = %d, want 0", *calls)
	}
}

func TestRerankTruncatesLongDocument(t *testing.T) {
	long := ""
	for i := 0; i < 4000; i++ {
		long += "字"
	}
	srv, _, bodies := rerankServer(t, func(body rerankBody) (int, string) {
		return http.StatusOK, rerankResults(rerankItem{Index: 0, RelevanceScore: 0.7})
	})
	if _, err := newTestRerankClient(srv).Rerank(context.Background(), "q", []string{long}); err != nil {
		t.Fatalf("Rerank: %v", err)
	}
	sent := []rune((*bodies)[0].Input.Documents[0])
	if len(sent) != maxRerankDocRunes {
		t.Errorf("发出的文档长度 = %d rune, want %d", len(sent), maxRerankDocRunes)
	}
}

func TestRerankRejectsTooManyDocuments(t *testing.T) {
	docs := make([]string, maxRerankDocuments+1)
	for i := range docs {
		docs[i] = "x"
	}
	srv, calls, _ := rerankServer(t, func(body rerankBody) (int, string) {
		t.Error("超限不应发请求")
		return http.StatusOK, rerankResults()
	})
	_, err := newTestRerankClient(srv).Rerank(context.Background(), "q", docs)
	if port.CodeOf(err) != port.CodeInvalidArgument {
		t.Fatalf("err = %v, want 40001", err)
	}
	if atomic.LoadInt32(calls) != 0 {
		t.Errorf("调用次数 = %d, want 0", *calls)
	}
}

// contains 判断子串，独立于测试里别的同名 helper。
func contains(s, sub string) bool {
	if sub == "" {
		return true
	}
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}
