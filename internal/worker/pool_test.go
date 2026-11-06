package worker

import (
	"context"
	"encoding/json"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"laborlaw/internal/config"
	"laborlaw/internal/port"
)

// fakeTaskStore 是内存版 port.TaskStore。
//
// 只实现 UpdateProgress 有意义，其余方法返回固定结果：本轮的池子只调它一个，
// 但接口必须完整——用一个只实现部分方法的假类型会让"池子悄悄多调了 Finish"
// 这类变化编译不过，反而更安全。
type fakeTaskStore struct {
	mu     sync.Mutex
	update []progressCall
}

type progressCall struct {
	taskID  string
	percent int
	stage   string
}

func (f *fakeTaskStore) Create(context.Context, *port.Task) error { return nil }

func (f *fakeTaskStore) Get(context.Context, string) (*port.Task, error) {
	return nil, port.New(port.CodeNotFound, "任务不存在")
}

func (f *fakeTaskStore) UpdateProgress(_ context.Context, id string, percent int, stage string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.update = append(f.update, progressCall{taskID: id, percent: percent, stage: stage})
	return nil
}

func (f *fakeTaskStore) Finish(context.Context, string, port.TaskStatus, string) error { return nil }

func (f *fakeTaskStore) ListByStatus(context.Context, port.TaskStatus, int) ([]port.Task, error) {
	return nil, nil
}

func (f *fakeTaskStore) FailRunning(context.Context, string) (int64, error) { return 0, nil }

func (f *fakeTaskStore) calls() []progressCall {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]progressCall(nil), f.update...)
}

// shutdown 用固定上限等待池子退出：测试里用 context.Background() 会让
// 一个死锁变成整个 go test 卡死，而不是一条失败信息。
func shutdown(t *testing.T, p *Pool) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := p.Shutdown(ctx); err != nil {
		t.Fatalf("Shutdown() 返回错误: %v", err)
	}
}

func testCfg(poolSize, queueSize int) config.WorkerConfig {
	return config.WorkerConfig{
		PoolSize:    poolSize,
		QueueSize:   queueSize,
		TaskTimeout: 5 * time.Second,
	}
}

func newPool(t *testing.T, poolSize, queueSize int) *Pool {
	t.Helper()
	p := New(testCfg(poolSize, queueSize), nil)
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := p.Shutdown(ctx); err != nil {
			t.Logf("Shutdown() 返回错误: %v", err)
		}
	})
	return p
}

// blockingHandler 一直阻塞到 release 关闭，用于把池子占满。
func blockingHandler(started chan<- struct{}, release <-chan struct{}) port.Handler {
	return func(context.Context, json.RawMessage, func(int, string)) error {
		started <- struct{}{}
		<-release
		return nil
	}
}

// TestSubmitQueueFullRejects 守住 42901：队列满时不排队、不阻塞，立刻拒绝。
func TestSubmitQueueFullRejects(t *testing.T) {
	const queueSize = 2
	p := newPool(t, 1, queueSize)

	started := make(chan struct{}, 16)
	release := make(chan struct{})
	h := blockingHandler(started, release)

	ctx := context.Background()
	// 先把唯一的 worker 占住，队列才是干净的初始状态。
	if err := p.Submit(ctx, &port.Task{ID: "t-0", Type: "review.run"}, h); err != nil {
		t.Fatalf("第 1 个 Submit 失败: %v", err)
	}
	<-started

	// 队列容量 2：再塞两个都能进，第三个必须被拒。
	for i := 1; i <= queueSize; i++ {
		if err := p.Submit(ctx, &port.Task{ID: "t-fill"}, h); err != nil {
			t.Fatalf("第 %d 个 Submit 失败: %v", i+1, err)
		}
	}

	start := time.Now()
	err := p.Submit(ctx, &port.Task{ID: "t-overflow"}, h)
	if code := port.CodeOf(err); code != port.CodeRateLimited {
		t.Fatalf("队列满时错误码 = %v, want 42901（err=%v）", code, err)
	}
	if elapsed := time.Since(start); elapsed > 100*time.Millisecond {
		t.Fatalf("队列满时耗时 %v：Submit 阻塞了，契约要求立即返回", elapsed)
	}

	close(release)
}

// TestRetryOnRetryableError 守住"可重试错误最多 2 次重试"，即总执行 3 次。
func TestRetryOnRetryableError(t *testing.T) {
	p := newPool(t, 1, 4)

	var calls int32
	h := func(context.Context, json.RawMessage, func(int, string)) error {
		atomic.AddInt32(&calls, 1)
		return port.New(port.CodeUpstream, "上游返回 502")
	}

	task := &port.Task{ID: "t-retry", Type: "review.run"}
	if err := p.Submit(context.Background(), task, h); err != nil {
		t.Fatalf("Submit() 返回错误: %v", err)
	}
	shutdown(t, p)

	if got := atomic.LoadInt32(&calls); got != 3 {
		t.Fatalf("执行次数 = %d, want 3（1 次 + 2 次重试）", got)
	}
	if task.Attempt != 3 {
		t.Fatalf("Attempt = %d, want 3", task.Attempt)
	}
	if task.Status != port.TaskFailed {
		t.Fatalf("Status = %s, want failed", task.Status)
	}
}

// TestNoRetryOnPlainError 守住"普通 error 一律不重试"。
func TestNoRetryOnPlainError(t *testing.T) {
	p := newPool(t, 1, 4)

	var calls int32
	h := func(context.Context, json.RawMessage, func(int, string)) error {
		atomic.AddInt32(&calls, 1)
		return errors.New("空指针，未分类")
	}

	task := &port.Task{ID: "t-plain", Type: "review.run"}
	if err := p.Submit(context.Background(), task, h); err != nil {
		t.Fatalf("Submit() 返回错误: %v", err)
	}
	shutdown(t, p)

	if got := atomic.LoadInt32(&calls); got != 1 {
		t.Fatalf("执行次数 = %d, want 1（普通 error 不重试）", got)
	}
	if task.Status != port.TaskFailed {
		t.Fatalf("Status = %s, want failed", task.Status)
	}
	// Task.Error 只放 Message，不含 cause。
	if task.Error != "服务内部错误" {
		t.Fatalf("Error = %q, want 服务内部错误（不得拼接 cause）", task.Error)
	}
}

// TestSubmitAfterShutdown 守住"关池后拒绝新任务"。
// 选 40901：池关闭是终态，42901 会让调用方以为等一会儿还能重试。
func TestSubmitAfterShutdown(t *testing.T) {
	p := New(testCfg(1, 2), nil)
	shutdown(t, p)

	h := func(context.Context, json.RawMessage, func(int, string)) error { return nil }
	err := p.Submit(context.Background(), &port.Task{ID: "t-after"}, h)
	if code := port.CodeOf(err); code != port.CodeConflict {
		t.Fatalf("关池后 Submit 错误码 = %v, want 40901", code)
	}
}

// TestShutdownWaitsInFlight 守住"优雅关闭"：在途任务没结束前 Shutdown 不能返回。
func TestShutdownWaitsInFlight(t *testing.T) {
	p := New(testCfg(1, 2), nil)

	started := make(chan struct{})
	release := make(chan struct{})
	h := func(context.Context, json.RawMessage, func(int, string)) error {
		close(started)
		<-release
		return nil
	}

	if err := p.Submit(context.Background(), &port.Task{ID: "t-slow"}, h); err != nil {
		t.Fatalf("Submit() 返回错误: %v", err)
	}
	<-started

	done := make(chan error, 1)
	go func() { done <- p.Shutdown(context.Background()) }()

	select {
	case err := <-done:
		t.Fatalf("Shutdown 在任务结束前就返回了: %v", err)
	case <-time.After(100 * time.Millisecond):
	}

	close(release)
	if err := <-done; err != nil {
		t.Fatalf("Shutdown() 返回错误: %v", err)
	}
}

// TestShutdownTimeout 守住"等不到就返回 50401，而不是永久挂住"。
func TestShutdownTimeout(t *testing.T) {
	p := New(testCfg(1, 2), nil)

	started := make(chan struct{})
	release := make(chan struct{})

	h := func(context.Context, json.RawMessage, func(int, string)) error {
		close(started)
		<-release
		return nil
	}
	if err := p.Submit(context.Background(), &port.Task{ID: "t-timeout"}, h); err != nil {
		t.Fatalf("Submit() 返回错误: %v", err)
	}
	<-started

	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	err := p.Shutdown(ctx)
	if code := port.CodeOf(err); code != port.CodeTaskTimeout {
		t.Fatalf("Shutdown 超时错误码 = %v, want 50401（err=%v）", code, err)
	}

	// 收尾：放行任务后再关一次，让 worker 正常退出，否则 -race 下每次跑都漏一个 goroutine。
	close(release)
	if err := p.Shutdown(context.Background()); err != nil {
		t.Fatalf("第二次 Shutdown() 返回错误: %v", err)
	}
}

// TestProgressThrottle 守住"≥500ms 才落库 + 结束时补写最后一次"。
func TestProgressThrottle(t *testing.T) {
	store := &fakeTaskStore{}
	p := newPool(t, 1, 4)
	p.SetProgressSink(func(ctx context.Context, taskID string, percent int, stage string) error {
		return store.UpdateProgress(ctx, taskID, percent, stage)
	})

	h := func(_ context.Context, _ json.RawMessage, progress func(int, string)) error {
		progress(10, "解析")
		progress(30, "抽取") // 距上次 <500ms，丢弃
		time.Sleep(progressInterval + 20*time.Millisecond)
		progress(60, "规则") // 距上次 ≥500ms，落库
		progress(100, "完成") // 被节流，靠 flush 补写
		return nil
	}

	task := &port.Task{ID: "t-progress", Type: "review.run"}
	if err := p.Submit(context.Background(), task, h); err != nil {
		t.Fatalf("Submit() 返回错误: %v", err)
	}
	shutdown(t, p)

	calls := store.calls()
	wantPercent := []int{10, 60, 100}
	if len(calls) != len(wantPercent) {
		t.Fatalf("落库次数 = %d, want %d（got=%+v）", len(calls), len(wantPercent), calls)
	}
	for i, want := range wantPercent {
		if calls[i].percent != want {
			t.Fatalf("第 %d 次落库 percent = %d, want %d（全部=%+v）", i, calls[i].percent, want, calls)
		}
		if calls[i].taskID != "t-progress" {
			t.Fatalf("第 %d 次落库 taskID = %q", i, calls[i].taskID)
		}
	}
	if task.Progress != 100 || task.Status != port.TaskSucceeded {
		t.Fatalf("任务终态 = %d/%s, want 100/succeeded", task.Progress, task.Status)
	}
}

// TestTaskTimeoutFails 守住单任务超时：Handler 不收手时由 ctx 截止，任务标 failed。
func TestTaskTimeoutFails(t *testing.T) {
	cfg := testCfg(1, 2)
	cfg.TaskTimeout = 60 * time.Millisecond
	p := New(cfg, nil)
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = p.Shutdown(ctx)
	})

	h := func(ctx context.Context, _ json.RawMessage, _ func(int, string)) error {
		<-ctx.Done()
		return port.Wrap(port.CodeTaskTimeout, ctx.Err(), "任务超时")
	}

	task := &port.Task{ID: "t-slow-timeout"}
	if err := p.Submit(context.Background(), task, h); err != nil {
		t.Fatalf("Submit() 返回错误: %v", err)
	}
	shutdown(t, p)

	if task.Status != port.TaskFailed {
		t.Fatalf("Status = %s, want failed", task.Status)
	}
	if task.Attempt != 1 {
		t.Fatalf("Attempt = %d, want 1（超时后不重试）", task.Attempt)
	}
}

// TestNewClampsInvalidConfig 守住 PoolSize/QueueSize 非法时不至于让池子不可用。
func TestNewClampsInvalidConfig(t *testing.T) {
	p := New(config.WorkerConfig{PoolSize: 0, QueueSize: 0, TaskTimeout: 0}, nil)
	defer func() {
		_ = p.Shutdown(context.Background())
	}()

	var called int32
	h := func(context.Context, json.RawMessage, func(int, string)) error {
		atomic.AddInt32(&called, 1)
		return nil
	}
	if err := p.Submit(context.Background(), &port.Task{ID: "t-clamp"}, h); err != nil {
		t.Fatalf("Submit() 返回错误: %v", err)
	}
	shutdown(t, p)
	if atomic.LoadInt32(&called) != 1 {
		t.Fatal("任务没有执行：非法配置被夹到安全值后仍应正常工作")
	}
}
