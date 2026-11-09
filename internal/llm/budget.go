package llm

import (
	"context"
	"log/slog"
	"sync"
	"time"
)

// budget 是日成本闸：本地缓存 + 写穿 Redis。
//
// 为什么不是"每次调用都查 Redis"：exceeded() 由 route() 调用，而 route 没有 ctx。
// 把 ctx 一路传进 route 要改 Chat / ChatStream 的全部路径和十几个测试调用点，
// 只为多一次 Redis 往返；而真正的风险不是"多花一毛"，是"重启后预算清零"。
// 所以采用写穿 + 回读 + 启动预热：本地永远是最新的一份，Redis 只是把它记住。
//
// 为什么 exceeded() 不带 ctx：它是纯内存判断，必须在任何地方、任何时刻都能立刻
// 给出答案（包括在 ctx 已取消的收尾路径上）。Redis 的读写只发生在 add 与 warm。
//
// 为什么 Redis 挂了要放行：预算是防呆闸，不是权限闸。Redis 抖一下就把所有人
// 拦死，代价远大于当天多花几块钱。写失败时本地计数照走，闸门退化成改动前的
// 进程内语义，并在日志里 Warn 一次（同日只喊一次，否则每条请求一条会淹没真问题）。
//
// 多实例的边界：每次 add 都会用 Redis 返回的当日总额覆盖本地值，所以两个实例
// 的本地值会在各自下一次调用时被校正到全局总额。唯一的缝隙是两边同时读到
// "还没超" 并在同一瞬间各放一次调用——最多多放一两次，可接受；要彻底消掉这个
// 缝隙就得让 route 带上 ctx 并每次查 Redis，那是拿每轮一次的往返换一个可忽略的
// 误差，不划算。
type budget struct {
	mu    sync.Mutex
	limit float64
	spent float64
	day   string
	now   func() time.Time

	store BudgetStore
	log   *slog.Logger
	// warnedDay 记录已经 Warn 过的那一天：Redis 挂的时候每个请求都会失败，
	// 没有这道闸，日志会被"写 Redis 失败"刷满，真正的故障反而看不见。
	warnedDay string
}

// BudgetStore 是日预算的持久化口子。
//
// 声明在使用侧（而不是 port）：port 是两人共用的契约，冻结着；这份接口只有
// llm 与 store 知道，形态随时可以随实现调整。
//
// day 一律是 budget.dayKey(now()) 的产物（UTC "2006-01-02"），两边必须一致，
// 否则跨零点时会读到昨天的账。
type BudgetStore interface {
	LoadCost(ctx context.Context, day string) (float64, error)
	AddCost(ctx context.Context, day string, cost float64) (float64, error)
}

// budgetWriteTimeout 是 Redis 读写的上限。
//
// 记账不能拖住请求：客户端拿到最后一帧就断开是常态，此时早就没有 ctx 预算了。
const budgetWriteTimeout = 2 * time.Second

// newBudget 构造日预算。limit <= 0 表示不限（配置校验保证不会出现，这里只是兜底）。
func newBudget(limit float64) *budget {
	b := &budget{limit: limit, now: time.Now, log: slog.Default()}
	b.day = b.dayKey(b.now())
	return b
}

// withStore 挂上持久化。返回 b 便于链式构造。
func (b *budget) withStore(s BudgetStore, log *slog.Logger) *budget {
	b.store = s
	if log != nil {
		b.log = log
	}
	return b
}

// dayKey 用 UTC 日期分组。
//
// 用 UTC 而不是本地时间：凌晨换算会让"今天"在 08:00 突然重置，
// 而 UTC 至少是单调的，日报脚本也好对。
func (b *budget) dayKey(t time.Time) string {
	return t.UTC().Format("2006-01-02")
}

// add 累加一笔花费：本地先记（Redis 挂了也有下界），再写穿到 Redis 并回读总额。
func (b *budget) add(ctx context.Context, cost float64) {
	if cost <= 0 {
		return
	}

	b.mu.Lock()
	today := b.dayKey(b.now())
	if today != b.day {
		b.day = today
		b.spent = 0
	}
	b.spent += cost
	day := b.day
	b.mu.Unlock()

	if b.store == nil {
		return
	}

	// 客户端拿到最后一帧就断开是常态，此时 ctx 已取消；记账不能跟着失败。
	// 与 internal/api/chat.go 的 persistTurn 同一取舍。
	wctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), budgetWriteTimeout)
	defer cancel()

	total, err := b.store.AddCost(wctx, day, cost)
	if err != nil {
		b.warnOnce(day, "llm budget: 写 Redis 失败，今日预算按本地计数判断", err)
		return
	}

	b.mu.Lock()
	// 期间跨天则不要覆盖：Redis 记的是那一天的总额，写进今天的 spent 会把
	// 昨天的账算到头上。
	if b.day == day {
		b.spent = total
	}
	b.mu.Unlock()
}

// warm 启动时把当日已花从 Redis 读回本地，补上重启前的值。
func (b *budget) warm(ctx context.Context) error {
	if b.store == nil {
		return nil
	}
	wctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), budgetWriteTimeout)
	defer cancel()

	day := b.dayKey(b.now())
	total, err := b.store.LoadCost(wctx, day)
	if err != nil {
		return err
	}
	b.mu.Lock()
	b.day = day
	b.spent = total
	b.mu.Unlock()
	return nil
}

// warnOnce 同一天只喊一次。跨天重置，让第二天 Redis 还没修好时再提醒一次。
func (b *budget) warnOnce(day, msg string, err error) {
	b.mu.Lock()
	if b.warnedDay == day {
		b.mu.Unlock()
		return
	}
	b.warnedDay = day
	b.mu.Unlock()
	b.log.Warn(msg, "day", day, "err", err.Error())
}

// exceeded 表示今日已超出预算。
func (b *budget) exceeded() bool {
	if b.limit <= 0 {
		return false
	}
	b.mu.Lock()
	defer b.mu.Unlock()

	if today := b.dayKey(b.now()); today != b.day {
		return false
	}
	return b.spent >= b.limit
}

// snapshot 返回今日已花与上限，跨天时已花按 0 计。
func (b *budget) snapshot() (spent, limit float64) {
	b.mu.Lock()
	defer b.mu.Unlock()

	if b.dayKey(b.now()) != b.day {
		return 0, b.limit
	}
	return b.spent, b.limit
}
