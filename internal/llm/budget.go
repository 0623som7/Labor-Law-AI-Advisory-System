package llm

import (
	"sync"
	"time"
)

// budget 是进程内的日成本闸。
//
// 只记内存、不落库：它是"防呆"而不是"账本"——账本在 usage_records 表里。
// 进程重启后预算清零是可接受的（大不了当天多花一点），但如果为了准确去查库，
// 每次调用都会多一次往返，2 核机器上不值得。
type budget struct {
	mu    sync.Mutex
	limit float64
	spent float64
	day   string
	now   func() time.Time
}

// newBudget 构造日预算。limit <= 0 表示不限（配置校验保证不会出现，这里只是兜底）。
func newBudget(limit float64) *budget {
	b := &budget{limit: limit, now: time.Now}
	b.day = b.dayKey(b.now())
	return b
}

// dayKey 用 UTC 日期分组。
//
// 用 UTC 而不是本地时间：凌晨换算会让"今天"在 08:00 突然重置，
// 而 UTC 至少是单调的，日报脚本也好对。
func (b *budget) dayKey(t time.Time) string {
	return t.UTC().Format("2006-01-02")
}

// add 累加一笔花费，跨天先清零。
func (b *budget) add(cost float64) {
	if cost <= 0 {
		return
	}
	b.mu.Lock()
	defer b.mu.Unlock()

	today := b.dayKey(b.now())
	if today != b.day {
		b.day = today
		b.spent = 0
	}
	b.spent += cost
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
