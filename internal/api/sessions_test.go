package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"laborlaw/internal/port"
)

// 本文件用内存假实现跑会话与消息两条链路。
//
// handler 的职责是"HTTP 进、port 调用出、错误折成业务码"，用假实现就能覆盖
// 全部错误分支；DAL 自己的 SQL 语义由 internal/store 的 TEST_MYSQL_DSN 用例负责。
// 两边都不重复，也都不遗漏。

var testSessionTime = time.Date(2026, 11, 7, 12, 0, 0, 0, time.UTC)

type fakeSessionStore struct {
	port.SessionStore

	sessions  map[string]port.Session
	listErr   error
	createErr error
	getErr    error
	deleteErr error

	created  *port.Session
	gotLimit int
	gotOff   int
	deleted  string
}

func newFakeSessions() *fakeSessionStore {
	return &fakeSessionStore{sessions: map[string]port.Session{}}
}

func (f *fakeSessionStore) Create(_ context.Context, sn *port.Session) error {
	if f.createErr != nil {
		return f.createErr
	}
	if sn.ID == "" {
		sn.ID = "sess-1"
	}
	sn.CreatedAt = testSessionTime
	sn.UpdatedAt = testSessionTime
	f.created = sn
	f.sessions[sn.ID] = *sn
	return nil
}

func (f *fakeSessionStore) Get(_ context.Context, _, id string) (*port.Session, error) {
	if f.getErr != nil {
		return nil, f.getErr
	}
	s, ok := f.sessions[id]
	if !ok {
		return nil, port.New(port.CodeNotFound, "会话不存在")
	}
	return &s, nil
}

func (f *fakeSessionStore) ListByUser(_ context.Context, _ string, limit, offset int) ([]port.Session, error) {
	f.gotLimit, f.gotOff = limit, offset
	if f.listErr != nil {
		return nil, f.listErr
	}
	return []port.Session{{
		ID:        "sess-1",
		UserID:    "dev-user-0001",
		Title:     "试用期能不能随便辞退",
		Scene:     "consult",
		CreatedAt: testSessionTime,
		UpdatedAt: testSessionTime,
	}}, nil
}

func (f *fakeSessionStore) Touch(_ context.Context, _, _ string) error { return nil }

func (f *fakeSessionStore) Delete(_ context.Context, _, id string) error {
	if f.deleteErr != nil {
		return f.deleteErr
	}
	if _, ok := f.sessions[id]; !ok {
		return port.New(port.CodeNotFound, "会话不存在")
	}
	f.deleted = id
	delete(f.sessions, id)
	return nil
}

type fakeMessageStore struct {
	port.MessageStore

	msgs     []port.ChatMessage
	err      error
	gotLimit int
	calls    int
}

func (f *fakeMessageStore) ListBySession(_ context.Context, _ string, limit int) ([]port.ChatMessage, error) {
	f.calls++
	f.gotLimit = limit
	if f.err != nil {
		return nil, f.err
	}
	return f.msgs, nil
}

func (f *fakeMessageStore) Append(_ context.Context, _ *port.ChatMessage) error { return nil }

// sessionFakeStore 只提供 Session() 与 Message()，其余域嵌入 nil。
type sessionFakeStore struct {
	port.Store
	sessions port.SessionStore
	messages port.MessageStore
}

func (f sessionFakeStore) Session() port.SessionStore { return f.sessions }
func (f sessionFakeStore) Message() port.MessageStore { return f.messages }

func newSessionAPI(t *testing.T, sessions *fakeSessionStore, messages *fakeMessageStore) http.Handler {
	t.Helper()
	return New(testConfig("dev-user-0001"), discardLogger(),
		WithStore(sessionFakeStore{sessions: sessions, messages: messages})).Handler()
}

// doSession 发一个请求并解出包络。
//
// 不复用 auth_test.go 的 doJSON：那个函数把 body 当 any 去 Marshal，
// 而这里的用例需要"原样发一串非法 JSON 看它怎么报错"，所以保留字符串入口。
func doSession(t *testing.T, h http.Handler, method, path, body string) (*httptest.ResponseRecorder, Envelope) {
	t.Helper()
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	var env Envelope
	if err := json.Unmarshal(rec.Body.Bytes(), &env); err != nil {
		t.Fatalf("%s %s 的响应不是 JSON: %v，body=%q", method, path, err, rec.Body.String())
	}
	return rec, env
}

func TestListSessions(t *testing.T) {
	h := newSessionAPI(t, newFakeSessions(), &fakeMessageStore{})

	rec, env := doSession(t, h, http.MethodGet, "/api/v1/sessions?limit=5&offset=10", "")

	if rec.Code != http.StatusOK || env.Code != int(port.CodeOK) {
		t.Fatalf("status=%d code=%d, want 200/0", rec.Code, env.Code)
	}
	raw, _ := json.Marshal(env.Data)
	var arr []map[string]any
	if err := json.Unmarshal(raw, &arr); err != nil {
		t.Fatalf("data 不是数组: %v, raw=%s", err, raw)
	}
	if len(arr) != 1 {
		t.Fatalf("len(data) = %d, want 1", len(arr))
	}
	if arr[0]["scene"] != "consult" {
		t.Fatalf("scene = %v, want consult", arr[0]["scene"])
	}
	if _, ok := arr[0]["created_at"]; !ok {
		t.Fatalf("响应缺少 created_at: %v", arr[0])
	}
}

func TestListSessionsRejectsBadLimit(t *testing.T) {
	h := newSessionAPI(t, newFakeSessions(), &fakeMessageStore{})

	rec, env := doSession(t, h, http.MethodGet, "/api/v1/sessions?limit=101", "")

	if rec.Code != http.StatusBadRequest || env.Code != int(port.CodeInvalidArgument) {
		t.Fatalf("status=%d code=%d, want 400/40001", rec.Code, env.Code)
	}
}

func TestCreateSession(t *testing.T) {
	sessions := newFakeSessions()
	h := newSessionAPI(t, sessions, &fakeMessageStore{})

	rec, env := doSession(t, h, http.MethodPost, "/api/v1/sessions", `{"scene":"consult","title":"试用期辞退"}`)

	if rec.Code != http.StatusOK || env.Code != int(port.CodeOK) {
		t.Fatalf("status=%d code=%d body=%s", rec.Code, env.Code, rec.Body.String())
	}
	if sessions.created == nil {
		t.Fatal("Create 没有被调用")
	}
	if sessions.created.Scene != "consult" || sessions.created.Title != "试用期辞退" {
		t.Fatalf("Create 收到 %+v", *sessions.created)
	}
	d := dataMap(t, env)
	if d["created_at"] == "" || d["created_at"] == nil {
		t.Fatalf("响应里的 created_at 是空值: %v", d)
	}
}

func TestCreateSessionValidates(t *testing.T) {
	cases := []struct {
		name string
		body string
	}{
		{"缺 scene", `{"title":"x"}`},
		{"非法 scene", `{"scene":"chat"}`},
		{"标题超长", `{"scene":"consult","title":"` + strings.Repeat("劳", 101) + `"}`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h := newSessionAPI(t, newFakeSessions(), &fakeMessageStore{})
			rec, env := doSession(t, h, http.MethodPost, "/api/v1/sessions", tc.body)
			if rec.Code != http.StatusBadRequest || env.Code != int(port.CodeInvalidArgument) {
				t.Fatalf("status=%d code=%d, want 400/40001", rec.Code, env.Code)
			}
		})
	}
}

func TestGetSessionNotFound(t *testing.T) {
	h := newSessionAPI(t, newFakeSessions(), &fakeMessageStore{})

	rec, env := doSession(t, h, http.MethodGet, "/api/v1/sessions/missing", "")

	if rec.Code != http.StatusNotFound || env.Code != int(port.CodeNotFound) {
		t.Fatalf("status=%d code=%d, want 404/40401", rec.Code, env.Code)
	}
}

func TestDeleteSession(t *testing.T) {
	sessions := newFakeSessions()
	sessions.sessions["sess-1"] = port.Session{ID: "sess-1", Title: "旧会话"}
	h := newSessionAPI(t, sessions, &fakeMessageStore{})

	rec, env := doSession(t, h, http.MethodDelete, "/api/v1/sessions/sess-1", "")

	if rec.Code != http.StatusOK || env.Code != int(port.CodeOK) {
		t.Fatalf("status=%d code=%d", rec.Code, env.Code)
	}
	if sessions.deleted != "sess-1" {
		t.Fatalf("Delete 收到 %q, want sess-1", sessions.deleted)
	}
}

func TestDeleteMissingSession(t *testing.T) {
	h := newSessionAPI(t, newFakeSessions(), &fakeMessageStore{})

	rec, env := doSession(t, h, http.MethodDelete, "/api/v1/sessions/nope", "")

	if rec.Code != http.StatusNotFound || env.Code != int(port.CodeNotFound) {
		t.Fatalf("status=%d code=%d, want 404/40401", rec.Code, env.Code)
	}
}

func TestListMessagesDefaultLimit(t *testing.T) {
	sessions := newFakeSessions()
	sessions.sessions["sess-1"] = port.Session{ID: "sess-1"}
	msgs := &fakeMessageStore{msgs: []port.ChatMessage{
		{ID: "m1", SessionID: "sess-1", Role: port.RoleUser, Content: "公司不给我交社保", CreatedAt: testSessionTime},
		{ID: "m2", SessionID: "sess-1", Role: port.RoleAssistant, Content: "可以据此解除合同", CreatedAt: testSessionTime},
	}}
	h := newSessionAPI(t, sessions, msgs)

	rec, env := doSession(t, h, http.MethodGet, "/api/v1/sessions/sess-1/messages", "")

	if rec.Code != http.StatusOK || env.Code != int(port.CodeOK) {
		t.Fatalf("status=%d code=%d", rec.Code, env.Code)
	}
	if msgs.gotLimit != 50 {
		t.Fatalf("默认 limit = %d, want 50", msgs.gotLimit)
	}
	raw, _ := json.Marshal(env.Data)
	var arr []map[string]any
	if err := json.Unmarshal(raw, &arr); err != nil {
		t.Fatalf("data 不是数组: %v", err)
	}
	if len(arr) != 2 {
		t.Fatalf("len(data) = %d, want 2", len(arr))
	}
	if arr[0]["role"] != "user" {
		t.Fatalf("role = %v, want user", arr[0]["role"])
	}
	// 计量与排障字段不该出网。
	for _, k := range []string{"prompt_tokens", "completion_tokens", "trace_id"} {
		if _, ok := arr[0][k]; ok {
			t.Fatalf("响应里不该出现 %s: %v", k, arr[0])
		}
	}
}

func TestListMessagesLimitBounds(t *testing.T) {
	sessions := newFakeSessions()
	sessions.sessions["sess-1"] = port.Session{ID: "sess-1"}
	msgs := &fakeMessageStore{}
	h := newSessionAPI(t, sessions, msgs)

	rec, env := doSession(t, h, http.MethodGet, "/api/v1/sessions/sess-1/messages?limit=201", "")
	if rec.Code != http.StatusBadRequest || env.Code != int(port.CodeInvalidArgument) {
		t.Fatalf("limit=201 应 400/40001，得到 %d/%d", rec.Code, env.Code)
	}
	if msgs.calls != 0 {
		t.Fatalf("参数非法时不该访问存储，calls=%d", msgs.calls)
	}

	rec, env = doSession(t, h, http.MethodGet, "/api/v1/sessions/sess-1/messages?limit=200", "")
	if rec.Code != http.StatusOK || env.Code != int(port.CodeOK) {
		t.Fatalf("limit=200 应 200/0，得到 %d/%d", rec.Code, env.Code)
	}
	if msgs.gotLimit != 200 {
		t.Fatalf("limit = %d, want 200", msgs.gotLimit)
	}
}

// TestListMessagesChecksOwnership 守住"读消息前必须验会话归属"。
//
// 缺陷形态：直接按 session_id 查 messages，不校验会话属于谁。
// 后果是任何登录用户换一个 session ID 就能读到别人的咨询记录——
// 合同与工资这类内容泄漏一次就够严重。
func TestListMessagesChecksOwnership(t *testing.T) {
	sessions := newFakeSessions()
	msgs := &fakeMessageStore{}
	h := newSessionAPI(t, sessions, msgs)

	rec, env := doSession(t, h, http.MethodGet, "/api/v1/sessions/other-user-session/messages", "")

	if rec.Code != http.StatusNotFound || env.Code != int(port.CodeNotFound) {
		t.Fatalf("status=%d code=%d, want 404/40401", rec.Code, env.Code)
	}
	if msgs.calls != 0 {
		t.Fatalf("会话不属于当前用户时不该读消息，calls=%d", msgs.calls)
	}
}
