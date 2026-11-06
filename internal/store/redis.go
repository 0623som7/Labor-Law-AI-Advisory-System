package store

import (
	"context"
	"errors"
	"fmt"

	"github.com/redis/go-redis/v9"

	"laborlaw/internal/config"
)

// OpenRedis 建立客户端并探测一次。
//
// redis.NewClient 同样是懒连接：不 Ping 的话，Redis 挂掉要到第一次取缓存才暴露，
// 而那时用户已经点进了对话页。
func OpenRedis(ctx context.Context, cfg config.RedisConfig) (*redis.Client, error) {
	client := redis.NewClient(&redis.Options{
		Addr:         cfg.Addr,
		Password:     cfg.Password,
		DB:           cfg.DB,
		PoolSize:     cfg.PoolSize,
		DialTimeout:  cfg.DialTimeout,
		ReadTimeout:  checkTimeout,
		WriteTimeout: checkTimeout,
	})

	pingCtx, cancel := context.WithTimeout(ctx, openTimeout)
	defer cancel()
	if err := client.Ping(pingCtx).Err(); err != nil {
		wrapped := fmt.Errorf("连接 Redis 失败: %w", err)
		if closeErr := client.Close(); closeErr != nil {
			return nil, fmt.Errorf("%w（关闭客户端时又出错: %v）", wrapped, closeErr)
		}
		return nil, wrapped
	}
	return client, nil
}

// RedisChecker 是 /readyz 的 Redis 探针。
//
// 注意它是"就绪"而不是"存活"：Redis 挂了应当返回 503 摘流量，但缓存不可用
// 在代码里必须能降级（查库），不能直接 500——这条由各业务 handler 保证。
type RedisChecker struct {
	client *redis.Client
}

// NewRedisChecker 构造探针。
func NewRedisChecker(client *redis.Client) RedisChecker {
	return RedisChecker{client: client}
}

// Name 返回探针名。
func (c RedisChecker) Name() string { return "redis" }

// Check 用一次 PING 判断连接是否可用。
func (c RedisChecker) Check(ctx context.Context) error {
	if c.client == nil {
		return errors.New("redis: 未初始化")
	}
	ctx, cancel := context.WithTimeout(ctx, checkTimeout)
	defer cancel()
	if err := c.client.Ping(ctx).Err(); err != nil {
		return fmt.Errorf("redis ping: %w", err)
	}
	return nil
}
