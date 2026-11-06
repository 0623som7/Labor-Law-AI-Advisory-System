// Package worker 是审查任务的协程池。
//
// 申报书里写的"基于 Go Routine 实现高并发审查任务调度"就是这一层：固定数量的
// worker goroutine 从有缓冲队列里取任务，并发上限只在这里配置，B 的 Handler
// 不许自己起 goroutine——否则并发数会随 B 加节点而无上限增长，压测时才暴露。
//
// 本包只做五件事：限并发、限队列、单任务超时、可重试错误重试、进度节流。
// 它不读 tasks 表：进度落库通过 ProgressSink 注入，测试用内存 fake，
// 生产装配时接 store.Task().UpdateProgress。
package worker

import (
	"context"
	"errors"
	"log/slog"
	"sync"
	"time"

	"laborlaw/internal/config"
	"laborlaw/internal/port"
)

// maxRetries 是首次执行之外的重试次数。3 次总尝试是 LLM 类上游的经验值：
// 再多的重试基本只会把上游的一次抖动放大成持续的 429。
const maxRetries = 2

// progressInterval 是进度落库的最小间隔，来自 port.Task.UpdateProgress 的注释（≥500ms）。
//
// 为什么必须节流：一次审查会按规则条数回调几十次，不节流就是几十条 UPDATE，
// 而这些 UPDATE 全部落在同一行上，只会互相锁等，读到的还是最后一个值。
const progressInterval = 500 * time.Millisecond

// ProgressSink 把任务进度写进持久层。
//
// 用函数而不是直接依赖 port.TaskStore：池子不该知道"任务表长什么样"，
// 换一张表或加缓存都不用改这里；测试也只需要一个内存 fake。
type ProgressSink func(ctx context.Context, taskID string, percent int, stage string) error

// Pool 是固定大小的任务池。零值不可用，必须用 New 构造。
type Pool struct {
	cfg config.WorkerConfig
	log *slog.Logger

	// queue 必须有缓冲。无缓冲队列会让 Submit 等价于同步等待，42901 永远触发不了，
	// "池满立即拒绝"的契约也就无从谈起。
	queue chan job
	// stop 关闭后 worker 退出。由 Shutdown 在所有在途任务结束后关闭。
	stop     chan struct{}
	stopOnce sync.Once
	// workers 追踪 worker goroutine，Shutdown 要等它们全部退出。
	workers sync.WaitGroup
	// jobs 追踪"已接收但未完成"的任务，Shutdown 用它判断在途是否清零。
	jobs sync.WaitGroup

	// mu 保护 closed 与 sink。Submit 持读锁完成"检查 closed + Add + 入队"，
	// Shutdown 持写锁置 closed：这样 jobs.Add 永远不会与 jobs.Wait 并发，
	// 否则 Wait 可能在计数为 0 时提前返回，留下一份没人执行的任务。
	mu     sync.RWMutex
	closed bool
	sink   ProgressSink
}

// job 是队列元素。Task 指针与 Handler 一起入队：同一个池可以跑多种任务类型，
// 具体类型 → Handler 的查表在提交方（api / 调度器）完成，池不参与注册表。
type job struct {
	task *port.Task
	h    port.Handler
}

// New 构造并启动池。
//
// cfg 里的非法值会被夹到安全值而不是报错：2G 机器上 PoolSize=0 会让池一个
// goroutine 都不起（任务永远挂着），这比"按 1 个 worker 跑"糟得多。
func New(cfg config.WorkerConfig, log *slog.Logger) *Pool {
	if log == nil {
		log = slog.Default()
	}
	if cfg.PoolSize <= 0 {
		log.Warn("WORKER_POOL_SIZE 非法，按 1 处理", "value", cfg.PoolSize)
		cfg.PoolSize = 1
	}
	if cfg.QueueSize <= 0 {
		// 无缓冲队列会让 Submit 退化成同步等待，直接违背 42901 的语义。
		log.Warn("WORKER_QUEUE_SIZE 非法，按 1 处理", "value", cfg.QueueSize)
		cfg.QueueSize = 1
	}
	if cfg.TaskTimeout <= 0 {
		cfg.TaskTimeout = 120 * time.Second
	}

	p := &Pool{
		cfg:   cfg,
		log:   log,
		queue: make(chan job, cfg.QueueSize),
		stop:  make(chan struct{}),
	}
	p.workers.Add(cfg.PoolSize)
	for i := 0; i < cfg.PoolSize; i++ {
		go p.worker(i)
	}
	return p
}

// SetProgressSink 注入进度落库回调。
//
// 允许在启动后设置（而不是塞进 New）：没有 sink 时池子照样能跑任务，
// 只是进度只存在内存里——这正是单元测试与"先接通链路、后接落库"的形态。
func (p *Pool) SetProgressSink(s ProgressSink) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.sink = s
}

// Submit 把任务交给池。
//
// 队列满时立刻返回 42901，绝不阻塞：42901 的契约含义就是"池满立即拒绝"，
// 而阻塞会让 api 的请求 goroutine 跟着排队，最终把整个 HTTP 层拖死。
//
// t 在 Submit 成功后归池所有，调用方不得再读写它的字段；唯一安全的读时机是
// Shutdown 返回之后（那时所有 worker 都已退出）。
func (p *Pool) Submit(ctx context.Context, t *port.Task, h port.Handler) error {
	if t == nil || h == nil {
		return port.New(port.CodeInvalidArgument, "任务或处理器为空")
	}

	// 读锁覆盖"检查 closed → jobs.Add → 入队"整段：Shutdown 的写锁会一直等到
	// 这段结束，从而保证 jobs.Wait 不会在 Add 之前返回。
	p.mu.RLock()
	defer p.mu.RUnlock()

	if p.closed {
		return errClosed
	}

	p.jobs.Add(1)
	select {
	case p.queue <- job{task: t, h: h}:
		return nil
	default:
		// 这一步失败必须回滚计数，否则 Shutdown 会永远等一个不存在的任务。
		p.jobs.Done()
		return port.New(port.CodeRateLimited, "任务队列已满")
	}
}

// errClosed 是池关闭后的拒绝原因。
//
// 选 40901 而不是 42901：42901 的含义是"现在忙，等会儿再试"，前端会自动重试；
// 池关闭是终态，重试永远不会成功，必须让调用方走"标记 pending → failed"的分支。
// TaskStatus.CanTransition 允许 pending → failed，正是为这个场景留的。
var errClosed = port.New(port.CodeConflict, "任务池已关闭")

// Shutdown 停止接收新任务，等在途任务（含队列里未取出的）全部结束。
//
// ctx 控制等待上限：超时返回 50401，但不会杀任务——杀到一半的任务会留下
// 半份审查结果，而 worker 已经带着自己的 TaskTimeout 在跑，让它自然结束更安全。
// 可以重复调用：已关闭时只做等待。
func (p *Pool) Shutdown(ctx context.Context) error {
	p.mu.Lock()
	p.closed = true
	p.mu.Unlock()

	// jobs 计数为 0 时，队列必然已空（每个入队任务都带着一次 Add）。
	done := make(chan struct{})
	go func() {
		p.jobs.Wait()
		close(done)
	}()

	select {
	case <-done:
	case <-ctx.Done():
		return port.Wrap(port.CodeTaskTimeout, ctx.Err(), "等待在途任务超时")
	}

	p.stopOnce.Do(func() { close(p.stop) })
	p.workers.Wait()
	return nil
}

// worker 是池里的一个执行者，只做"取任务 → 跑 → 记日志"。
func (p *Pool) worker(id int) {
	defer p.workers.Done()
	for {
		select {
		case j := <-p.queue:
			p.run(id, j)
		case <-p.stop:
			// Shutdown 关闭 stop 时队列已空，这里直接退出即可。
			return
		}
	}
}

// run 执行一个任务，含超时与重试。
func (p *Pool) run(workerID int, j job) {
	defer p.jobs.Done()

	// 任务上下文不派生自 Submit 的 ctx：那个 ctx 属于 HTTP 请求，响应一返回就被取消，
	// 用它会让所有异步任务在用户关页面时集体失败。任务只受 TaskTimeout 约束。
	ctx, cancel := context.WithTimeout(context.Background(), p.cfg.TaskTimeout)
	defer cancel()

	task := j.task
	task.Status = port.TaskRunning

	progress := newProgressReporter(ctx, p, task)

	var lastErr error
	for attempt := 1; attempt <= 1+maxRetries; attempt++ {
		task.Attempt = attempt
		err := j.h(ctx, task.Payload, progress.report)
		if err == nil {
			progress.flush()
			task.Status = port.TaskSucceeded
			p.log.Info("任务完成",
				"task_id", task.ID, "type", task.Type, "attempt", attempt, "worker", workerID)
			return
		}

		lastErr = err
		if ctxErr := ctx.Err(); ctxErr != nil {
			// 超时后不再重试：上游已经超时一次，再重试两次只会把 120s 变成 360s。
			task.Status = port.TaskFailed
			task.Error = "任务超时"
			progress.flush()
			p.log.Warn("任务超时",
				"task_id", task.ID, "type", task.Type, "attempt", attempt, "worker", workerID)
			return
		}

		if !shouldRetry(err) {
			break
		}
		if attempt < 1+maxRetries {
			p.log.Warn("任务失败，准备重试",
				"task_id", task.ID, "type", task.Type, "attempt", attempt, "err", err.Error())
		}
	}

	progress.flush()
	task.Status = port.TaskFailed
	task.Error = failureMessage(lastErr)
	p.log.Error("任务失败",
		"task_id", task.ID, "type", task.Type, "attempt", task.Attempt, "err", lastErr)
}

// shouldRetry 决定要不要再跑一次。
//
// 两条都必须满足：错误是 *port.AppError（分类过），且它的码标了可重试。
// 普通 error 一律不重试——未分类的失败基本是确定性 bug，重试只会让一个缺陷
// 变成三次资源消耗，还会把真正的原因埋进重试日志里。
func shouldRetry(err error) bool {
	var appErr *port.AppError
	if !errors.As(err, &appErr) {
		return false
	}
	return port.IsRetryable(err)
}

// failureMessage 取出可以落库的失败原因。
//
// 只写 Message：cause 里可能是上游原始报文、合同原文或密钥片段，
// Task.Error 会被接口原样返给前端。
func failureMessage(err error) string {
	if err == nil {
		return "任务失败"
	}
	return port.AsAppError(err).Message
}

// progressReporter 是每个任务一份的进度节流器。
//
// 不共享：进度是任务级状态，共享一个实例会让并发任务的 last 时间互相干扰，
// 表现为"有的任务进度永远不落库"。
type progressReporter struct {
	ctx  context.Context
	pool *Pool
	task *port.Task

	mu      sync.Mutex
	last    time.Time // 零值表示还没写过，第一次一定落库
	pending *progressPoint
}

type progressPoint struct {
	percent int
	stage   string
}

func newProgressReporter(ctx context.Context, p *Pool, t *port.Task) *progressReporter {
	return &progressReporter{ctx: ctx, pool: p, task: t}
}

// report 是交给 Handler 的进度回调，可能被 Handler 从多个 goroutine 调用。
//
// 节流规则：距上次落库 ≥500ms 就写，否则只记下最新值（后面的覆盖前面的）。
// 被丢掉的那些中间值没有信息量——前端轮询看到的是"最新进度"，不是历史曲线。
func (r *progressReporter) report(percent int, stage string) {
	if percent < 0 {
		percent = 0
	}
	if percent > 100 {
		percent = 100
	}

	r.mu.Lock()
	defer r.mu.Unlock()

	now := time.Now()
	if !r.last.IsZero() && now.Sub(r.last) < progressInterval {
		r.pending = &progressPoint{percent: percent, stage: stage}
		return
	}
	r.emitLocked(now, progressPoint{percent: percent, stage: stage})
}

// flush 在任务结束时把最后一次进度补写出去。
//
// 少了它，界面上会永远停在 40%——那一次 100% 的回调正好落在节流窗口里。
func (r *progressReporter) flush() {
	r.mu.Lock()
	defer r.mu.Unlock()

	if r.pending == nil {
		return
	}
	r.emitLocked(time.Now(), *r.pending)
	r.pending = nil
}

// emitLocked 更新内存副本并调用 sink，调用方必须持锁。
func (r *progressReporter) emitLocked(now time.Time, pt progressPoint) {
	r.last = now
	r.task.Progress = pt.percent
	r.task.Stage = pt.stage

	p := r.pool
	p.mu.RLock()
	sink := p.sink
	p.mu.RUnlock()
	if sink == nil {
		return
	}
	// 进度写失败不影响任务本身：丢了半秒的进度条比把一次成功的审查标成失败划算得多。
	if err := sink(r.ctx, r.task.ID, pt.percent, pt.stage); err != nil {
		p.log.Warn("进度落库失败", "task_id", r.task.ID, "err", err.Error())
	}
}
