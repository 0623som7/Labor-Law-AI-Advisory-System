package llm

import (
	"context"
	"errors"
	"testing"
	"time"

	"laborlaw/internal/port"
)

func TestTokenBucketAllowsInitialBurst(t *testing.T) {
	b := newTokenBucket(2) // 容量 2

	for i := 0; i < 2; i++ {
		ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
		err := b.Wait(ctx)
		cancel()
		if err != nil {
			t.Fatalf("第 %d 个令牌应当立刻拿到: %v", i+1, err)
		}
	}
}

func TestTokenBucketBlocksWhenEmpty(t *testing.T) {
	b := newTokenBucket(1)
	if err := b.Wait(context.Background()); err != nil {
		t.Fatalf("首个令牌应当立刻拿到: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 80*time.Millisecond)
	defer cancel()

	err := b.Wait(ctx)
	if err == nil {
		t.Fatal("令牌耗尽后应当阻塞到 ctx 结束")
	}
	if code := port.CodeOf(err); code != port.CodeRateLimited {
		t.Fatalf("错误码 = %v, want 42901", code)
	}
}

func TestTokenBucketReplenishes(t *testing.T) {
	b := newTokenBucket(20) // 每 50ms 回一个
	if err := b.Wait(context.Background()); err != nil {
		t.Fatalf("首个令牌: %v", err)
	}

	time.Sleep(70 * time.Millisecond)

	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	if err := b.Wait(ctx); err != nil {
		t.Fatalf("令牌应当已补充: %v", err)
	}
}

func TestBudgetAccumulatesAndExceeds(t *testing.T) {
	b := newBudget(1.0)
	fixed := time.Date(2026, 11, 6, 12, 0, 0, 0, time.UTC)
	b.now = func() time.Time { return fixed }
	b.day = b.dayKey(fixed)

	b.add(context.Background(), 0.6)
	if spent, limit := b.snapshot(); spent != 0.6 || limit != 1.0 {
		t.Fatalf("snapshot = (%v, %v), want (0.6, 1)", spent, limit)
	}
	if b.exceeded() {
		t.Fatal("0.6 < 1 不该判定超限")
	}

	b.add(context.Background(), 0.5)
	if !b.exceeded() {
		t.Fatal("累计 1.1 > 1 应当判定超限")
	}
}

func TestBudgetResetsOnNewDay(t *testing.T) {
	b := newBudget(1.0)
	day1 := time.Date(2026, 11, 6, 23, 0, 0, 0, time.UTC)
	b.now = func() time.Time { return day1 }
	b.day = b.dayKey(day1)
	b.add(context.Background(), 5)

	if !b.exceeded() {
		t.Fatal("当天已超额")
	}

	day2 := day1.Add(2 * time.Hour) // 跨到 UTC 次日
	b.now = func() time.Time { return day2 }

	if b.exceeded() {
		t.Fatal("跨天后应当重置")
	}
	if spent, _ := b.snapshot(); spent != 0 {
		t.Fatalf("跨天后 spent = %v, want 0", spent)
	}
}

func TestBudgetIgnoresNonPositiveCost(t *testing.T) {
	b := newBudget(1)
	b.add(context.Background(), 0)
	b.add(context.Background(), -5)
	if spent, _ := b.snapshot(); spent != 0 {
		t.Fatalf("spent = %v, want 0", spent)
	}
}

// fakeBudgetStore 是内存版 BudgetStore。
//
// total 模拟 Redis 里的当日总额：AddCost 返回值就是它，这样"本地值被
// 全局值覆盖"这条规则才测得出来。
type fakeBudgetStore struct {
	total   float64
	loadErr error
	addErr  error

	loads   int
	added   []float64
	lastDay string
}

func (f *fakeBudgetStore) LoadCost(_ context.Context, day string) (float64, error) {
	f.loads++
	f.lastDay = day
	if f.loadErr != nil {
		return 0, f.loadErr
	}
	return f.total, nil
}

func (f *fakeBudgetStore) AddCost(_ context.Context, day string, cost float64) (float64, error) {
	f.lastDay = day
	if f.addErr != nil {
		return 0, f.addErr
	}
	f.total += cost
	f.added = append(f.added, cost)
	return f.total, nil
}

// 下面四条是这次改动的真正验收：现有三条用例在 store 为 nil 时走的是老路径，
// 全绿也证明不了 Redis 接上了。

func TestBudgetWritesThroughStore(t *testing.T) {
	store := &fakeBudgetStore{}
	b := newBudget(1.0).withStore(store, nil)
	fixed := time.Date(2026, 11, 6, 12, 0, 0, 0, time.UTC)
	b.now = func() time.Time { return fixed }
	b.day = b.dayKey(fixed)

	b.add(context.Background(), 0.6)

	if store.total != 0.6 {
		t.Fatalf("写穿后 fake.total = %v, want 0.6", store.total)
	}
	if spent, _ := b.snapshot(); spent != 0.6 {
		t.Fatalf("snapshot().spent = %v, want 0.6", spent)
	}
	if len(store.added) != 1 || store.lastDay != b.dayKey(fixed) {
		t.Fatalf("应当写一次、day=%s，实际 added=%v day=%s", b.dayKey(fixed), store.added, store.lastDay)
	}
}

func TestBudgetUsesStoreTotalAfterAdd(t *testing.T) {
	// Redis 里已经有别人记的账：本地不能停在"我只花了 0.1"。
	store := &fakeBudgetStore{total: 4.9}
	b := newBudget(10).withStore(store, nil)
	fixed := time.Date(2026, 11, 6, 12, 0, 0, 0, time.UTC)
	b.now = func() time.Time { return fixed }
	b.day = b.dayKey(fixed)

	b.add(context.Background(), 0.1)

	if spent, _ := b.snapshot(); spent != 5.0 {
		t.Fatalf("snapshot().spent = %v, want 5.0（应被 Redis 总额覆盖）", spent)
	}
}

func TestBudgetSurvivesRestart(t *testing.T) {
	// 模拟重启：新 budget 的 spent 是 0，预热后必须认出今天已经花超。
	store := &fakeBudgetStore{total: 100}
	b := newBudget(1).withStore(store, nil)

	if b.exceeded() {
		t.Fatal("预热前 spent 为 0，不该判定超限")
	}
	if err := b.warm(context.Background()); err != nil {
		t.Fatalf("warm 失败: %v", err)
	}
	if !b.exceeded() {
		t.Fatal("预热读到 100 > 1，必须判定超限")
	}
	if spent, _ := b.snapshot(); spent != 100 {
		t.Fatalf("snapshot().spent = %v, want 100", spent)
	}
}

func TestBudgetStoreErrorDoesNotBlock(t *testing.T) {
	store := &fakeBudgetStore{addErr: errors.New("redis: connection refused")}
	b := newBudget(1).withStore(store, nil)
	fixed := time.Date(2026, 11, 6, 12, 0, 0, 0, time.UTC)
	b.now = func() time.Time { return fixed }
	b.day = b.dayKey(fixed)

	b.add(context.Background(), 0.6)

	// Redis 挂了要放行：本地计数照走，闸门退化成进程内语义。
	if spent, _ := b.snapshot(); spent != 0.6 {
		t.Fatalf("snapshot().spent = %v, want 0.6（写 Redis 失败不该影响本地计数）", spent)
	}
	if b.exceeded() {
		t.Fatal("0.6 < 1 不该判定超限")
	}
}
