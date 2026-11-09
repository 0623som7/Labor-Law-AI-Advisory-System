// Package config 把散落在环境变量里的启动参数收敛成一个启动即校验的结构体。
//
// 为什么不用 viper：本项目的配置来源只有环境变量与 .env 两种，viper 的多层优先级
// 与大小写折叠在排障时只会增加猜测成本。这里只做三件事：读、转换、校验。
//
// 校验失败的报错只报变量名、不报变量值——DSN 与 API key 都住在值里，
// 把值打进日志等于把凭据写进磁盘。
package config

import (
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"
)

// Env 是运行环境。production 下的校验比 dev 严格，防止本地调试开关带上线。
type Env string

const (
	// EnvDev 本地开发：允许 AUTH_DEV_USER，日志可以用文本格式。
	EnvDev Env = "dev"
	// EnvStaging 预发：配置与生产一致，但数据可以随便丢。
	EnvStaging Env = "staging"
	// EnvProduction 生产：禁止一切调试开关。
	EnvProduction Env = "production"
)

// minSecretLen 是 HS256 密钥的最小字节数。
//
// 短于这个长度等于没有签名：32 字节是 HMAC-SHA256 的输出长度，
// 再短就会让离线爆破变得可行。
const minSecretLen = 32

// Config 是进程的完整配置。
//
// 分组与 .env.example 的段落一一对应：增删任何一项，两边必须同时改，
// 否则新项会悄悄拿到默认值，而默认值在线上通常是错的。
type Config struct {
	App       AppConfig
	HTTP      HTTPConfig
	Auth      AuthConfig
	MySQL     MySQLConfig
	Redis     RedisConfig
	LLM       LLMConfig
	Storage   StorageConfig
	Worker    WorkerConfig
	File      FileConfig
	Retrieval RetrievalConfig
	Log       LogConfig
}

// AppConfig 是进程身份。
type AppConfig struct {
	// Env 决定校验严格程度与 gin 的运行模式。
	Env Env
	// Name 出现在日志与 /metrics 的标签里，多进程部署时用来区分 api 与 worker。
	Name string
	// Version 是构建版本，验收演示时用它证明跑的是哪个 commit。
	Version string
	// AutoMigrate 为 true 时进程启动即应用未执行的迁移。
	// 只给本地与预发用：生产环境的 schema 变更必须走人工确认，不能随进程重启发生。
	AutoMigrate bool
}

// HTTPConfig 是 http.Server 的超时与限额。
type HTTPConfig struct {
	// Addr 形如 ":8080"。
	Addr string
	// ReadHeaderTimeout 防慢速请求头攻击，这是唯一必须短到秒级的超时。
	ReadHeaderTimeout time.Duration
	// ReadTimeout 覆盖整个请求体的读取，上传大文件时要留够。
	ReadTimeout time.Duration
	// WriteTimeout 默认 0（不设限）。SSE 连接短则几十秒、长则几分钟，
	// 任何有限值都会把长回答掐断；长连接的收敛改由 handler 的 ctx 负责。
	WriteTimeout time.Duration
	// IdleTimeout 控制 keep-alive 空闲连接的回收。
	IdleTimeout time.Duration
	// ShutdownTimeout 是收到 SIGTERM 后等待在途请求完成的上限。
	ShutdownTimeout time.Duration
	// MaxBodyBytes 是请求体硬上限，必须大于 STORAGE_MAX_UPLOAD_MB。
	MaxBodyBytes int64
	// TrustedProxies 是允许通过 X-Forwarded-For 声明客户端 IP 的代理地址。
	//
	// 空表示不信任任何代理，ClientIP() 退回 RemoteAddr。gin 的默认行为是信任
	// 0.0.0.0/0，也就是任何一个客户端都能用 XFF 伪造自己的来源 IP——限流按 IP
	// 计数时，这意味着换一个头就换一个计数桶。反过来，挂了 nginx 却谁都不信，
	// 所有人会被认成同一个 127.0.0.1，同样把限流打成一锅粥。
	TrustedProxies []string
}

// AuthConfig 是令牌签发参数。
type AuthConfig struct {
	// Secret 是 HS256 的密钥，至少 32 字节。
	Secret string
	// Issuer 写进 JWT 的 iss，多环境共用一个 Redis 时用来区分。
	Issuer string
	// AccessTTL 是 access token 有效期。
	AccessTTL time.Duration
	// RefreshTTL 是 refresh token 有效期，必须大于 AccessTTL。
	RefreshTTL time.Duration
	// DevUser 非空时跳过鉴权、直接以该用户身份处理请求。
	// production 下 Validate 会拒绝它：这是本地开发的便利，不是部署选项。
	DevUser string
	// LoginRateLimit 是登录接口每 IP 每分钟的尝试次数，防撞库。
	LoginRateLimit int
}

// MySQLConfig 是数据库连接池参数。
type MySQLConfig struct {
	// DSN 是完整连接串，含密码，任何时候都不许打进日志。
	DSN string
	// MaxOpenConns 是连接上限。2 核机器上超过 50 只会让 MySQL 上下文切换变频繁。
	MaxOpenConns int
	// MaxIdleConns 是空闲连接数，必须小于等于 MaxOpenConns。
	MaxIdleConns int
	// ConnMaxLifetime 要短于 MySQL 的 wait_timeout，否则会拿到已被服务端关闭的连接。
	ConnMaxLifetime time.Duration
}

// RedisConfig 是缓存连接参数。
type RedisConfig struct {
	// Addr 形如 "127.0.0.1:6379"。
	Addr string
	// Password 为空表示无密码；同样不许进日志。
	Password string
	// DB 是逻辑库编号。
	DB int
	// PoolSize 是连接池大小。
	PoolSize int
	// DialTimeout 是建连超时，Redis 不可用时要快速失败而不是拖住请求。
	DialTimeout time.Duration
}

// LLMConfig 是模型网关参数。
//
// 这些值全部由 internal/llm 消费。单价与预算也放这里，是因为成本闸必须读配置，
// 硬编码的单价在涨价那天会让所有人一脸茫然。
type LLMConfig struct {
	// BaseURL 是 OpenAI 兼容网关地址。
	BaseURL string
	// APIKey 由 B 填写，绝不入库、绝不进日志。
	APIKey string
	// ModelConsult 是 consult 场景的模型名。
	ModelConsult string
	// ModelExtract 是 review.extract 场景的模型名，走便宜档。
	ModelExtract string
	// ModelAdvise 是 review.advise 场景的模型名，走强档。
	ModelAdvise string
	// ModelEmbedding 是向量化模型名。换它必须整体重建向量，不能与旧向量混用：
	// 不同模型的向量空间互不相通，混着算余弦得到的排序没有任何意义。
	ModelEmbedding string
	// Timeout 是单次调用上限（不含重试等待）。非流式调用用它。
	Timeout time.Duration
	// StreamTimeout 是流式生成的整体上限。
	//
	// 流式响应是一个持续吐字的过程，耗时随回答长度增长（实测三轮咨询 35.6s /
	// 44.2s / 51.7s），用 Timeout 这种"单次往返"口径当上限，会在长回答上把
	// 连接整段掐断。所以单独给一个更大的值，且必须不小于 Timeout。
	StreamTimeout time.Duration
	// MaxRetries 是上游可重试错误的额外尝试次数。
	MaxRetries int
	// RetryBackoff 是首次重试前的等待，之后每次翻倍（上限 8s）。
	// 放配置而不是写死：本地打假上游时要把它调成 0，否则测试会慢得没法看。
	RetryBackoff time.Duration
	// PriceInputPer1K 是每千输入 token 的单价（元），用于 CostEst。
	PriceInputPer1K float64
	// PriceOutputPer1K 是每千输出 token 的单价（元）。
	PriceOutputPer1K float64
	// QPS 是全局令牌桶速率，防止并发一高就把预算烧穿。
	QPS float64
	// DailyBudgetYuan 是日成本硬闸，超过即降级到 ModelExtract 并告警。
	DailyBudgetYuan float64
}

// StorageConfig 是合同文件的落盘参数。
type StorageConfig struct {
	// Root 是文件根目录，映射进容器的 volume。
	Root string
	// MaxUploadMB 是单文件上限。
	MaxUploadMB int
	// MaxPages 是页数上限，2G 内存解析超长 PDF 的峰值不可控。
	MaxPages int
}

// WorkerConfig 是异步任务池参数。
type WorkerConfig struct {
	// PoolSize 是并发处理任务的 goroutine 数。2 核上解析类任务只该开 1-2 个。
	PoolSize int
	// TaskTimeout 是单个任务的上限，超时由 ctx 取消并回写 failed。
	TaskTimeout time.Duration
	// QueueSize 是等待队列长度，满了立即返回 42901 而不是无限堆内存。
	QueueSize int
}

// FileConfig 是 PDF 解析参数。
type FileConfig struct {
	// MinConfidence 是"有文本层"的最低置信度，低于它返回 42201 让前端走 OCR。
	MinConfidence float64
	// MaxBlocksPerPage 是单页文本块上限，防畸形 PDF 撑爆内存。
	MaxBlocksPerPage int
	// PdfToTextPath 是 pdftotext 可执行文件路径。默认走 PATH；
	// Windows 上 poppler 装完不一定进 PATH，需要填绝对路径。
	PdfToTextPath string
	// ExtractTimeout 是单份 PDF 的解析预算，超时返回 50401。
	ExtractTimeout time.Duration
}

// RetrievalConfig 是检索策略开关。
type RetrievalConfig struct {
	// Semantic 控制咨询链路是否使用向量召回。默认 false：
	// 向量库要先灌、模型名要能对上，灰度期开着只会让每次问答多一次网络往返。
	Semantic bool
}

// LogConfig 是日志输出参数。
type LogConfig struct {
	// Level 取 debug / info / warn / error。
	Level string
	// Format 取 text / json；生产必须是 json，否则日志聚合解析不了。
	Format string
}

// Load 从进程环境变量读取配置。
//
// 只在 cmd/ 的 main 里调用一次：库代码拿到的永远是已经校验过的 Config，
// 不允许某个包自己再去读环境变量，否则同一份配置会出现两个来源。
func Load() (Config, error) {
	return LoadFrom(os.Getenv)
}

// LoadFrom 用一个可注入的取值函数读取配置，供测试在不改进程环境的情况下覆盖每一项。
//
// 先收集全部读取错误再一次性返回：缺三个变量时报三次比"修一个重启一次"省时间。
func LoadFrom(get func(string) string) (Config, error) {
	l := &loader{get: get}

	cfg := Config{
		App: AppConfig{
			Env:         Env(l.str("APP_ENV", string(EnvDev))),
			Name:        l.str("APP_NAME", "laborlaw-api"),
			Version:     l.str("APP_VERSION", "dev"),
			AutoMigrate: l.boolean("AUTO_MIGRATE", false),
		},
		HTTP: HTTPConfig{
			Addr:              l.str("HTTP_ADDR", ":8080"),
			ReadHeaderTimeout: l.dur("HTTP_READ_HEADER_TIMEOUT", 5*time.Second),
			ReadTimeout:       l.dur("HTTP_READ_TIMEOUT", 30*time.Second),
			WriteTimeout:      l.durAllowZero("HTTP_WRITE_TIMEOUT", 0),
			IdleTimeout:       l.dur("HTTP_IDLE_TIMEOUT", 120*time.Second),
			ShutdownTimeout:   l.dur("HTTP_SHUTDOWN_TIMEOUT", 15*time.Second),
			MaxBodyBytes:      int64(l.num("HTTP_MAX_BODY_BYTES", 12<<20, 1<<20, 64<<20)),
			TrustedProxies:    l.list("TRUSTED_PROXIES", []string{"127.0.0.1", "::1"}),
		},
		Auth: AuthConfig{
			Secret:         l.req("JWT_SECRET"),
			Issuer:         l.str("JWT_ISSUER", "laborlaw"),
			AccessTTL:      l.dur("JWT_ACCESS_TTL", 2*time.Hour),
			RefreshTTL:     l.dur("JWT_REFRESH_TTL", 30*24*time.Hour),
			DevUser:        l.str("AUTH_DEV_USER", ""),
			LoginRateLimit: l.num("AUTH_LOGIN_RATE_LIMIT", 5, 1, 100),
		},
		MySQL: MySQLConfig{
			DSN:             l.req("MYSQL_DSN"),
			MaxOpenConns:    l.num("MYSQL_MAX_OPEN_CONNS", 20, 1, 200),
			MaxIdleConns:    l.num("MYSQL_MAX_IDLE_CONNS", 5, 0, 200),
			ConnMaxLifetime: l.dur("MYSQL_CONN_MAX_LIFETIME", 30*time.Minute),
		},
		Redis: RedisConfig{
			Addr:        l.req("REDIS_ADDR"),
			Password:    l.str("REDIS_PASSWORD", ""),
			DB:          l.num("REDIS_DB", 0, 0, 15),
			PoolSize:    l.num("REDIS_POOL_SIZE", 10, 1, 200),
			DialTimeout: l.dur("REDIS_DIAL_TIMEOUT", 2*time.Second),
		},
		LLM: LLMConfig{
			BaseURL:          l.str("LLM_BASE_URL", "https://api.deepseek.com/v1"),
			APIKey:           l.str("LLM_API_KEY", ""),
			ModelConsult:     l.str("LLM_MODEL_CONSULT", "deepseek-chat"),
			ModelExtract:     l.str("LLM_MODEL_EXTRACT", "deepseek-chat"),
			ModelAdvise:      l.str("LLM_MODEL_ADVISE", "deepseek-chat"),
			ModelEmbedding:   l.str("LLM_EMBEDDING_MODEL", "text-embedding-v3"),
			Timeout:          l.dur("LLM_TIMEOUT", 60*time.Second),
			StreamTimeout:    l.dur("LLM_STREAM_TIMEOUT", 300*time.Second),
			MaxRetries:       l.num("LLM_MAX_RETRIES", 2, 0, 5),
			RetryBackoff:     l.durAllowZero("LLM_RETRY_BACKOFF", 300*time.Millisecond),
			PriceInputPer1K:  l.frac("LLM_PRICE_INPUT_PER_1K", 0.001, 0, 1000),
			PriceOutputPer1K: l.frac("LLM_PRICE_OUTPUT_PER_1K", 0.002, 0, 1000),
			QPS:              l.frac("LLM_QPS", 5, 0.1, 1000),
			DailyBudgetYuan:  l.frac("LLM_DAILY_BUDGET_YUAN", 20, 0.1, 100000),
		},
		Storage: StorageConfig{
			Root:        l.str("STORAGE_ROOT", "./storage"),
			MaxUploadMB: l.num("STORAGE_MAX_UPLOAD_MB", 5, 1, 50),
			MaxPages:    l.num("STORAGE_MAX_PAGES", 50, 1, 200),
		},
		Worker: WorkerConfig{
			PoolSize:    l.num("WORKER_POOL_SIZE", 2, 1, 64),
			TaskTimeout: l.dur("WORKER_TASK_TIMEOUT", 120*time.Second),
			QueueSize:   l.num("WORKER_QUEUE_SIZE", 100, 0, 10000),
		},
		File: FileConfig{
			MinConfidence:    l.frac("FILE_MIN_CONFIDENCE", 0.6, 0, 1),
			MaxBlocksPerPage: l.num("FILE_MAX_BLOCKS_PER_PAGE", 500, 1, 10000),
			PdfToTextPath:    l.str("FILE_PDFTOTEXT_PATH", "pdftotext"),
			ExtractTimeout:   l.dur("FILE_EXTRACT_TIMEOUT", 30*time.Second),
		},
		Retrieval: RetrievalConfig{
			Semantic: l.boolean("RETRIEVAL_SEMANTIC", false),
		},
		Log: LogConfig{
			Level:  strings.ToLower(l.str("LOG_LEVEL", "info")),
			Format: strings.ToLower(l.str("LOG_FORMAT", "text")),
		},
	}

	if err := l.err(); err != nil {
		return Config{}, err
	}
	if err := cfg.Validate(); err != nil {
		return Config{}, err
	}
	return cfg, nil
}

// Validate 做跨字段一致性检查，读取阶段做不了的那些。
//
// 单独暴露是为了让测试能直接构造 Config 字面量来验证规则，
// 不必绕一圈环境变量。
func (c Config) Validate() error {
	var problems []string
	add := func(format string, args ...any) {
		problems = append(problems, fmt.Sprintf(format, args...))
	}

	switch c.App.Env {
	case EnvDev, EnvStaging, EnvProduction:
	default:
		add("APP_ENV 必须是 dev / staging / production 之一")
	}

	if len(c.Auth.Secret) < minSecretLen {
		add("JWT_SECRET 长度至少 %d 字节", minSecretLen)
	}
	if c.Auth.AccessTTL >= c.Auth.RefreshTTL {
		add("JWT_ACCESS_TTL 必须小于 JWT_REFRESH_TTL")
	}
	if c.LLM.StreamTimeout < c.LLM.Timeout {
		add("LLM_STREAM_TIMEOUT 不能小于 LLM_TIMEOUT（流式覆盖整段生成，必须留得更宽）")
	}
	if c.MySQL.MaxIdleConns > c.MySQL.MaxOpenConns {
		add("MYSQL_MAX_IDLE_CONNS 不能大于 MYSQL_MAX_OPEN_CONNS")
	}
	if c.HTTP.MaxBodyBytes < int64(c.Storage.MaxUploadMB)<<20 {
		add("HTTP_MAX_BODY_BYTES 必须不小于 STORAGE_MAX_UPLOAD_MB")
	}
	if c.File.MinConfidence < 0 || c.File.MinConfidence > 1 {
		add("FILE_MIN_CONFIDENCE 必须在 0..1 之间")
	}

	switch c.Log.Level {
	case "debug", "info", "warn", "error":
	default:
		add("LOG_LEVEL 必须是 debug / info / warn / error 之一")
	}
	switch c.Log.Format {
	case "text", "json":
	default:
		add("LOG_FORMAT 必须是 text / json 之一")
	}

	if c.App.Env == EnvProduction {
		if c.Auth.DevUser != "" {
			add("生产环境禁止 AUTH_DEV_USER")
		}
		if c.Log.Format != "json" {
			add("生产环境 LOG_FORMAT 必须是 json")
		}
		if c.App.AutoMigrate {
			add("生产环境禁止 AUTO_MIGRATE：schema 变更必须人工确认")
		}
	}

	if len(problems) > 0 {
		return fmt.Errorf("配置校验失败: %s", strings.Join(problems, "；"))
	}
	return nil
}

// loader 负责读取与收集问题，把"缺一项就 return"改写成"读完再报"。
type loader struct {
	get      func(string) string
	problems []string
}

func (l *loader) fail(format string, args ...any) {
	l.problems = append(l.problems, fmt.Sprintf(format, args...))
}

func (l *loader) err() error {
	if len(l.problems) == 0 {
		return nil
	}
	return fmt.Errorf("配置读取失败: %s", strings.Join(l.problems, "；"))
}

// str 返回去空白后的值，空值用默认值。默认值只对非敏感项有意义。
func (l *loader) str(name, def string) string {
	if v := strings.TrimSpace(l.get(name)); v != "" {
		return v
	}
	return def
}

// req 读必填项。缺失只报变量名，不回显任何值。
func (l *loader) req(name string) string {
	v := strings.TrimSpace(l.get(name))
	if v == "" {
		l.fail("%s 必填", name)
	}
	return v
}

// dur 读正时长。0 与负数都算错——"永不超时"必须是显式选择，而不是漏填的后果。
func (l *loader) dur(name string, def time.Duration) time.Duration {
	raw := strings.TrimSpace(l.get(name))
	if raw == "" {
		return def
	}
	d, err := time.ParseDuration(raw)
	if err != nil || d <= 0 {
		l.fail("%s 不是合法的正时长（如 5s、1m30s）", name)
		return def
	}
	return d
}

// durAllowZero 用于 HTTP_WRITE_TIMEOUT 这类"0 表示不限"的项。
func (l *loader) durAllowZero(name string, def time.Duration) time.Duration {
	raw := strings.TrimSpace(l.get(name))
	if raw == "" {
		return def
	}
	d, err := time.ParseDuration(raw)
	if err != nil || d < 0 {
		l.fail("%s 不是合法的时长（如 5s、0s）", name)
		return def
	}
	return d
}

// num 读闭区间整数。
func (l *loader) num(name string, def, min, max int) int {
	raw := strings.TrimSpace(l.get(name))
	if raw == "" {
		return def
	}
	n, err := strconv.Atoi(raw)
	if err != nil {
		l.fail("%s 必须是整数", name)
		return def
	}
	if n < min || n > max {
		l.fail("%s 必须在 %d..%d 之间", name, min, max)
		return def
	}
	return n
}

// frac 读闭区间小数，用于 QPS、置信度这类带小数的项。
func (l *loader) frac(name string, def, min, max float64) float64 {
	raw := strings.TrimSpace(l.get(name))
	if raw == "" {
		return def
	}
	f, err := strconv.ParseFloat(raw, 64)
	if err != nil {
		l.fail("%s 必须是小数", name)
		return def
	}
	if f < min || f > max {
		l.fail("%s 必须在 %g..%g 之间", name, min, max)
		return def
	}
	return f
}

// list 读逗号分隔的列表。空项被丢掉，"a,,b" 与 "a,b" 等价。
// 整项缺失或为空时返回默认值——"不信任任何代理"要用显式的 none 语义，
// 不能靠一个漏写的变量达成，那会让部署时的笔误变成安全默认值反转。
func (l *loader) list(name string, def []string) []string {
	raw := strings.TrimSpace(l.get(name))
	if raw == "" {
		return def
	}
	parts := strings.Split(raw, ",")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	if len(out) == 0 {
		return def
	}
	return out
}

// boolean 接受 true/false/1/0，以及 on/off 这对同义词。
//
// on/off 是给开关类配置留的：RETRIEVAL_SEMANTIC=off 这种写法在运维眼里就是布尔值，
// 而把这个键写成运维习惯的样子却启动失败，是最不好解释的一类故障。
// yes/no 仍然当错误：环境变量文件是人手写的，含糊的取值在排查时最费时间。
func (l *loader) boolean(name string, def bool) bool {
	raw := strings.ToLower(strings.TrimSpace(l.get(name)))
	switch raw {
	case "":
		return def
	case "true", "1", "on":
		return true
	case "false", "0", "off":
		return false
	default:
		l.fail("%s 必须是 true / false / 1 / 0 / on / off", name)
		return def
	}
}
