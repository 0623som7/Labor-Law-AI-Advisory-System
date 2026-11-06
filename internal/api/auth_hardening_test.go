package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
)

// postJSONStatus 在 goroutine 里发请求，只返回状态码。
//
// 不用 doJSON：那个助手会调 t.Fatalf，而 FailNow 只能从测试主 goroutine 调，
// 并发用例里用它会直接把整个测试进程打乱（表现为莫名其妙的 panic 而不是失败信息）。
func postJSONStatus(h http.Handler, path, body string) int {
	req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec.Code
}

// TestRefreshConcurrentOnlyOneWins 守住轮换的原子性。
//
// 缺陷形态：refresh 先 get 白名单再 del。八个并发请求可以全部 get 命中，
// 于是同一枚 refresh 换出八条续期链——令牌被窃取后攻击者与用户各拿一条，
// 谁都不会被踢下线。
func TestRefreshConcurrentOnlyOneWins(t *testing.T) {
	h := authTestServer(t, authTestConfig("", 100), newFakeUserStore(), newFakeCache())
	reg := registerUser(t, h, testPhone, testPassword)

	const workers = 8
	body, err := json.Marshal(map[string]string{"refresh_token": reg.RefreshToken})
	if err != nil {
		t.Fatalf("序列化请求体失败: %v", err)
	}

	codes := make([]int, workers)
	start := make(chan struct{})
	var wg sync.WaitGroup
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start
			codes[i] = postJSONStatus(h, "/api/v1/auth/refresh", string(body))
		}(i)
	}
	close(start)
	wg.Wait()

	won := 0
	for _, code := range codes {
		if code == http.StatusOK {
			won++
		}
	}
	if won != 1 {
		t.Fatalf("并发续期成功 %d 次，want 1（同一枚 refresh 只能换一次）: %v", won, codes)
	}
}
