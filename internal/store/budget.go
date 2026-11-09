package store

import (
	"context"
	"fmt"
	"log/slog"
	"strconv"
	"time"

	"github.com/redis/go-redis/v9"
)

// defaultBudgetTTL 是日预算键的存活时间。
//
// 8 天而不是 1 天：跨天之后运维还得能读到昨天的账面（"昨天到底花了多少"是
// 最常见的追问），1 天 TTL 会在零点后几分钟内把证据删掉。
const defaultBudgetTTL = 8 * 24 * time.Hour

// RedisBudgetStore 用 Redis 存每日模型花费。
//
// 与 vectorcache.go 同一套写法：具体类型 *redis.Client、client 为 nil 当作
// "没有持久化"（调用方照常拿到本地语义，不报错）、redis.Nil 不算错误也不刷日志。
//
// 不 import internal/llm：那边已经 import 了 config，而 store 被 llm 的调用方
// 依赖，直接引会成环。llm 侧声明同名同形的 BudgetStore 接口，靠鸭子类型对上。
type RedisBudgetStore struct {
	client *redis.Client
	log    *slog.Logger
	ttl    time.Duration
}

// NewBudgetStore 构造日预算存储。
func NewBudgetStore(client *redis.Client, log *slog.Logger) *RedisBudgetStore {
	if log == nil {
		log = slog.Default()
	}
	return &RedisBudgetStore{client: client, log: log, ttl: defaultBudgetTTL}
}

// WithTTL 覆盖存活时间，测试用它验证过期行为。
func (s *RedisBudgetStore) WithTTL(ttl time.Duration) *RedisBudgetStore {
	if ttl > 0 {
		s.ttl = ttl
	}
	return s
}

// budgetKey 是单日账面的键。day 必须是调用方算好的 UTC 日期，不在这里换算：
// 换算规则只能有一处，否则预热和记账会算到不同的天。
func budgetKey(day string) string {
	return "llm:budget:" + day
}

// LoadCost 读某一天已花。没有 Redis、没有这个键都当作 0。
func (s *RedisBudgetStore) LoadCost(ctx context.Context, day string) (float64, error) {
	if s.client == nil {
		return 0, nil
	}
	raw, err := s.client.Get(ctx, budgetKey(day)).Result()
	if err != nil {
		// redis.Nil 是"今天还没花钱"，是最常见的正常路径，不该刷日志。
		if err == redis.Nil {
			return 0, nil
		}
		return 0, err
	}
	cost, err := strconv.ParseFloat(raw, 64)
	if err != nil {
		return 0, fmt.Errorf("预算键 %s 的值不是数字（原始值 %q）: %w", budgetKey(day), raw, err)
	}
	return cost, nil
}

// AddCost 记一笔并返回当日总额。
//
// 先 IncrByFloat 再 Expire：每次 add 都续一次期是幂等的，比 Exists+Expire
// 少一次往返；账面的键不会因为"刚好在最后一次记账后过期"而丢掉当天的数。
// Expire 失败只忽略——记账已经成功，TTL 没设上最多让键多留几天，不影响判断。
func (s *RedisBudgetStore) AddCost(ctx context.Context, day string, cost float64) (float64, error) {
	if s.client == nil {
		return cost, nil
	}
	key := budgetKey(day)
	total, err := s.client.IncrByFloat(ctx, key, cost).Result()
	if err != nil {
		return 0, err
	}
	_ = s.client.Expire(ctx, key, s.ttl).Err()
	return total, nil
}
