// Package api 组装 HTTP 层：路由、中间件、响应包络与各业务 handler。
//
// 本包只做三件事：把 HTTP 请求翻译成 port 层的调用、把错误翻译成状态码与业务码、
// 把结果套进统一包络。业务规则一行都不写在这里——判规则的活在 internal/rules，
// 编排的活在 internal/agent；写在这里会让 B 每改一条规则都被迫读 A 的代码。
package api

import (
	"context"
	"log/slog"
	"net/http"

	"github.com/gin-gonic/gin"

	"laborlaw/internal/config"
	"laborlaw/internal/port"
	"laborlaw/internal/stream"
)

// Checker 是 /readyz 的一个依赖探针。
//
// 实现方自己负责超时：探针挂住会把 healthcheck 一起挂住，表现为"容器一直 starting"，
// 比直接失败更难查。API 另外给了整体 3s 预算，两层都要有。
type Checker interface {
	// Name 是 checks 里的键名，如 "mysql"、"redis"。
	Name() string
	// Check 返回 nil 表示可用。不许缓存结果：缓存过的探针只能证明"曾经可用"。
	Check(ctx context.Context) error
}

// API 持有 HTTP 层的全部依赖与路由树。
//
// 字段一律不导出：路由树在 New 里一次建完，运行期没有任何地方应该改它。
type API struct {
	cfg      config.Config
	log      *slog.Logger
	checkers []Checker
	runner   port.Runner
	hub      *stream.Hub
	engine   *gin.Engine
}

// Option 是装配期的可选依赖。
//
// 用 Option 而不是把六个参数塞进 New：依赖会随里程碑增加（下个是 store），
// 每加一个都改调用点签名，会让 main 与测试一起飘。
type Option func(*API)

// WithCheckers 注册 /readyz 探针。
func WithCheckers(cs ...Checker) Option {
	return func(a *API) { a.checkers = append(a.checkers, cs...) }
}

// WithRunner 注入 Agent 编排实现。不注入时 /chat/completions 返回 501。
func WithRunner(r port.Runner) Option {
	return func(a *API) { a.runner = r }
}

// WithHub 注入 SSE 流中心。不注入时 /chat/completions 返回 501——
// 没有 Hub 就没有断线续传，宁可明确失败也不要退化成"能连但续不上"。
func WithHub(h *stream.Hub) Option {
	return func(a *API) { a.hub = h }
}

// New 构造路由树。log 为 nil 时退回 slog.Default，免得测试里到处传 logger。
//
// 这里只做装配，不碰网络、不连数据库：进程能起来但依赖没就绪时，
// /healthz 应当是 200、/readyz 才是 503——启动顺序不该由初始化代码决定。
func New(cfg config.Config, log *slog.Logger, opts ...Option) *API {
	if log == nil {
		log = slog.Default()
	}
	if cfg.App.Env == config.EnvProduction || cfg.App.Env == config.EnvStaging {
		gin.SetMode(gin.ReleaseMode)
	} else {
		gin.SetMode(gin.DebugMode)
	}

	a := &API{cfg: cfg, log: log}
	for _, opt := range opts {
		opt(a)
	}
	a.engine = a.buildRouter()
	return a
}

// Handler 返回可直接交给 http.Server 的 handler。
//
// 返回 http.Handler 而不是 *gin.Engine：调用方（main 与测试）不需要知道底层框架，
// 将来换框架时改这一个签名即可。
func (a *API) Handler() http.Handler {
	return a.engine
}
