package api

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gin-gonic/gin"

	"laborlaw/internal/port"
)

// 本文件用内存假实现跑审查与任务两条链路。为什么不用真实 MySQL：
// handler 的职责是把 HTTP 翻译成 port 调用、把 port 错误翻译成业务码，
// 这一段用假实现能覆盖全部错误分支，而真库只能覆盖"happy path + 一条约束"。
// DAL 自己的语义由 internal/store 的 TEST_MYSQL_DSN 用例负责。

// ---------- 假 FileStore ----------

type fakeReviewFileStore struct {
	port.FileStore
	file    *port.FileRecord
	getErr  error
	loadErr error
	// getCalls 记 Get 被调了几次。列表补文件名必须按 file_id 去重，
	// 「同一个文件的多条审查记录只查一次库」只能靠计数断言。
	mu       sync.Mutex
	getCalls int
}

func (f *fakeReviewFileStore) Get(_ context.Context, _, _ string) (*port.FileRecord, error) {
	f.mu.Lock()
	f.getCalls++
	f.mu.Unlock()
	if f.getErr != nil {
		return nil, f.getErr
	}
	return f.file, nil
}

// calls 读调用次数，给断言用。
func (f *fakeReviewFileStore) calls() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.getCalls
}

func (f *fakeReviewFileStore) LoadPages(_ context.Context, _ string) ([]port.ParsedPage, error) {
	if f.loadErr != nil {
		return nil, f.loadErr
	}
	return []port.ParsedPage{{Index: 0, Width: 595, Height: 842}}, nil
}

// ---------- 假 ReviewStore ----------

type fakeReviewStore struct {
	mu           sync.Mutex
	reviews      []port.Review
	findings     map[string][]port.Finding
	createErr    error
	getErr       error
	getByFileErr error
	// getByFileMissOnce 只让第一次 GetByFile 返回它，用来模拟并发场景：
	// 本请求查重时另一方还没落库，落库时才撞上唯一键。
	getByFileMissOnce error
	getByFileCalls    int
	listByUserCalls   int
	listErr           error
	findErr           error
	saved             int
	updated           []port.Review
}

func newFakeReviewStore() *fakeReviewStore {
	return &fakeReviewStore{findings: map[string][]port.Finding{}}
}

func (f *fakeReviewStore) Create(_ context.Context, rv *port.Review) error {
	if f.createErr != nil {
		return f.createErr
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if rv.CreatedAt.IsZero() {
		rv.CreatedAt = time.Now().UTC()
	}
	rv.UpdatedAt = rv.CreatedAt
	f.reviews = append(f.reviews, *rv)
	return nil
}

func (f *fakeReviewStore) Get(_ context.Context, userID, id string) (*port.Review, error) {
	if f.getErr != nil {
		return nil, f.getErr
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	for i := range f.reviews {
		if f.reviews[i].ID == id && (userID == "" || f.reviews[i].UserID == userID) {
			rv := f.reviews[i]
			return &rv, nil
		}
	}
	return nil, port.New(port.CodeNotFound, "审查记录不存在")
}

func (f *fakeReviewStore) GetByFile(_ context.Context, userID, fileID string) (*port.Review, error) {
	if f.getByFileErr != nil {
		return nil, f.getByFileErr
	}
	f.mu.Lock()
	f.getByFileCalls++
	first := f.getByFileCalls == 1
	f.mu.Unlock()
	if first && f.getByFileMissOnce != nil {
		return nil, f.getByFileMissOnce
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	for i := len(f.reviews) - 1; i >= 0; i-- {
		if f.reviews[i].UserID == userID && f.reviews[i].FileID == fileID {
			rv := f.reviews[i]
			return &rv, nil
		}
	}
	return nil, port.New(port.CodeNotFound, "审查记录不存在")
}

func (f *fakeReviewStore) Update(_ context.Context, rv *port.Review) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	for i := range f.reviews {
		if f.reviews[i].ID == rv.ID {
			rv.UpdatedAt = time.Now().UTC()
			f.reviews[i] = *rv
			f.updated = append(f.updated, *rv)
			return nil
		}
	}
	return port.New(port.CodeNotFound, "审查记录不存在")
}

func (f *fakeReviewStore) SaveFindings(_ context.Context, reviewID string, items []port.Finding) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.saved++
	f.findings[reviewID] = append([]port.Finding(nil), items...)
	return nil
}

func (f *fakeReviewStore) ListFindings(_ context.Context, reviewID string) ([]port.Finding, error) {
	if f.findErr != nil {
		return nil, f.findErr
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]port.Finding{}, f.findings[reviewID]...), nil
}

func (f *fakeReviewStore) ListByUser(_ context.Context, userID string, limit, offset int) ([]port.Review, error) {
	if f.listErr != nil {
		return nil, f.listErr
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.listByUserCalls++
	out := []port.Review{}
	for i := len(f.reviews) - 1; i >= 0; i-- {
		if f.reviews[i].UserID == userID {
			out = append(out, f.reviews[i])
		}
	}
	if offset >= len(out) {
		return []port.Review{}, nil
	}
	out = out[offset:]
	if limit > 0 && limit < len(out) {
		out = out[:limit]
	}
	return out, nil
}

// ---------- 假 TaskStore ----------

type fakeTaskStore struct {
	mu    sync.Mutex
	tasks map[string]*port.Task
	err   error
}

func newFakeTaskStore() *fakeTaskStore {
	return &fakeTaskStore{tasks: map[string]*port.Task{}}
}

func (f *fakeTaskStore) Create(_ context.Context, t *port.Task) error {
	if f.err != nil {
		return f.err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	cp := *t
	f.tasks[t.ID] = &cp
	return nil
}

func (f *fakeTaskStore) Get(_ context.Context, id string) (*port.Task, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	t, ok := f.tasks[id]
	if !ok {
		return nil, port.New(port.CodeNotFound, "任务不存在")
	}
	cp := *t
	return &cp, nil
}

func (f *fakeTaskStore) UpdateProgress(_ context.Context, id string, percent int, stage string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	t, ok := f.tasks[id]
	if !ok {
		return port.New(port.CodeNotFound, "任务不存在")
	}
	if percent < 0 {
		percent = 0
	}
	if percent > 100 {
		percent = 100
	}
	t.Progress = percent
	t.Stage = stage
	if t.Status == port.TaskPending {
		t.Status = port.TaskRunning
	}
	return nil
}

func (f *fakeTaskStore) Finish(_ context.Context, id string, status port.TaskStatus, message string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	t, ok := f.tasks[id]
	if !ok {
		return port.New(port.CodeNotFound, "任务不存在")
	}
	if !t.Status.CanTransition(status) {
		return port.New(port.CodeConflict, "任务状态不能从 %s 变为 %s", t.Status, status)
	}
	t.Status = status
	t.Error = message
	return nil
}

func (f *fakeTaskStore) ListByStatus(_ context.Context, _ port.TaskStatus, _ int) ([]port.Task, error) {
	return []port.Task{}, nil
}

func (f *fakeTaskStore) FailRunning(_ context.Context, _ string) (int64, error) {
	return 0, nil
}

// ---------- 假 Store 与 submitter ----------

// reviewFakeStore 只实现 handler 用得到的三域；其余走内嵌 nil 接口，调了就 panic。
// 这是刻意的：多实现一个域会让"这行代码到底用了什么"变得不可见。
type reviewFakeStore struct {
	port.Store
	files   *fakeReviewFileStore
	reviews *fakeReviewStore
	tasks   *fakeTaskStore
}

func (s *reviewFakeStore) File() port.FileStore     { return s.files }
func (s *reviewFakeStore) Review() port.ReviewStore { return s.reviews }
func (s *reviewFakeStore) Task() port.TaskStore     { return s.tasks }

type fakeSubmitter struct {
	mu       sync.Mutex
	tasks    []*port.Task
	handlers []port.Handler
	err      error
}

func (s *fakeSubmitter) Submit(_ context.Context, t *port.Task, h port.Handler) error {
	if s.err != nil {
		return s.err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.tasks = append(s.tasks, t)
	s.handlers = append(s.handlers, h)
	return nil
}

// ---------- 脚手架 ----------

type reviewFixture struct {
	api     http.Handler
	store   *reviewFakeStore
	submit  *fakeSubmitter
	handler port.Handler
}

func newReviewFixture(t *testing.T) *reviewFixture {
	t.Helper()
	gin.SetMode(gin.TestMode)
	store := &reviewFakeStore{
		files: &fakeReviewFileStore{
			file: &port.FileRecord{
				ID: "F-1", UserID: "dev-user-0001", PageCount: 1,
				Filename: "劳动合同.pdf",
			},
		},
		reviews: newFakeReviewStore(),
		tasks:   newFakeTaskStore(),
	}
	sub := &fakeSubmitter{}
	h := New(testConfig("dev-user-0001"), discardLogger(),
		WithStore(store), WithTaskSubmitter(sub), WithReviewHandler(okHandler),
	).Handler()
	return &reviewFixture{api: h, store: store, submit: sub, handler: okHandler}
}

func okHandler(_ context.Context, _ json.RawMessage, progress func(int, string)) error {
	progress(100, "审查完成")
	return nil
}

func postJSON(t *testing.T, h http.Handler, path, body string) (*httptest.ResponseRecorder, Envelope) {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, path, bytes.NewBufferString(body))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	var env Envelope
	if err := json.Unmarshal(rec.Body.Bytes(), &env); err != nil {
		t.Fatalf("响应不是 JSON: %v，body=%q", err, rec.Body.String())
	}
	return rec, env
}

func dataMap(t *testing.T, env Envelope) map[string]any {
	t.Helper()
	raw, err := json.Marshal(env.Data)
	if err != nil {
		t.Fatalf("data 无法再序列化: %v", err)
	}
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatalf("data 不是对象: %v，data=%s", err, raw)
	}
	return m
}

// ---------- 创建审查 ----------

func TestCreateReviewAccepted(t *testing.T) {
	f := newReviewFixture(t)

	rec, env := postJSON(t, f.api, "/api/v1/contracts/review",
		`{"file_id":"F-1","rule_scope":["R-DEPOSIT"],"async":true}`)

	if rec.Code != http.StatusAccepted {
		t.Fatalf("状态码 = %d，期望 202，body=%s", rec.Code, rec.Body.String())
	}
	if env.Code != 0 || env.Message != "accepted" {
		t.Fatalf("包络 = %+v，期望 code=0 message=accepted", env)
	}
	data := dataMap(t, env)
	taskID, _ := data["task_id"].(string)
	reviewID, _ := data["review_id"].(string)
	if taskID == "" || reviewID == "" {
		t.Fatalf("task_id / review_id 为空: %v", data)
	}
	if data["status"] != "pending" {
		t.Fatalf("status = %v，期望 pending", data["status"])
	}

	if len(f.submit.tasks) != 1 {
		t.Fatalf("入池任务数 = %d，期望 1", len(f.submit.tasks))
	}
	got := f.submit.tasks[0]
	if got.Type != "review.run" || got.ID != taskID {
		t.Fatalf("入池任务 = %+v，期望 id=%s type=review.run", got, taskID)
	}
	// payload 必须能让编排层原样解出 review_id / file_id / user_id / rule_scope。
	var payload struct {
		ReviewID  string   `json:"review_id"`
		FileID    string   `json:"file_id"`
		UserID    string   `json:"user_id"`
		RuleScope []string `json:"rule_scope"`
	}
	if err := json.Unmarshal(got.Payload, &payload); err != nil {
		t.Fatalf("payload 不是 JSON: %v，raw=%s", err, got.Payload)
	}
	if payload.ReviewID != reviewID || payload.FileID != "F-1" || payload.UserID != "dev-user-0001" {
		t.Fatalf("payload 字段不对: %+v", payload)
	}
	if len(payload.RuleScope) != 1 || payload.RuleScope[0] != "R-DEPOSIT" {
		t.Fatalf("payload.rule_scope = %v", payload.RuleScope)
	}

	// 审查主记录也要真的落到 store 里，否则前端拿 review_id 查不到东西。
	rv, err := f.store.reviews.Get(context.Background(), "dev-user-0001", reviewID)
	if err != nil {
		t.Fatalf("审查记录没落库: %v", err)
	}
	if rv.TaskID != taskID || rv.Status != port.TaskPending {
		t.Fatalf("审查记录 = %+v", rv)
	}
}

func TestCreateReviewReusesRunning(t *testing.T) {
	f := newReviewFixture(t)
	ctx := context.Background()
	_ = f.store.tasks.Create(ctx, &port.Task{ID: "T-OLD", Type: "review.run", Status: port.TaskRunning})
	_ = f.store.reviews.Create(ctx, &port.Review{
		ID: "R-OLD", FileID: "F-1", UserID: "dev-user-0001",
		Status: port.TaskRunning, TaskID: "T-OLD",
	})

	rec, env := postJSON(t, f.api, "/api/v1/contracts/review", `{"file_id":"F-1"}`)

	if rec.Code != http.StatusAccepted {
		t.Fatalf("状态码 = %d，期望 202", rec.Code)
	}
	data := dataMap(t, env)
	if data["review_id"] != "R-OLD" || data["task_id"] != "T-OLD" {
		t.Fatalf("重复审查没有复用旧记录: %v", data)
	}
	if len(f.submit.tasks) != 0 {
		t.Fatalf("重复审查不该再次入池，实际入池 %d 个", len(f.submit.tasks))
	}
}

// TestCreateReviewReopensAfterDeadTask 覆盖"任务已死、审查还挂着 pending"的孤儿状态。
//
// 真实来源：worker 超时或进程重启时，任务被标 failed，而 review 停在 pending。
// 这时候如果只看 review.status 就复用旧 task_id，用户会拿到一个永远不前进的任务号。
func TestCreateReviewReopensAfterDeadTask(t *testing.T) {
	f := newReviewFixture(t)
	ctx := context.Background()
	_ = f.store.tasks.Create(ctx, &port.Task{ID: "T-DEAD", Type: "review.run", Status: port.TaskFailed})
	_ = f.store.reviews.Create(ctx, &port.Review{
		ID: "R-STALE", FileID: "F-1", UserID: "dev-user-0001",
		Status: port.TaskPending, TaskID: "T-DEAD",
	})

	rec, env := postJSON(t, f.api, "/api/v1/contracts/review", `{"file_id":"F-1"}`)

	if rec.Code != http.StatusAccepted {
		t.Fatalf("状态码 = %d，期望 202", rec.Code)
	}
	data := dataMap(t, env)
	if data["task_id"] == "T-DEAD" || data["review_id"] == "R-STALE" {
		t.Fatalf("孤儿任务被复用了: %v", data)
	}
	if len(f.submit.tasks) != 1 {
		t.Fatalf("重开审查应当重新入池，实际 %d 个", len(f.submit.tasks))
	}
	// 假 Store 的 Update 是整条替换（真库里 UPDATE 只覆盖可变列、user_id 不动），
	// 所以按 id 取，不带 user 过滤。
	stale, err := f.store.reviews.Get(ctx, "", "R-STALE")
	if err != nil {
		t.Fatalf("读旧审查: %v", err)
	}
	if stale.Status != port.TaskFailed {
		t.Fatalf("旧审查状态 = %s，期望 failed", stale.Status)
	}
}

func TestCreateReviewReusesSucceeded(t *testing.T) {
	f := newReviewFixture(t)
	ctx := context.Background()
	_ = f.store.reviews.Create(ctx, &port.Review{
		ID: "R-DONE", FileID: "F-1", UserID: "dev-user-0001",
		Status: port.TaskSucceeded, RiskLevel: port.SeverityHigh, Score: 40,
	})

	rec, env := postJSON(t, f.api, "/api/v1/contracts/review", `{"file_id":"F-1"}`)

	if rec.Code != http.StatusOK {
		t.Fatalf("状态码 = %d，期望 200（已成功的审查直接回结果）", rec.Code)
	}
	data := dataMap(t, env)
	if data["id"] != "R-DONE" {
		t.Fatalf("返回的审查 id = %v，期望 R-DONE", data["id"])
	}
	if data["score"].(float64) != 40 {
		t.Fatalf("score = %v", data["score"])
	}
}

func TestCreateReviewFileNotParsed(t *testing.T) {
	f := newReviewFixture(t)
	f.store.files.loadErr = port.New(port.CodeNotFound, "文件尚未解析")

	rec, env := postJSON(t, f.api, "/api/v1/contracts/review", `{"file_id":"F-1"}`)

	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("状态码 = %d，期望 422", rec.Code)
	}
	if env.Code != int(port.CodeNoTextLayer) {
		t.Fatalf("业务码 = %d，期望 %d", env.Code, port.CodeNoTextLayer)
	}
	if len(f.submit.tasks) != 0 {
		t.Fatal("未解析的合同不该产生任务")
	}
}

func TestCreateReviewRejectsOtherUsersFile(t *testing.T) {
	f := newReviewFixture(t)
	f.store.files.getErr = port.New(port.CodeNotFound, "文件不存在")

	rec, env := postJSON(t, f.api, "/api/v1/contracts/review", `{"file_id":"F-OTHER"}`)

	// 别人的文件必须表现为 404：403 本身就是一条信息（"这个 id 存在"）。
	if rec.Code != http.StatusNotFound || env.Code != int(port.CodeNotFound) {
		t.Fatalf("状态码/业务码 = %d/%d，期望 404/40401", rec.Code, env.Code)
	}
}

func TestCreateReviewRejectsBadBody(t *testing.T) {
	f := newReviewFixture(t)
	for _, body := range []string{`{`, `{"file_id":""}`, `{"file_id":"F-1","rule_scope":["a","b","c","d","e","f","g","h","i","j","k","l","m","n","o","p","q","r","s","t","u","v","w","x","y","z","aa","bb","cc","dd","ee","ff","gg"]}`} {
		rec, env := postJSON(t, f.api, "/api/v1/contracts/review", body)
		if rec.Code != http.StatusBadRequest || env.Code != int(port.CodeInvalidArgument) {
			t.Fatalf("body=%s 得到 %d/%d，期望 400/40001", body, rec.Code, env.Code)
		}
	}
}

func TestCreateReviewWithoutDeps(t *testing.T) {
	gin.SetMode(gin.TestMode)
	h := New(testConfig("dev-user-0001"), discardLogger(),
		WithStore(&reviewFakeStore{files: &fakeReviewFileStore{}, reviews: newFakeReviewStore(), tasks: newFakeTaskStore()}),
	).Handler()

	rec, env := postJSON(t, h, "/api/v1/contracts/review", `{"file_id":"F-1"}`)
	if rec.Code != http.StatusNotImplemented || env.Code != int(port.CodeInternal) {
		t.Fatalf("状态码/业务码 = %d/%d，期望 501/50001", rec.Code, env.Code)
	}
}

// ---------- 查询 ----------

func TestListReviewsPagination(t *testing.T) {
	f := newReviewFixture(t)
	ctx := context.Background()
	for i := 0; i < 3; i++ {
		_ = f.store.reviews.Create(ctx, &port.Review{
			ID:     "R-" + string(rune('A'+i)),
			FileID: "F-1", UserID: "dev-user-0001", Status: port.TaskSucceeded,
		})
	}

	rec, env := doRequest(t, f.api, http.MethodGet, "/api/v1/reviews?limit=2&offset=0", nil)
	if rec.Code != http.StatusOK || env.Code != 0 {
		t.Fatalf("状态码/业务码 = %d/%d", rec.Code, env.Code)
	}
	list, ok := env.Data.([]any)
	if !ok || len(list) != 2 {
		t.Fatalf("data = %#v，期望 2 条", env.Data)
	}

	rec, env = doRequest(t, f.api, http.MethodGet, "/api/v1/reviews?limit=0", nil)
	if rec.Code != http.StatusBadRequest || env.Code != int(port.CodeInvalidArgument) {
		t.Fatalf("limit=0 得到 %d/%d，期望 400/40001", rec.Code, env.Code)
	}
}

// file_id 命中：列表只回那一条。前端进 /review/{fileId} 就靠这个恢复结果，
// 否则用户每次回来都得再等一次审查（复用逻辑会 2 秒返回旧结果，但界面先空一次）。
func TestListReviewsFiltersByFile(t *testing.T) {
	f := newReviewFixture(t)
	ctx := context.Background()
	_ = f.store.reviews.Create(ctx, &port.Review{
		ID: "R-OTHER", FileID: "F-OTHER", UserID: "dev-user-0001", Status: port.TaskSucceeded,
	})
	_ = f.store.reviews.Create(ctx, &port.Review{
		ID: "R-WANTED", FileID: "F-1", UserID: "dev-user-0001", Status: port.TaskSucceeded, Score: 65,
	})

	rec, env := doRequest(t, f.api, http.MethodGet, "/api/v1/reviews?file_id=F-1", nil)
	if rec.Code != http.StatusOK || env.Code != 0 {
		t.Fatalf("状态码/业务码 = %d/%d，期望 200/0", rec.Code, env.Code)
	}
	list, ok := env.Data.([]any)
	if !ok {
		t.Fatalf("data 不是数组：%#v", env.Data)
	}
	if len(list) != 1 {
		t.Fatalf("data 长度 = %d，期望 1", len(list))
	}
	item, _ := list[0].(map[string]any)
	if item["id"] != "R-WANTED" {
		t.Fatalf("data[0].id = %v，期望 R-WANTED", item["id"])
	}
}

// 该文件没审过：列表语义下"没有"就是空列表，不是 404。
// 回 40401 会把前端的"空态 + 开始审查"变成错误提示。
func TestListReviewsFileNotFoundReturnsEmptyList(t *testing.T) {
	f := newReviewFixture(t)

	rec, env := doRequest(t, f.api, http.MethodGet, "/api/v1/reviews?file_id=NOPE-1", nil)
	if rec.Code != http.StatusOK || env.Code != 0 {
		t.Fatalf("状态码/业务码 = %d/%d，期望 200/0（不是 40401）", rec.Code, env.Code)
	}
	list, ok := env.Data.([]any)
	if !ok {
		t.Fatalf("data 不是数组：%#v（空列表也要是 []，不能是 null）", env.Data)
	}
	if len(list) != 0 {
		t.Fatalf("data 长度 = %d，期望 0", len(list))
	}
}

// 不带 file_id 时行为必须一字不变：仍走 ListByUser 分页。
func TestListReviewsWithoutFileUsesListByUser(t *testing.T) {
	f := newReviewFixture(t)
	ctx := context.Background()
	_ = f.store.reviews.Create(ctx, &port.Review{
		ID: "R-1", FileID: "F-1", UserID: "dev-user-0001", Status: port.TaskSucceeded,
	})

	if rec, env := doRequest(t, f.api, http.MethodGet, "/api/v1/reviews", nil); rec.Code != http.StatusOK || env.Code != 0 {
		t.Fatalf("状态码/业务码 = %d/%d", rec.Code, env.Code)
	}
	f.store.reviews.mu.Lock()
	listed, byFile := f.store.reviews.listByUserCalls, f.store.reviews.getByFileCalls
	f.store.reviews.mu.Unlock()
	if listed != 1 || byFile != 0 {
		t.Fatalf("ListByUser/GetByFile 调用次数 = %d/%d，期望 1/0", listed, byFile)
	}

	if rec, env := doRequest(t, f.api, http.MethodGet, "/api/v1/reviews?file_id=F-1", nil); rec.Code != http.StatusOK || env.Code != 0 {
		t.Fatalf("带 file_id 时状态码/业务码 = %d/%d", rec.Code, env.Code)
	}
	f.store.reviews.mu.Lock()
	listed, byFile = f.store.reviews.listByUserCalls, f.store.reviews.getByFileCalls
	f.store.reviews.mu.Unlock()
	if listed != 1 || byFile != 1 {
		t.Fatalf("带 file_id 后 ListByUser/GetByFile 调用次数 = %d/%d，期望 1/1（不该再走分页）", listed, byFile)
	}
}

// 列表必须带出文件名：没有它，前端只能显示 file_id 尾号，用户认不出是哪份合同。
//
// 两条记录指向同一个文件是真实场景（失败重试、取消后重发起都会留下旧行），
// 所以除了断言名字正确，还要断言只查了一次库——去重漏掉的话 Twenty 条一页
// 就会对同一个主键连查二十遍，而且这个退化管理端完全看不出来。
func TestListReviewsIncludesFilename(t *testing.T) {
	f := newReviewFixture(t)
	ctx := context.Background()
	for _, id := range []string{"R-OLD", "R-NEW"} {
		_ = f.store.reviews.Create(ctx, &port.Review{
			ID: id, FileID: "F-1", UserID: "dev-user-0001", Status: port.TaskSucceeded,
		})
	}
	before := f.store.files.calls()

	rec, env := doRequest(t, f.api, http.MethodGet, "/api/v1/reviews", nil)
	if rec.Code != http.StatusOK || env.Code != 0 {
		t.Fatalf("状态码/业务码 = %d/%d", rec.Code, env.Code)
	}
	list, ok := env.Data.([]any)
	if !ok || len(list) != 2 {
		t.Fatalf("data = %#v，期望 2 条", env.Data)
	}
	for i, raw := range list {
		item, _ := raw.(map[string]any)
		if item["filename"] != "劳动合同.pdf" {
			t.Fatalf("data[%d].filename = %v，期望 劳动合同.pdf", i, item["filename"])
		}
	}

	// Create 链路不碰 Files().Get，所以这里就是列表补名字产生的调用数。
	if got := f.store.files.calls() - before; got != 1 {
		t.Fatalf("Files().Get 调用 %d 次，期望 1 次（同一 file_id 必须去重）", got)
	}
}

// 文件名补不到时降级成空串，列表照常 200。
// 为了一条展示字段把整页变成 500，用户会连"有哪几份合同"都看不到。
func TestListReviewsFilenameFailureDegrades(t *testing.T) {
	f := newReviewFixture(t)
	ctx := context.Background()
	_ = f.store.reviews.Create(ctx, &port.Review{
		ID: "R-1", FileID: "F-1", UserID: "dev-user-0001", Status: port.TaskSucceeded,
	})
	f.store.files.getErr = port.New(port.CodeInternal, "读文件失败")

	rec, env := doRequest(t, f.api, http.MethodGet, "/api/v1/reviews", nil)
	if rec.Code != http.StatusOK || env.Code != 0 {
		t.Fatalf("状态码/业务码 = %d/%d，期望 200/0", rec.Code, env.Code)
	}
	list, ok := env.Data.([]any)
	if !ok || len(list) != 1 {
		t.Fatalf("data = %#v，期望 1 条", env.Data)
	}
	item, _ := list[0].(map[string]any)
	if item["filename"] != "" {
		t.Fatalf("data[0].filename = %v，期望空串", item["filename"])
	}
}

func TestGetReviewNotFound(t *testing.T) {
	f := newReviewFixture(t)
	rec, env := doRequest(t, f.api, http.MethodGet, "/api/v1/reviews/NOPE", nil)
	if rec.Code != http.StatusNotFound || env.Code != int(port.CodeNotFound) {
		t.Fatalf("得到 %d/%d，期望 404/40401", rec.Code, env.Code)
	}
}

func TestListFindingsEmptyIsArray(t *testing.T) {
	f := newReviewFixture(t)
	ctx := context.Background()
	_ = f.store.reviews.Create(ctx, &port.Review{ID: "R-1", FileID: "F-1", UserID: "dev-user-0001", Status: port.TaskSucceeded})

	rec, env := doRequest(t, f.api, http.MethodGet, "/api/v1/reviews/R-1/findings", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("状态码 = %d", rec.Code)
	}
	raw, _ := json.Marshal(env.Data)
	if string(raw) != "[]" {
		t.Fatalf("空 findings 序列化成 %s，期望 []（null 会让前端 .map 崩）", raw)
	}
}

func TestListFindingsFiltersSeverity(t *testing.T) {
	f := newReviewFixture(t)
	ctx := context.Background()
	_ = f.store.reviews.Create(ctx, &port.Review{ID: "R-1", FileID: "F-1", UserID: "dev-user-0001", Status: port.TaskSucceeded})
	f.store.reviews.findings["R-1"] = []port.Finding{
		{ID: "F-001", ReviewID: "R-1", Severity: port.SeverityHigh},
		{ID: "F-002", ReviewID: "R-1", Severity: port.SeverityMedium},
	}

	rec, env := doRequest(t, f.api, http.MethodGet, "/api/v1/reviews/R-1/findings?severity=high", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("状态码 = %d", rec.Code)
	}
	list := env.Data.([]any)
	if len(list) != 1 || list[0].(map[string]any)["id"] != "F-001" {
		t.Fatalf("severity 过滤结果 = %#v", list)
	}

	rec, env = doRequest(t, f.api, http.MethodGet, "/api/v1/reviews/R-1/findings?severity=urgent", nil)
	if rec.Code != http.StatusBadRequest || env.Code != int(port.CodeInvalidArgument) {
		t.Fatalf("非法 severity 得到 %d/%d", rec.Code, env.Code)
	}
}

func TestListFindingsOtherUsersReview(t *testing.T) {
	f := newReviewFixture(t)
	ctx := context.Background()
	_ = f.store.reviews.Create(ctx, &port.Review{ID: "R-2", FileID: "F-9", UserID: "someone-else", Status: port.TaskSucceeded})

	rec, env := doRequest(t, f.api, http.MethodGet, "/api/v1/reviews/R-2/findings", nil)
	if rec.Code != http.StatusNotFound || env.Code != int(port.CodeNotFound) {
		t.Fatalf("越权访问得到 %d/%d，期望 404/40401", rec.Code, env.Code)
	}
}

// ---------- 任务 ----------

func TestGetTask(t *testing.T) {
	f := newReviewFixture(t)
	ctx := context.Background()
	_ = f.store.tasks.Create(ctx, &port.Task{ID: "T-1", Type: "review.run", Status: port.TaskRunning, Progress: 30, Stage: "正在抽取合同要素"})

	rec, env := doRequest(t, f.api, http.MethodGet, "/api/v1/tasks/T-1", nil)
	if rec.Code != http.StatusOK || env.Code != 0 {
		t.Fatalf("状态码/业务码 = %d/%d", rec.Code, env.Code)
	}
	data := dataMap(t, env)
	if data["progress"].(float64) != 30 || data["stage"] != "正在抽取合同要素" {
		t.Fatalf("任务体 = %v", data)
	}
	if _, leaked := data["payload"]; leaked {
		t.Fatal("payload 是内部载荷，不该出现在响应里")
	}

	rec, env = doRequest(t, f.api, http.MethodGet, "/api/v1/tasks/NOPE", nil)
	if rec.Code != http.StatusNotFound || env.Code != int(port.CodeNotFound) {
		t.Fatalf("不存在的任务得到 %d/%d", rec.Code, env.Code)
	}
}

func TestCancelTask(t *testing.T) {
	f := newReviewFixture(t)
	ctx := context.Background()
	_ = f.store.tasks.Create(ctx, &port.Task{ID: "T-1", Type: "review.run", Status: port.TaskRunning})

	rec, env := doRequest(t, f.api, http.MethodPost, "/api/v1/tasks/T-1/cancel", nil)
	if rec.Code != http.StatusOK || env.Code != 0 {
		t.Fatalf("状态码/业务码 = %d/%d", rec.Code, env.Code)
	}
	if dataMap(t, env)["status"] != "cancelled" {
		t.Fatalf("取消后状态 = %v", dataMap(t, env)["status"])
	}

	// 再取消一次必须 409：终态不可再转移。
	rec, env = doRequest(t, f.api, http.MethodPost, "/api/v1/tasks/T-1/cancel", nil)
	if rec.Code != http.StatusConflict || env.Code != int(port.CodeConflict) {
		t.Fatalf("重复取消得到 %d/%d，期望 409/40901", rec.Code, env.Code)
	}
}

// ---------- 状态回写包装 ----------

func TestReviewHandlerMarksSucceeded(t *testing.T) {
	f := newReviewFixture(t)
	ctx := context.Background()
	_ = f.store.tasks.Create(ctx, &port.Task{ID: "T-1", Type: "review.run", Status: port.TaskRunning})

	wrapped := New(testConfig("dev-user-0001"), discardLogger(),
		WithStore(f.store), WithReviewHandler(okHandler)).reviewHandlerFor("T-1")
	if err := wrapped(ctx, json.RawMessage(`{}`), func(int, string) {}); err != nil {
		t.Fatalf("包装后的 handler 返回错误: %v", err)
	}

	task, _ := f.store.tasks.Get(ctx, "T-1")
	if task.Status != port.TaskSucceeded {
		t.Fatalf("任务状态 = %s，期望 succeeded", task.Status)
	}
}

func TestReviewHandlerRetryableLeavesRunning(t *testing.T) {
	f := newReviewFixture(t)
	ctx := context.Background()
	_ = f.store.tasks.Create(ctx, &port.Task{ID: "T-1", Type: "review.run", Status: port.TaskRunning})

	calls := 0
	a := New(testConfig("dev-user-0001"), discardLogger(),
		WithStore(f.store),
		WithReviewHandler(func(context.Context, json.RawMessage, func(int, string)) error {
			calls++
			// 前两次可重试失败，第三次成功：模拟 worker 的重试语义。
			if calls < 3 {
				return port.New(port.CodeUpstream, "上游超时")
			}
			return nil
		}),
	)

	wrapped := a.reviewHandlerFor("T-1")
	for i := 0; i < 3; i++ {
		_ = wrapped(ctx, json.RawMessage(`{}`), func(int, string) {})
	}

	task, _ := f.store.tasks.Get(ctx, "T-1")
	if task.Status != port.TaskSucceeded {
		t.Fatalf("三次尝试后任务状态 = %s，期望 succeeded（重试成功不能被中途的 failed 挡住）", task.Status)
	}
}

func TestReviewHandlerFinalFailureMarksFailed(t *testing.T) {
	f := newReviewFixture(t)
	ctx := context.Background()
	_ = f.store.tasks.Create(ctx, &port.Task{ID: "T-1", Type: "review.run", Status: port.TaskRunning})

	a := New(testConfig("dev-user-0001"), discardLogger(),
		WithStore(f.store),
		WithReviewHandler(func(context.Context, json.RawMessage, func(int, string)) error {
			return port.New(port.CodeInvalidArgument, "抽取失败")
		}),
	)
	_ = a.reviewHandlerFor("T-1")(ctx, json.RawMessage(`{}`), func(int, string) {})

	task, _ := f.store.tasks.Get(ctx, "T-1")
	if task.Status != port.TaskFailed {
		t.Fatalf("不可重试的失败后任务状态 = %s，期望 failed", task.Status)
	}
	if !strings.Contains(task.Error, "抽取失败") {
		t.Fatalf("任务错误文案 = %q，期望带上原因", task.Error)
	}
}

// TestReviewHandlerCanceledContextMarksFailed 覆盖 worker 超时/停机的路径。
//
// worker 超时会 cancel ctx：此时上游错误码往往是可重试的 5xx，
// 但 worker 已经放弃这次执行，任务必须落成 failed——否则前端永远转圈，
// 数据库里留下一条永远不会被任何人收尾的 running 记录。
func TestReviewHandlerCanceledContextMarksFailed(t *testing.T) {
	f := newReviewFixture(t)
	ctx := context.Background()
	if err := f.store.tasks.Create(ctx, &port.Task{ID: "T-1", Type: "review.run", Status: port.TaskRunning}); err != nil {
		t.Fatalf("准备任务: %v", err)
	}

	runCtx, cancel := context.WithCancel(context.Background())
	a := New(testConfig("dev-user-0001"), discardLogger(),
		WithStore(f.store),
		WithReviewHandler(func(hctx context.Context, _ json.RawMessage, _ func(int, string)) error {
			cancel() // 模拟 worker 到点 cancel
			<-hctx.Done()
			return port.New(port.CodeUpstream, "上游超时")
		}),
	)
	_ = a.reviewHandlerFor("T-1")(runCtx, json.RawMessage(`{}`), func(int, string) {})

	task, _ := f.store.tasks.Get(ctx, "T-1")
	if task.Status != port.TaskFailed {
		t.Fatalf("ctx 取消后任务状态 = %s，期望 failed", task.Status)
	}
	if !strings.Contains(task.Error, "中断") {
		t.Fatalf("任务错误文案 = %q，期望说明已中断", task.Error)
	}
}

// TestCreateReviewFallsBackOnUniqueConflict 覆盖并发审查：
//
// 两个请求同时过了 GetByFile 查重（第一个请求查重时另一方还没落库），后插入者
// 撞上 uk_reviews_active_file。这时不能把 40901 抛给用户——他双击了一下按钮而已，
// 应该拿到已经存在的那条 review 与 task，继续轮询。
func TestCreateReviewFallsBackOnUniqueConflict(t *testing.T) {
	f := newReviewFixture(t)
	ctx := context.Background()

	// 另一方已经建好了：一条 running 的审查 + 它的任务。
	_ = f.store.tasks.Create(ctx, &port.Task{ID: "T-RACE", Type: "review.run", Status: port.TaskRunning})
	_ = f.store.reviews.Create(ctx, &port.Review{
		ID: "R-RACE", FileID: "F-1", UserID: "dev-user-0001",
		Status: port.TaskRunning, TaskID: "T-RACE",
	})

	// 本请求查重时看不到（模拟并发窗口），落库时才冲突。
	f.store.reviews.getByFileMissOnce = port.New(port.CodeNotFound, "审查记录不存在")
	f.store.reviews.createErr = port.Wrap(port.CodeConflict, nil, "该合同正在审查中，请等待结果")

	rec, env := postJSON(t, f.api, "/api/v1/contracts/review", `{"file_id":"F-1"}`)

	if rec.Code != http.StatusAccepted {
		t.Fatalf("状态码 = %d，期望 202（应回退到已有记录）", rec.Code)
	}
	data := dataMap(t, env)
	if data["review_id"] != "R-RACE" || data["task_id"] != "T-RACE" {
		t.Fatalf("没有回退到已存在的审查: %v", data)
	}
	if len(f.submit.tasks) != 0 {
		t.Fatalf("冲突时不该入池，实际入池 %d 个", len(f.submit.tasks))
	}
}
