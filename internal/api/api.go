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
	"github.com/redis/go-redis/v9"

	"laborlaw/internal/config"
	"laborlaw/internal/file"
	"laborlaw/internal/port"
	"laborlaw/internal/storage"
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

	// 文件链路的三个依赖。分开注入而不是塞一个结构体：
	// 测试与 dev 无存储模式各自缺的只是其中一段，打包会让"缺哪一段"变成布尔猜谜。
	store     port.Store
	blobs     *storage.Store
	extractor *file.Extractor

	// 账号域。users 与 store 分开：UserStore 是独立接口（端口草案未挂进 Store），
	// 且它有自己的密钥依赖，跟着 store 走会被迫在无密钥环境里也构造一遍。
	users port.UserStore
	// cache 承载登录限流与 refresh 白名单。nil 表示无缓存：限流放行、续期拒绝。
	cache authCache

	// statutes 是法条检索器。与 store 分开注入：检索要过一层口语归一
	// （internal/retrieval，"加班费"→"加班"/"延长工作时间"），
	// 直接拿 store.SearchText 查整句会绕过归一，召回率掉到看起来像"库里没有"。
	statutes statuteSearcher

	// 审查链路的两段：submitter 把任务送进 worker 池，reviewHandler 是编排层
	// （internal/agent）给出的真正干活函数。两者分开注入，是因为它们在测试里
	// 需要各自被替换——一个假实现 Submit 就能验证 202 载荷，不必拉起整个池。
	submitter     taskSubmitter
	reviewHandler port.Handler

	engine *gin.Engine
}

// taskSubmitter 是任务入池能力，*worker.Pool 满足它。
//
// 不直接依赖 *worker.Pool 的原因：api 一旦 import worker，测试就得连池的
// 生命周期一起管理（Submit/Shutdown/队列满），而 handler 只关心"能不能塞进去"。
type taskSubmitter interface {
	Submit(ctx context.Context, t *port.Task, h port.Handler) error
}

// statuteSearcher 是 /statutes 需要的检索能力，*retrieval.Retriever 满足它。
//
// 声明窄接口而不是直接依赖具体类型：handler 只用得上"一句话进、条文出"，
// 测试也就能给个假实现，不必为此拉起真实 DAL。
type statuteSearcher interface {
	SearchIn(ctx context.Context, input, law string, topK int) ([]port.StatuteHit, error)
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

// WithStore 注入持久层。不注入（dev 无存储模式）时 /files/* 返回 501。
func WithStore(s port.Store) Option {
	return func(a *API) { a.store = s }
}

// WithStorage 注入合同原文落盘层。不注入时 /files/* 返回 501。
func WithStorage(s *storage.Store) Option {
	return func(a *API) { a.blobs = s }
}

// WithExtractor 注入 PDF 解析器。不注入时 /files/* 返回 501。
//
// 与 Store / Storage 分开注入：解析器依赖外部二进制（pdftotext），
// 它可以单独换成别的实现或换成桩，而落盘与数据库不该跟着一起动。
func WithExtractor(e *file.Extractor) Option {
	return func(a *API) { a.extractor = e }
}

// WithUsers 注入账号存储。不注入时 /auth/register、/auth/login 返回 50001
// （账号服务未就绪）——注册与登录没有"降级实现"可言，编一个假用户比报错更危险。
func WithUsers(u port.UserStore) Option {
	return func(a *API) { a.users = u }
}

// WithStatutes 注入法条检索器。不注入时 /statutes 返回 501。
func WithStatutes(s statuteSearcher) Option {
	return func(a *API) { a.statutes = s }
}

// WithTaskSubmitter 注入任务入池能力。不注入时 /contracts/review 返回 501。
func WithTaskSubmitter(s taskSubmitter) Option {
	return func(a *API) { a.submitter = s }
}

// WithReviewHandler 注入审查编排。不注入时 /contracts/review 返回 501。
//
// 缺任何一个依赖都返回 501 而不是 500：这是装配缺失，不是运行期故障，
// 让调用方一眼看出"这条链路还没接上"比一条含糊的 50001 有用得多。
func WithReviewHandler(h port.Handler) Option {
	return func(a *API) { a.reviewHandler = h }
}

// WithRedis 注入缓存客户端，用于登录限流与 refresh 白名单。
//
// 不注入（dev 无 Redis）不是致命错误：限流转为放行，续期接口明确失败，
// 其余接口不受影响。传 nil 客户端等同于不注入。
func WithRedis(client *redis.Client) Option {
	return func(a *API) { a.cache = newRedisAuthCache(client) }
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
