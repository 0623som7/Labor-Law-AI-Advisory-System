package api

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gin-gonic/gin"

	"laborlaw/internal/auth"
	"laborlaw/internal/config"
	"laborlaw/internal/port"
)

const (
	testSecret   = "0123456789abcdef0123456789abcdef"
	testPhone    = "13800138000"
	testPassword = "password123"
)

// authTestConfig 是账号域测试的配置：staging 让 gin 走 ReleaseMode，
// 密钥至少 32 字节（与 config 的 minSecretLen 一致），否则 Sign 会直接报错。
func authTestConfig(devUser string, rateLimit int) config.Config {
	cfg := testConfig(devUser)
	cfg.Auth = config.AuthConfig{
		Secret:         testSecret,
		Issuer:         "laborlaw-test",
		AccessTTL:      2 * time.Hour,
		RefreshTTL:     30 * 24 * time.Hour,
		DevUser:        devUser,
		LoginRateLimit: rateLimit,
	}
	return cfg
}

// fakeUserStore 是内存版 port.UserStore。
//
// 行为对齐真实 DAL 的两处约定：Create 会把 u.Phone 就地改成脱敏形态；
// 查不到时返回 CodeNotFound，而不是 (nil, nil)。
type fakeUserStore struct {
	mu      sync.Mutex
	byID    map[string]*port.User
	byPhone map[string]*port.User
	// createErr 让测试注入"不是唯一键冲突"的存储故障。
	createErr error
}

func newFakeUserStore() *fakeUserStore {
	return &fakeUserStore{byID: map[string]*port.User{}, byPhone: map[string]*port.User{}}
}

func (f *fakeUserStore) Create(_ context.Context, u *port.User) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.createErr != nil {
		return f.createErr
	}
	if _, ok := f.byPhone[u.Phone]; ok {
		return port.New(port.CodeConflict, "手机号已存在")
	}
	clone := *u
	clone.Phone = auth.Mask(u.Phone)
	f.byPhone[u.Phone] = &clone
	stored := clone
	f.byID[clone.ID] = &stored
	// 与 DAL 一致：调用方拿到的对象也被脱敏。
	u.Phone = clone.Phone
	return nil
}

func (f *fakeUserStore) GetByPhone(_ context.Context, phone string) (*port.User, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	u, ok := f.byPhone[phone]
	if !ok {
		return nil, port.New(port.CodeNotFound, "用户不存在")
	}
	clone := *u
	return &clone, nil
}

func (f *fakeUserStore) Get(_ context.Context, id string) (*port.User, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	u, ok := f.byID[id]
	if !ok {
		return nil, port.New(port.CodeNotFound, "用户不存在")
	}
	clone := *u
	return &clone, nil
}

var _ port.UserStore = (*fakeUserStore)(nil)

// fakeCache 是内存版 authCache，实现与 Redis 版相同的语义（过期时间不真正生效，
// 需要时用 del 模拟）。down=true 时所有方法报 errAuthCacheDown，用于测降级分支。
type fakeCache struct {
	mu   sync.Mutex
	kv   map[string]string
	down bool
}

func newFakeCache() *fakeCache { return &fakeCache{kv: map[string]string{}} }

func (f *fakeCache) get(_ context.Context, key string) (string, bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.down {
		return "", false, errAuthCacheDown
	}
	v, ok := f.kv[key]
	return v, ok, nil
}

func (f *fakeCache) set(_ context.Context, key, value string, ttl time.Duration) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.down {
		return errAuthCacheDown
	}
	if ttl <= 0 {
		return errors.New("ttl 必须为正")
	}
	f.kv[key] = value
	return nil
}

func (f *fakeCache) del(_ context.Context, keys ...string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.down {
		return errAuthCacheDown
	}
	for _, k := range keys {
		delete(f.kv, k)
	}
	return nil
}

func (f *fakeCache) incr(_ context.Context, key string) (int64, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.down {
		return 0, errAuthCacheDown
	}
	n, _ := strconv.ParseInt(f.kv[key], 10, 64)
	n++
	f.kv[key] = strconv.FormatInt(n, 10)
	return n, nil
}

func (f *fakeCache) expire(_ context.Context, _ string, ttl time.Duration) error {
	if f.down {
		return errAuthCacheDown
	}
	if ttl <= 0 {
		return errors.New("ttl 必须为正")
	}
	return nil
}

var _ authCache = (*fakeCache)(nil)

// authTestServer 组装一个只依赖假存储的 API。
func authTestServer(t *testing.T, cfg config.Config, users port.UserStore, cache authCache) http.Handler {
	t.Helper()
	gin.SetMode(gin.TestMode)
	opts := []Option{WithUsers(users)}
	if cache != nil {
		opts = append(opts, func(a *API) { a.cache = cache })
	}
	return New(cfg, discardLogger(), opts...).Handler()
}

// doJSON 发一个带 JSON 体的请求，返回状态码与包络。
func doJSON(t *testing.T, h http.Handler, method, path string, body any, headers map[string]string) (*httptest.ResponseRecorder, Envelope) {
	t.Helper()

	var reader *strings.Reader
	if body == nil {
		reader = strings.NewReader("")
	} else {
		raw, err := json.Marshal(body)
		if err != nil {
			t.Fatalf("序列化请求体失败: %v", err)
		}
		reader = strings.NewReader(string(raw))
	}

	req := httptest.NewRequest(method, path, reader)
	req.Header.Set("Content-Type", "application/json")
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	var env Envelope
	if err := json.Unmarshal(rec.Body.Bytes(), &env); err != nil {
		t.Fatalf("%s %s 的响应不是 JSON: %v，body=%q", method, path, err, rec.Body.String())
	}
	return rec, env
}

// authResult 把包络的 data 还原成 AuthResult，断言令牌三件套用。
func authResult(t *testing.T, env Envelope) authResultDTO {
	t.Helper()
	raw, err := json.Marshal(env.Data)
	if err != nil {
		t.Fatalf("重新编码 data 失败: %v", err)
	}
	var res authResultDTO
	if err := json.Unmarshal(raw, &res); err != nil {
		t.Fatalf("data 不是 AuthResult: %v，data=%s", err, raw)
	}
	return res
}

// registerUser 走真实 HTTP 路径注册一个用户并返回令牌。
func registerUser(t *testing.T, h http.Handler, phone, password string) authResultDTO {
	t.Helper()
	rec, env := doJSON(t, h, http.MethodPost, "/api/v1/auth/register",
		map[string]string{"phone": phone, "password": password, "nickname": "小明"}, nil)
	if rec.Code != http.StatusOK || env.Code != 0 {
		t.Fatalf("注册失败: status=%d code=%d msg=%s", rec.Code, env.Code, env.Message)
	}
	return authResult(t, env)
}

func authHeader(token string) map[string]string {
	return map[string]string{"Authorization": "Bearer " + token}
}

func TestRegisterReturnsTokensAndMaskedPhone(t *testing.T) {
	h := authTestServer(t, authTestConfig("", 5), newFakeUserStore(), newFakeCache())

	res := registerUser(t, h, testPhone, testPassword)

	if res.User.PhoneMasked != "138****8000" {
		t.Fatalf("phone_masked = %q, want 138****8000", res.User.PhoneMasked)
	}
	if res.User.ID == "" || len(res.User.ID) != 26 {
		t.Fatalf("user.id = %q, want 26 位 ULID", res.User.ID)
	}
	if res.AccessToken == "" || res.RefreshToken == "" {
		t.Fatal("注册必须直接下发 access_token 与 refresh_token")
	}
	if res.ExpiresIn != int64((2*time.Hour)/time.Second) {
		t.Fatalf("expires_in = %d, want %d", res.ExpiresIn, int64(2*time.Hour/time.Second))
	}
	if strings.Contains(envJSON(t, res), testPhone) {
		t.Fatal("注册响应里出现了手机号明文")
	}
}

// envJSON 把 DTO 编回字符串，用来做"整个响应体里有没有明文"的断言。
func envJSON(t *testing.T, v any) string {
	t.Helper()
	raw, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("序列化失败: %v", err)
	}
	return string(raw)
}

func TestRegisterDuplicatePhoneConflict(t *testing.T) {
	h := authTestServer(t, authTestConfig("", 5), newFakeUserStore(), newFakeCache())
	registerUser(t, h, testPhone, testPassword)

	rec, env := doJSON(t, h, http.MethodPost, "/api/v1/auth/register",
		map[string]string{"phone": testPhone, "password": testPassword}, nil)

	if rec.Code != http.StatusConflict || env.Code != 40901 {
		t.Fatalf("status=%d code=%d, want 409/40901", rec.Code, env.Code)
	}
}

func TestRegisterValidation(t *testing.T) {
	h := authTestServer(t, authTestConfig("", 5), newFakeUserStore(), newFakeCache())

	cases := []struct {
		name  string
		body  map[string]string
		field string
	}{
		{"手机号格式错", map[string]string{"phone": "12345", "password": testPassword}, "phone"},
		{"密码太短", map[string]string{"phone": testPhone, "password": "abc"}, "password"},
		{"密码太长", map[string]string{"phone": testPhone, "password": strings.Repeat("a", 65)}, "password"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rec, env := doJSON(t, h, http.MethodPost, "/api/v1/auth/register", tc.body, nil)
			if rec.Code != http.StatusBadRequest || env.Code != 40001 {
				t.Fatalf("status=%d code=%d, want 400/40001", rec.Code, env.Code)
			}
			if _, ok := env.Fields[tc.field]; !ok {
				t.Fatalf("fields = %v, want 含 %q", env.Fields, tc.field)
			}
		})
	}
}

func TestLoginFailuresAreIndistinguishable(t *testing.T) {
	h := authTestServer(t, authTestConfig("", 100), newFakeUserStore(), newFakeCache())
	registerUser(t, h, testPhone, testPassword)

	wrongPwdRec, wrongPwdEnv := doJSON(t, h, http.MethodPost, "/api/v1/auth/login",
		map[string]string{"phone": testPhone, "password": "wrong-password"}, nil)
	unknownRec, unknownEnv := doJSON(t, h, http.MethodPost, "/api/v1/auth/login",
		map[string]string{"phone": "13900139000", "password": testPassword}, nil)

	if wrongPwdRec.Code != http.StatusUnauthorized || wrongPwdEnv.Code != 40101 {
		t.Fatalf("密码错: status=%d code=%d, want 401/40101", wrongPwdRec.Code, wrongPwdEnv.Code)
	}
	if unknownRec.Code != wrongPwdRec.Code || unknownEnv.Code != wrongPwdEnv.Code {
		t.Fatalf("账号不存在与密码错的响应不同: %d/%d vs %d/%d",
			unknownRec.Code, unknownEnv.Code, wrongPwdRec.Code, wrongPwdEnv.Code)
	}
	if unknownEnv.Message != wrongPwdEnv.Message {
		t.Fatalf("文案不同: %q vs %q —— 可据文案枚举账号", unknownEnv.Message, wrongPwdEnv.Message)
	}
	if wrongPwdEnv.Message != msgBadCredentials {
		t.Fatalf("message = %q, want %q", wrongPwdEnv.Message, msgBadCredentials)
	}
}

func TestLoginSucceedsWithCorrectPassword(t *testing.T) {
	h := authTestServer(t, authTestConfig("", 5), newFakeUserStore(), newFakeCache())
	registerUser(t, h, testPhone, testPassword)

	rec, env := doJSON(t, h, http.MethodPost, "/api/v1/auth/login",
		map[string]string{"phone": testPhone, "password": testPassword}, nil)
	if rec.Code != http.StatusOK || env.Code != 0 {
		t.Fatalf("登录失败: status=%d code=%d msg=%s", rec.Code, env.Code, env.Message)
	}
	if res := authResult(t, env); res.AccessToken == "" {
		t.Fatal("登录未返回 access_token")
	}
}

func TestLoginRateLimit(t *testing.T) {
	h := authTestServer(t, authTestConfig("", 3), newFakeUserStore(), newFakeCache())

	for i := 0; i < 3; i++ {
		rec, _ := doJSON(t, h, http.MethodPost, "/api/v1/auth/login",
			map[string]string{"phone": "13900139000", "password": testPassword}, nil)
		if rec.Code != http.StatusUnauthorized {
			t.Fatalf("第 %d 次: status=%d, want 401", i+1, rec.Code)
		}
	}
	rec, env := doJSON(t, h, http.MethodPost, "/api/v1/auth/login",
		map[string]string{"phone": "13900139000", "password": testPassword}, nil)
	if rec.Code != http.StatusTooManyRequests || env.Code != 42901 {
		t.Fatalf("超限后 status=%d code=%d, want 429/42901", rec.Code, env.Code)
	}
}

func TestLoginRateLimitDegradesWhenCacheDown(t *testing.T) {
	cache := newFakeCache()
	cache.down = true
	h := authTestServer(t, authTestConfig("", 1), newFakeUserStore(), cache)
	registerUser(t, h, testPhone, testPassword)

	// 限流不可用应当放行，而不是把所有人挡在 429 后面。
	for i := 0; i < 3; i++ {
		rec, env := doJSON(t, h, http.MethodPost, "/api/v1/auth/login",
			map[string]string{"phone": testPhone, "password": testPassword}, nil)
		if rec.Code != http.StatusOK || env.Code != 0 {
			t.Fatalf("第 %d 次: status=%d code=%d, want 200 —— 缓存不可用时限流应降级放行", i+1, rec.Code, env.Code)
		}
	}
}

func TestRefreshRotatesAndOldTokenDies(t *testing.T) {
	h := authTestServer(t, authTestConfig("", 5), newFakeUserStore(), newFakeCache())
	reg := registerUser(t, h, testPhone, testPassword)

	rec, env := doJSON(t, h, http.MethodPost, "/api/v1/auth/refresh",
		map[string]string{"refresh_token": reg.RefreshToken}, nil)
	if rec.Code != http.StatusOK || env.Code != 0 {
		t.Fatalf("续期失败: status=%d code=%d msg=%s", rec.Code, env.Code, env.Message)
	}
	next := authResult(t, env)
	if next.RefreshToken == reg.RefreshToken {
		t.Fatal("轮换后 refresh_token 未变化")
	}

	// 同一个 refresh 再用一次必须失效：否则被窃取的令牌可以无限续期。
	rec, env = doJSON(t, h, http.MethodPost, "/api/v1/auth/refresh",
		map[string]string{"refresh_token": reg.RefreshToken}, nil)
	if rec.Code != http.StatusUnauthorized || env.Code != 40101 {
		t.Fatalf("旧 refresh 复用: status=%d code=%d, want 401/40101", rec.Code, env.Code)
	}

	// 新令牌仍然可用，证明失效的只是旧的那一枚。
	rec, env = doJSON(t, h, http.MethodPost, "/api/v1/auth/refresh",
		map[string]string{"refresh_token": next.RefreshToken}, nil)
	if rec.Code != http.StatusOK || env.Code != 0 {
		t.Fatalf("新 refresh 不可用: status=%d code=%d", rec.Code, env.Code)
	}
}

func TestRefreshRejectsAccessToken(t *testing.T) {
	h := authTestServer(t, authTestConfig("", 5), newFakeUserStore(), newFakeCache())
	reg := registerUser(t, h, testPhone, testPassword)

	rec, env := doJSON(t, h, http.MethodPost, "/api/v1/auth/refresh",
		map[string]string{"refresh_token": reg.AccessToken}, nil)
	if rec.Code != http.StatusUnauthorized || env.Code != 40101 {
		t.Fatalf("access 当 refresh 用: status=%d code=%d, want 401/40101", rec.Code, env.Code)
	}
}

func TestRefreshRejectsForgedToken(t *testing.T) {
	h := authTestServer(t, authTestConfig("", 5), newFakeUserStore(), newFakeCache())
	reg := registerUser(t, h, testPhone, testPassword)

	// 换掉签名的最后一个字符：结构合法、签名无效。
	forged := reg.RefreshToken[:len(reg.RefreshToken)-1] + "A"
	rec, env := doJSON(t, h, http.MethodPost, "/api/v1/auth/refresh",
		map[string]string{"refresh_token": forged}, nil)
	if rec.Code != http.StatusUnauthorized || env.Code != 40101 {
		t.Fatalf("伪造 refresh: status=%d code=%d, want 401/40101", rec.Code, env.Code)
	}
}

func TestMeReturnsMaskedPhoneOnly(t *testing.T) {
	h := authTestServer(t, authTestConfig("", 5), newFakeUserStore(), newFakeCache())
	reg := registerUser(t, h, testPhone, testPassword)

	rec, env := doJSON(t, h, http.MethodGet, "/api/v1/auth/me", nil, authHeader(reg.AccessToken))
	if rec.Code != http.StatusOK || env.Code != 0 {
		t.Fatalf("status=%d code=%d, want 200/0", rec.Code, env.Code)
	}
	if strings.Contains(rec.Body.String(), testPhone) {
		t.Fatalf("/auth/me 泄漏了手机号明文: %s", rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "138****8000") {
		t.Fatalf("/auth/me 未返回脱敏手机号: %s", rec.Body.String())
	}
}

func TestBadTokenIsNotRescuedByDevUser(t *testing.T) {
	// 配了 DevUser 也必须先看 Authorization：坏令牌不许回落成开发用户。
	h := authTestServer(t, authTestConfig("dev-user-0001", 5), newFakeUserStore(), newFakeCache())

	rec, env := doJSON(t, h, http.MethodGet, "/api/v1/auth/me", nil, authHeader("bad.token.x"))
	if rec.Code != http.StatusUnauthorized || env.Code != 40101 {
		t.Fatalf("坏令牌 + DevUser: status=%d code=%d, want 401/40101", rec.Code, env.Code)
	}

	// 完全不带 Authorization 时才走 DevUser（此处仍会 404：开发用户不在库里）。
	rec, env = doJSON(t, h, http.MethodGet, "/api/v1/auth/me", nil, nil)
	if rec.Code == http.StatusUnauthorized {
		t.Fatalf("无 Authorization 头时应当走 DevUser，实际 401: %+v", env)
	}
}

func TestRefreshTokenIsNotAcceptedAsAccess(t *testing.T) {
	h := authTestServer(t, authTestConfig("", 5), newFakeUserStore(), newFakeCache())
	reg := registerUser(t, h, testPhone, testPassword)

	rec, env := doJSON(t, h, http.MethodGet, "/api/v1/auth/me", nil, authHeader(reg.RefreshToken))
	if rec.Code != http.StatusUnauthorized || env.Code != 40101 {
		t.Fatalf("refresh 当 access 用: status=%d code=%d, want 401/40101", rec.Code, env.Code)
	}
}

func TestLogoutRevokesRefresh(t *testing.T) {
	h := authTestServer(t, authTestConfig("", 5), newFakeUserStore(), newFakeCache())
	reg := registerUser(t, h, testPhone, testPassword)

	rec, env := doJSON(t, h, http.MethodPost, "/api/v1/auth/logout", nil, authHeader(reg.AccessToken))
	if rec.Code != http.StatusOK || env.Code != 0 {
		t.Fatalf("登出失败: status=%d code=%d", rec.Code, env.Code)
	}

	rec, env = doJSON(t, h, http.MethodPost, "/api/v1/auth/refresh",
		map[string]string{"refresh_token": reg.RefreshToken}, nil)
	if rec.Code != http.StatusUnauthorized || env.Code != 40101 {
		t.Fatalf("登出后 refresh 仍可用: status=%d code=%d", rec.Code, env.Code)
	}
}

func TestAuthRejectsWhenNoStore(t *testing.T) {
	h := authTestServer(t, authTestConfig("", 5), nil, newFakeCache())

	rec, env := doJSON(t, h, http.MethodPost, "/api/v1/auth/register",
		map[string]string{"phone": testPhone, "password": testPassword}, nil)
	if rec.Code != http.StatusInternalServerError || env.Code != 50001 {
		t.Fatalf("无账号存储: status=%d code=%d, want 500/50001", rec.Code, env.Code)
	}
}

func TestRefreshFailsClosedWithoutCache(t *testing.T) {
	h := authTestServer(t, authTestConfig("", 5), newFakeUserStore(), newFakeCache())
	reg := registerUser(t, h, testPhone, testPassword)

	// 同一套 API 但去掉缓存：白名单无法校验，必须拒绝而不是放行。
	h2 := authTestServer(t, authTestConfig("", 5), newFakeUserStore(), nil)
	rec, env := doJSON(t, h2, http.MethodPost, "/api/v1/auth/refresh",
		map[string]string{"refresh_token": reg.RefreshToken}, nil)
	if rec.Code != http.StatusInternalServerError || env.Code != 50001 {
		t.Fatalf("无缓存时续期: status=%d code=%d, want 500/50001（失败关闭）", rec.Code, env.Code)
	}
}
