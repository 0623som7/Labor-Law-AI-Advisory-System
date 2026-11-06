package api

import (
	"context"
	"errors"
	"time"

	"github.com/redis/go-redis/v9"
)

// errAuthCacheDown 表示缓存不可用（未注入、连接失败）。
//
// 它与"键不存在"必须分开：登录限流在缓存挂掉时应当降级放行，
// 而 refresh 白名单在缓存挂掉时必须拒绝——两种语义混成一个错误，
// 就会出现"Redis 抖一下，所有续期都变成 401"或者"Redis 挂了，白名单形同虚设"。
var errAuthCacheDown = errors.New("auth: 缓存不可用")

// authCache 是账号域用到的缓存能力子集。
//
// 抽成接口而不是直接依赖 *redis.Client：登录限流与 refresh 轮换都有
// "缓存不可用"的分支，用真实 Redis 测这两条分支需要起容器；接口让测试
// 用一个内存实现就能覆盖轮换与限流，而生产实现仍是 go-redis。
type authCache interface {
	// get 取值。found=false 表示键不存在（不是错误）。
	get(ctx context.Context, key string) (value string, found bool, err error)
	// getdel 原子地取值并删除。found=false 表示键本来就不存在。
	//
	// refresh 轮换必须用它：先 get 再 del 是两步，两个并发请求可以都通过 get、
	// 都拿到"白名单命中"的结论，于是同一枚令牌换出两条续期链——轮换等于没做。
	getdel(ctx context.Context, key string) (value string, found bool, err error)
	// set 写入并设置过期。
	set(ctx context.Context, key, value string, ttl time.Duration) error
	// del 删除若干键，键不存在不算错误。
	del(ctx context.Context, keys ...string) error
	// incr 自增并返回新值。
	incr(ctx context.Context, key string) (int64, error)
	// expire 设置过期时间。
	expire(ctx context.Context, key string, ttl time.Duration) error
}

// redisAuthCache 是 authCache 的 Redis 实现。
//
// client 为 nil 时所有方法返回 errAuthCacheDown 而不是 panic：
// dev 无 Redis 模式是一种合法启动形态（见 cmd/api 的降级逻辑）。
type redisAuthCache struct {
	client *redis.Client
}

// newRedisAuthCache 包装客户端。client 为 nil 时返回 nil 接口，
// 由调用方按"无缓存"处理——这比返回一个每次都报错的实现更直接，
// 因为这里的调用方本来就要写降级分支。
func newRedisAuthCache(client *redis.Client) authCache {
	if client == nil {
		return nil
	}
	return &redisAuthCache{client: client}
}

func (c *redisAuthCache) get(ctx context.Context, key string) (string, bool, error) {
	if c == nil || c.client == nil {
		return "", false, errAuthCacheDown
	}
	val, err := c.client.Get(ctx, key).Result()
	if errors.Is(err, redis.Nil) {
		return "", false, nil
	}
	if err != nil {
		return "", false, err
	}
	return val, true, nil
}

// getdel 用 Redis 的 GETDEL：取值与删除在服务端一次完成，
// 并发请求里只有一个能拿到值，其余拿到 redis.Nil。
func (c *redisAuthCache) getdel(ctx context.Context, key string) (string, bool, error) {
	if c == nil || c.client == nil {
		return "", false, errAuthCacheDown
	}
	val, err := c.client.GetDel(ctx, key).Result()
	if errors.Is(err, redis.Nil) {
		return "", false, nil
	}
	if err != nil {
		return "", false, err
	}
	return val, true, nil
}

func (c *redisAuthCache) set(ctx context.Context, key, value string, ttl time.Duration) error {
	if c == nil || c.client == nil {
		return errAuthCacheDown
	}
	// ttl<=0 时 Set 会写成永久键，refresh 白名单变成永不过期——
	// 一条被吊销的令牌就再也清不掉了，所以这里直接拒绝。
	if ttl <= 0 {
		return errors.New("auth: 缓存 TTL 必须为正")
	}
	return c.client.Set(ctx, key, value, ttl).Err()
}

func (c *redisAuthCache) del(ctx context.Context, keys ...string) error {
	if c == nil || c.client == nil {
		return errAuthCacheDown
	}
	if len(keys) == 0 {
		return nil
	}
	return c.client.Del(ctx, keys...).Err()
}

func (c *redisAuthCache) incr(ctx context.Context, key string) (int64, error) {
	if c == nil || c.client == nil {
		return 0, errAuthCacheDown
	}
	return c.client.Incr(ctx, key).Result()
}

func (c *redisAuthCache) expire(ctx context.Context, key string, ttl time.Duration) error {
	if c == nil || c.client == nil {
		return errAuthCacheDown
	}
	return c.client.Expire(ctx, key, ttl).Err()
}

// 缓存键约定。集中在这里，避免某处写 "auth:refresh:" 另一处写 "auth:refresh_"
// 导致轮换写出一个永远查不到的白名单项。
const (
	// keyLoginAttempts 是登录失败计数的前缀，后面接客户端 IP。
	keyLoginAttempts = "auth:login:"
	// keyRegisterAttempts 是注册尝试计数的前缀。
	//
	// 注册要跑 cost 12 的 bcrypt（约 250ms CPU），不限流的话单 IP 每秒几个
	// 请求就能占满一个核——这是比登录更容易被利用的放大器。
	keyRegisterAttempts = "auth:register:"
	// keyRefreshToken 是 refresh 白名单前缀，后面接 jti，值为 uid。
	keyRefreshToken = "auth:refresh:"
	// keyUserRefresh 是"用户当前有效的 refresh jti"前缀，后面接 uid。
	//
	// 为什么要这条反查键：登出时手里只有 access token（其 sub 是 uid），
	// 没有 refresh 的 jti，没有反查就只能把该用户全部 refresh 都留着。
	// 单键意味着一个用户同时只有一条有效续期链——第二台设备登录会顶掉第一台
	// （issueTokens 先注销旧 jti 再写新链，顺序不能反），这是 MVP 阶段有意的选择，
	// 换来的是登出能真正吊销。
	keyUserRefresh = "auth:refresh:uid:"
)
