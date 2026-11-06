package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"

	"laborlaw/internal/port"
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

// TestSecondLoginKillsFirstRefresh 守住"一个用户只有一条续期链"。
//
// 缺陷形态：签发新令牌时只写新链、不注销旧链。上一枚 refresh 会一直活到 30 天
// 自然过期，logout 也吊销不到它——反查键早已被新 jti 覆盖，等于留下一条隐形通道。
func TestSecondLoginKillsFirstRefresh(t *testing.T) {
	h := authTestServer(t, authTestConfig("", 100), newFakeUserStore(), newFakeCache())
	first := registerUser(t, h, testPhone, testPassword)

	_, env := doJSON(t, h, http.MethodPost, "/api/v1/auth/login",
		map[string]string{"phone": testPhone, "password": testPassword}, nil)
	if env.Code != 0 {
		t.Fatalf("第二次登录失败: code=%d msg=%s", env.Code, env.Message)
	}
	second := authResult(t, env)

	rec, env := doJSON(t, h, http.MethodPost, "/api/v1/auth/refresh",
		map[string]string{"refresh_token": first.RefreshToken}, nil)
	if rec.Code != http.StatusUnauthorized || env.Code != 40101 {
		t.Fatalf("第一台设备的 refresh 仍然可用: status=%d code=%d（新登录没有顶掉旧链）", rec.Code, env.Code)
	}

	rec, env = doJSON(t, h, http.MethodPost, "/api/v1/auth/refresh",
		map[string]string{"refresh_token": second.RefreshToken}, nil)
	if rec.Code != http.StatusOK || env.Code != 0 {
		t.Fatalf("新链不可用: status=%d code=%d", rec.Code, env.Code)
	}
}

// TestClientIPIgnoresUntrustedXFF 守住限流键的来源。
//
// 缺陷形态：没有 SetTrustedProxies，gin 默认信任 0.0.0.0/0，ClientIP() 无条件采信
// X-Forwarded-For。撞库方每发一次请求换一个 XFF 就换一个计数桶，限流形同虚设；
// 而真挂上 nginx 之后，反过来所有人会被认成同一个代理 IP。
func TestClientIPIgnoresUntrustedXFF(t *testing.T) {
	cfg := authTestConfig("", 100)
	// 只信任本机回环，与生产默认一致。测试请求来自 192.0.2.1，属于不可信来源。
	cfg.HTTP.TrustedProxies = []string{"127.0.0.1", "::1"}
	cache := newFakeCache()
	h := authTestServer(t, cfg, newFakeUserStore(), cache)

	body := map[string]string{"phone": "13900139000", "password": "wrong-password"}
	doJSON(t, h, http.MethodPost, "/api/v1/auth/login", body, nil)
	doJSON(t, h, http.MethodPost, "/api/v1/auth/login", body,
		map[string]string{"X-Forwarded-For": "203.0.113.9"})

	cache.mu.Lock()
	defer cache.mu.Unlock()
	if len(cache.kv) != 1 {
		keys := make([]string, 0, len(cache.kv))
		for k := range cache.kv {
			keys = append(keys, k)
		}
		t.Fatalf("限流键 = %v, want 只有一个桶（XFF 不得改变计数域）", keys)
	}
	for k, v := range cache.kv {
		if k != keyLoginAttempts+"192.0.2.1" {
			t.Fatalf("限流键 = %q, want %q（应以 TCP 对端为准）", k, keyLoginAttempts+"192.0.2.1")
		}
		if v != "2" {
			t.Fatalf("计数 = %q, want 2（两次请求必须落在同一个桶）", v)
		}
	}
}

// TestRegisterRateLimited 守住注册接口的 CPU 预算。
//
// 缺陷形态：register 无条件跑 cost 12 的 bcrypt 且完全不限流。注册不需要知道
// 任何密码，单 IP 每秒几个请求就能把一个核占满。
func TestRegisterRateLimited(t *testing.T) {
	cache := newFakeCache()
	h := authTestServer(t, authTestConfig("", 100), newFakeUserStore(), cache)

	for i := 0; i < registerRateLimit; i++ {
		phone := "1380013" + strconv.Itoa(8000+i)
		rec, env := doJSON(t, h, http.MethodPost, "/api/v1/auth/register",
			map[string]string{"phone": phone, "password": testPassword}, nil)
		if rec.Code != http.StatusOK || env.Code != 0 {
			t.Fatalf("第 %d 次注册: status=%d code=%d msg=%s", i+1, rec.Code, env.Code, env.Message)
		}
	}

	rec, env := doJSON(t, h, http.MethodPost, "/api/v1/auth/register",
		map[string]string{"phone": "13800139999", "password": testPassword}, nil)
	if rec.Code != http.StatusTooManyRequests || env.Code != 42901 {
		t.Fatalf("超限后 status=%d code=%d, want 429/42901", rec.Code, env.Code)
	}
}

// TestRefreshUserLookupFailureIsInternal 区分"用户没了"和"库挂了"。
//
// 缺陷形态：refresh 里 a.users.Get 的任何错误都走 failUnauthorized，
// 于是 MySQL 抖动被报成"登录失效"，而旧 jti 已经删掉，用户被迫重新登录，
// 日志里也只剩一次正常的过期事件——故障与业务错误混成同一个码。
func TestRefreshUserLookupFailureIsInternal(t *testing.T) {
	users := newFakeUserStore()
	h := authTestServer(t, authTestConfig("", 100), users, newFakeCache())
	reg := registerUser(t, h, testPhone, testPassword)

	users.getErr = port.New(port.CodeInternal, "模拟数据库故障")

	rec, env := doJSON(t, h, http.MethodPost, "/api/v1/auth/refresh",
		map[string]string{"refresh_token": reg.RefreshToken}, nil)
	if rec.Code != http.StatusInternalServerError || env.Code != 50001 {
		t.Fatalf("存储故障时续期: status=%d code=%d, want 500/50001（不能伪装成令牌失效）", rec.Code, env.Code)
	}
	if env.Message != msgRefreshService {
		t.Fatalf("message = %q, want %q", env.Message, msgRefreshService)
	}
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
