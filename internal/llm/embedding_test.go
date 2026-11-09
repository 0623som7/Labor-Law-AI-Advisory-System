package llm

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"laborlaw/internal/port"
)

// embeddedBody 是假上游读到的请求体，用来断言批大小与条数。
type embeddedBody struct {
	Model          string   `json:"model"`
	Input          []string `json:"input"`
	EncodingFormat string   `json:"encoding_format"`
}

// embedServer 起一个假 /embeddings 上游。
//
// reply 收到本次请求里要嵌的文本，返回 (status, body)——body 由各用例自己拼，
// 这样才能构造"乱序返回""少返回一条"这种真实网关干过的事。
func embedServer(t *testing.T, reply func(input []string) (int, string)) (*httptest.Server, *[]embeddedBody, *int32) {
	t.Helper()
	var mu sync.Mutex
	calls := 0
	bodies := make([]embeddedBody, 0, 4)
	var count int32

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/embeddings" {
			t.Errorf("路径 = %q, want /embeddings", r.URL.Path)
		}
		if got := r.Header.Get("Authorization"); got != "Bearer sk-test-key-do-not-leak" {
			t.Errorf("Authorization = %q", got)
		}
		var body embeddedBody
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Errorf("请求体不是合法 JSON: %v", err)
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		if body.EncodingFormat != "float" {
			t.Errorf("encoding_format = %q, want float", body.EncodingFormat)
		}
		mu.Lock()
		calls++
		bodies = append(bodies, body)
		mu.Unlock()
		atomic.AddInt32(&count, 1)

		status, out := reply(body.Input)
		w.WriteHeader(status)
		_, _ = w.Write([]byte(out))
	}))
	t.Cleanup(srv.Close)
	return srv, &bodies, &count
}

// dimsOf 生成"每条文本一个固定向量"的响应，index 顺序由 order 决定。
func dimsOf(input []string, order []int) string {
	var b strings.Builder
	b.WriteString(`{"data":[`)
	for i, idx := range order {
		if i > 0 {
			b.WriteString(",")
		}
		// 向量内容带上入参下标，方便断言语义对齐而不是只看长度。
		fmt.Fprintf(&b, `{"index":%d,"embedding":[%d.0,0.5]}`, idx, idx)
	}
	b.WriteString(`],"usage":{"prompt_tokens":3,"total_tokens":3}}`)
	return b.String()
}

// TestEmbedKeepsInputOrderDespiteUnorderedResponse 钉住"按 index 对齐"。
//
// DashScope 与部分自建网关在批量请求里会乱序返回 data，按下标直接用会把
// 第 3 条的向量存成第 1 条的。
func TestEmbedKeepsInputOrderDespiteUnorderedResponse(t *testing.T) {
	srv, _, _ := embedServer(t, func(input []string) (int, string) {
		if len(input) != 3 {
			t.Fatalf("假上游收到 %d 条，want 3", len(input))
		}
		return http.StatusOK, dimsOf(input, []int{2, 0, 1})
	})

	e := NewEmbeddingClient(testConfig(srv.URL))
	vecs, err := e.Embed(context.Background(), "text-embedding-v3", []string{"甲", "乙", "丙"})
	if err != nil {
		t.Fatalf("Embed 返回错误: %v", err)
	}
	if len(vecs) != 3 {
		t.Fatalf("返回 %d 条向量，want 3", len(vecs))
	}
	for i, vec := range vecs {
		if len(vec) != 2 {
			t.Fatalf("第 %d 条向量维度 = %d, want 2", i, len(vec))
		}
		if vec[0] != float32(i) {
			t.Fatalf("第 %d 条向量首分量 = %v, want %d（顺序没对齐）", i, vec[0], i)
		}
	}
}

// TestEmbedSplitsBatches 钉住客户端自己切批：25 条 → 3 次请求（10/10/5）。
//
// 切批交给调用方，切错就是静默丢一批向量；这条测试就是为了让"谁来切"这件事
// 在客户端内部闭环。
func TestEmbedSplitsBatches(t *testing.T) {
	srv, bodies, _ := embedServer(t, func(input []string) (int, string) {
		order := make([]int, len(input))
		for i := range input {
			order[i] = i
		}
		return http.StatusOK, dimsOf(input, order)
	})

	texts := make([]string, 25)
	for i := range texts {
		texts[i] = fmt.Sprintf("文本%d", i)
	}

	e := NewEmbeddingClient(testConfig(srv.URL))
	vecs, err := e.Embed(context.Background(), "text-embedding-v3", texts)
	if err != nil {
		t.Fatalf("Embed 返回错误: %v", err)
	}
	if len(vecs) != 25 {
		t.Fatalf("返回 %d 条向量，want 25", len(vecs))
	}

	if len(*bodies) != 3 {
		t.Fatalf("请求次数 = %d, want 3", len(*bodies))
	}
	wantSizes := []int{10, 10, 5}
	seen := make(map[string]int, 25)
	for i, body := range *bodies {
		if len(body.Input) != wantSizes[i] {
			t.Fatalf("第 %d 批条数 = %d, want %d", i+1, len(body.Input), wantSizes[i])
		}
		for _, s := range body.Input {
			seen[s]++
		}
	}
	if len(seen) != 25 {
		t.Fatalf("被嵌的文本去重后 %d 条，want 25", len(seen))
	}
	for s, n := range seen {
		if n != 1 {
			t.Fatalf("%q 被嵌了 %d 次，want 1", s, n)
		}
	}
}

// TestEmbedRejectsCountMismatch 钉住"条数不符就报 50201"，不许凑合着用。
func TestEmbedRejectsCountMismatch(t *testing.T) {
	srv, _, _ := embedServer(t, func(input []string) (int, string) {
		// 请求 3 条只回 2 条，且下标是 0/1。
		return http.StatusOK, dimsOf(input, []int{0, 1})
	})

	e := NewEmbeddingClient(testConfig(srv.URL))
	_, err := e.Embed(context.Background(), "text-embedding-v3", []string{"甲", "乙", "丙"})

	if err == nil {
		t.Fatal("期望报错，实际成功")
	}
	if got := port.CodeOf(err); got != port.CodeUpstream {
		t.Fatalf("错误码 = %s, want %s", got, port.CodeUpstream)
	}
	if !strings.Contains(err.Error(), "向量条数与请求不一致") {
		t.Fatalf("错误信息 = %q", err.Error())
	}
}

// TestEmbedRejectsDuplicateIndex 覆盖"两个条目抢同一个下标"的畸形响应：
// 不拦的话会有一条入参拿到 nil 向量，余弦那里直接除零。
func TestEmbedRejectsDuplicateIndex(t *testing.T) {
	srv, _, _ := embedServer(t, func(input []string) (int, string) {
		return http.StatusOK, dimsOf(input, []int{0, 0, 2})
	})

	e := NewEmbeddingClient(testConfig(srv.URL))
	_, err := e.Embed(context.Background(), "text-embedding-v3", []string{"甲", "乙", "丙"})

	if got := port.CodeOf(err); got != port.CodeUpstream {
		t.Fatalf("错误码 = %s, want %s（err=%v）", got, port.CodeUpstream, err)
	}
}

// TestEmbedRejectsEmptyText 钉住空串在发请求之前就被拦下。
func TestEmbedRejectsEmptyText(t *testing.T) {
	srv, _, count := embedServer(t, func(input []string) (int, string) {
		order := make([]int, len(input))
		for i := range input {
			order[i] = i
		}
		return http.StatusOK, dimsOf(input, order)
	})

	e := NewEmbeddingClient(testConfig(srv.URL))
	_, err := e.Embed(context.Background(), "text-embedding-v3", []string{"甲", "", "丙"})

	if err == nil {
		t.Fatal("期望报错，实际成功")
	}
	if got := port.CodeOf(err); got != port.CodeInvalidArgument {
		t.Fatalf("错误码 = %s, want %s", got, port.CodeInvalidArgument)
	}
	if n := atomic.LoadInt32(count); n != 0 {
		t.Fatalf("空串不该发请求，实际调用了 %d 次", n)
	}
}

// TestEmbedRedactsAPIKey 钉住错误链里不出现 key。
//
// 有些网关会把 Authorization 头回显在 401 正文里（"Bearer sk-... is invalid"），
// 这类文本进日志等于把 key 写进盘。
func TestEmbedRedactsAPIKey(t *testing.T) {
	srv, _, _ := embedServer(t, func(input []string) (int, string) {
		return http.StatusUnauthorized, `{"error":{"message":"Bearer sk-test-key-do-not-leak is invalid"}}`
	})

	e := NewEmbeddingClient(testConfig(srv.URL))
	_, err := e.Embed(context.Background(), "text-embedding-v3", []string{"甲"})

	if err == nil {
		t.Fatal("期望报错，实际成功")
	}
	if got := port.CodeOf(err); got != port.CodeInvalidArgument {
		t.Fatalf("错误码 = %s, want %s", got, port.CodeInvalidArgument)
	}
	if strings.Contains(err.Error(), "sk-test-key-do-not-leak") {
		t.Fatalf("错误串里带了 API key：%q", err.Error())
	}
	if !strings.Contains(err.Error(), "鉴权失败") {
		t.Fatalf("错误信息 = %q", err.Error())
	}
}

// TestEmbedSurfacesUpstreamStatus 钉住 5xx 带上状态码，方便对账上游故障。
func TestEmbedSurfacesUpstreamStatus(t *testing.T) {
	srv, _, _ := embedServer(t, func(input []string) (int, string) {
		return http.StatusInternalServerError, `{"error":{"message":"boom"}}`
	})

	e := NewEmbeddingClient(testConfig(srv.URL))
	_, err := e.Embed(context.Background(), "text-embedding-v3", []string{"甲"})

	if got := port.CodeOf(err); got != port.CodeUpstream {
		t.Fatalf("错误码 = %s, want %s", got, port.CodeUpstream)
	}
	if !strings.Contains(err.Error(), "HTTP 500") {
		t.Fatalf("错误信息里没有状态码：%q", err.Error())
	}
}

// TestEmbedSendsModelAndBatchSize 钉住请求体里的模型名就来自调用方，
// 且批次不超过 DashScope 的 10 条上限。
func TestEmbedSendsModelAndBatchSize(t *testing.T) {
	srv, bodies, _ := embedServer(t, func(input []string) (int, string) {
		order := make([]int, len(input))
		for i := range input {
			order[i] = i
		}
		return http.StatusOK, dimsOf(input, order)
	})

	texts := make([]string, 11)
	for i := range texts {
		texts[i] = fmt.Sprintf("文本%d", i)
	}

	e := NewEmbeddingClient(testConfig(srv.URL))
	if _, err := e.Embed(context.Background(), "my-embed-model", texts); err != nil {
		t.Fatalf("Embed 返回错误: %v", err)
	}

	if len(*bodies) != 2 {
		t.Fatalf("请求次数 = %d, want 2", len(*bodies))
	}
	for _, body := range *bodies {
		if body.Model != "my-embed-model" {
			t.Fatalf("请求里的 model = %q", body.Model)
		}
		if len(body.Input) > maxEmbedBatch {
			t.Fatalf("单批 %d 条，超过上限 %d", len(body.Input), maxEmbedBatch)
		}
	}
}
