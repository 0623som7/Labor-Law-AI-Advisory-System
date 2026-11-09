// Command api 是 HTTP 进程入口：装配配置、日志、存储与路由，然后进入优雅关闭循环。
//
// 这里几乎没有业务代码是有意的：进程层只负责"把依赖装起来、把信号接住"，
// 每一个 handler 都能在 httptest 里单独跑起来，不依赖这个文件。
package main

import (
	"context"
	"database/sql"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/redis/go-redis/v9"

	"laborlaw/internal/agent"
	"laborlaw/internal/api"
	"laborlaw/internal/config"
	"laborlaw/internal/extract"
	"laborlaw/internal/file"
	"laborlaw/internal/llm"
	"laborlaw/internal/storage"
	"laborlaw/internal/store"
	"laborlaw/internal/stream"
	"laborlaw/internal/worker"
)

func main() {
	// -migrate：只跑迁移然后退出，供部署时单独执行一次。
	// 生产环境禁了 AUTO_MIGRATE（schema 变更必须人工确认），但迁移本身总要有人跑，
	// 所以给它一个显式入口，而不是让运维去装 mysql 客户端手敲 SQL。
	migrateOnly := flag.Bool("migrate", false, "只执行数据库迁移后退出")
	flag.Parse()

	if err := run(*migrateOnly); err != nil {
		// 启动期失败时 logger 本身可能还没建起来（日志配置就在配置里），
		// 直接写 stderr 是唯一保证能看到错误的通道。
		fmt.Fprintf(os.Stderr, "api 启动失败: %v\n", err)
		os.Exit(1)
	}
}

// run 是 main 的唯一逻辑体，返回 error 而不是在内部 os.Exit，
// 这样 defer 能正常执行，也便于将来加集成测试。
func run(migrateOnly bool) error {
	// .env 只补缺：容器里注入的真实环境变量优先级永远更高。
	loaded, err := config.LoadDotEnv(".env")
	if err != nil {
		return fmt.Errorf("读取 .env: %w", err)
	}

	cfg, err := config.Load()
	if err != nil {
		return fmt.Errorf("加载配置: %w", err)
	}

	logger := newLogger(cfg.Log)

	// signal.NotifyContext 把 SIGINT / SIGTERM 折成一个 ctx.Done()：
	// 比手写 channel + goroutine 少一处泄漏点，stop 由 defer 回收。
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	// 迁移模式下不要顺手再触发一次自动迁移：两条路径同时跑，日志会分不清是谁做的。
	if migrateOnly {
		cfg.App.AutoMigrate = false
	}

	inf, err := setupInfra(ctx, cfg, logger)
	if err != nil {
		return err
	}
	defer inf.shutdown(logger)

	if migrateOnly {
		if inf.db == nil {
			return errors.New("--migrate 需要可用的 MySQL 连接，请检查 MYSQL_DSN")
		}
		applied, mErr := store.Migrate(ctx, inf.db)
		if mErr != nil {
			return fmt.Errorf("执行迁移: %w", mErr)
		}
		logger.Info("迁移完成", "applied", applied)
		return nil
	}

	// 文件链路：store / storage / extractor 三段分别注入。
	// 缺任一段时对应路由返回 501，而不是带着半条链路起服务。
	extractor := file.NewExtractor(cfg)
	// 启动自检一次 pdftotext：它装错（xpdf 冒充 poppler）时，上传接口会把每个
	// PDF 都判成"解析失败"，而那条错误只会在真上传时才出现。这里只 Warn 不拦启动：
	// dev 环境下即使没装 poppler，其余接口仍要能用。
	if err := extractor.CheckBinary(ctx); err != nil {
		logger.Warn("pdftotext 自检未通过，PDF 上传将无法解析", "err", err.Error())
	}

	// 没有 MySQL 时计量无处可写：传 nil 让 llm 只累加进程内预算，
	// 而不是每轮问答都打一条"记录模型用量失败"的告警。
	var usageRec llm.UsageRecorder
	var agg *store.DB
	if inf.db != nil {
		// 账号 DAL 需要 JWT 密钥做手机号 HMAC：把密钥随聚合体一起构造，
		// 事务回调里拿到的 Users 才不会丢密钥（见 store.NewWithSecret 的注释）。
		agg = store.NewWithSecret(inf.db, cfg.Auth.Secret)
		usageRec = agg.Usage()
	}

	// 全进程只构造一个 llm.Client：它内部带令牌桶与日预算闸，两个实例会让
	// LLM_QPS 与 LLM_DAILY_BUDGET_YUAN 在配置不变的情况下翻倍。
	// 没有密钥时不构造，让 newRunner 走演示桩（见 cmd/api/runner.go）。
	var llmClient *llm.Client
	if cfg.LLM.APIKey != "" {
		llmClient = newLLM(cfg, logger, usageRec)
	}

	// 检索器只构造一次：编排层（Runner）与 /statutes 接口必须用同一套口语归一规则，
	// 各造一个的话改词表只会生效一半，而这类不一致在演示时看不出来。
	searcher := newSearcher(ctx, inf.db, inf.rdb, cfg, logger)
	opts := []api.Option{
		api.WithCheckers(inf.checkers...),
		api.WithRunner(newRunner(logger, llmClient, searcher, newHistory(inf.db))),
		api.WithHub(stream.NewHub(stream.Options{}, 0, logger)),
		api.WithExtractor(extractor),
	}
	// 只有客户端真实存在时才注入计量：把 nil 的 *llm.Client 传进接口参数会得到
	// 非 nil 的接口值，/metrics 里 a.llmStats != nil 成立，随即调 Budget() 空指针。
	if llmClient != nil {
		opts = append(opts, api.WithLLMStats(llmClient))
	}
	if searcher != nil {
		opts = append(opts, api.WithStatutes(searcher))
	}
	// 无存储模式（dev 连不上 MySQL）不注入 Store：让 /files/* 明确返回 501，
	// 而不是在 handler 里对一个 nil 接口发请求。
	if agg != nil {
		opts = append(opts, api.WithStore(agg), api.WithUsers(agg.Users()))

		// 任务池与审查 handler 都依赖 DB，只能装在这里：
		// 无存储模式下 /contracts/review 继续返回 501 才是正确的。
		pool := worker.New(cfg.Worker, logger)
		pool.SetProgressSink(func(ctx context.Context, taskID string, percent int, stage string) error {
			return agg.Task().UpdateProgress(ctx, taskID, percent, stage)
		})
		// 上次进程被 kill 时正在跑的任务不会再有人推进它：启动时统一收尾，
		// 否则用户会拿着一个永远 running 的 task_id 轮询。
		if n, err := agg.Task().FailRunning(ctx, "服务重启，任务中断"); err != nil {
			logger.Warn("清理遗留 running 任务失败", "err", err.Error())
		} else if n > 0 {
			logger.Info("已把遗留的 running 任务标记为失败", "count", n)
		}
		// defer 在 run() 返回时执行，此时 srv 已 Shutdown 完：先停 HTTP 再停池，
		// 不会出现"接口还在收请求、池已经关了"的窗口。
		defer pool.Shutdown(context.Background())

		opts = append(opts, api.WithTaskSubmitter(pool))
		// 审查要靠模型抽取要素：没有客户端时装上去只会每次 50001，
		// 不如不装——接口退回 501"尚未实现"，语义上就是"这台机器没接模型"。
		if llmClient != nil {
			reviewHandler := agent.NewReviewHandler(agent.ReviewDeps{
				Files:     agg.File(),
				Reviews:   agg.Review(),
				Statutes:  agg.Statute(),
				Extractor: extract.New(llmClient),
				Logger:    logger,
			})
			opts = append(opts, api.WithReviewHandler(reviewHandler))
		}
	}
	// Redis 只影响限流与续期：没起 Redis 时 WithRedis(nil) 等价于不注入，
	// 登录照常、续期明确失败，不会把整个服务拖成不可用。
	opts = append(opts, api.WithRedis(inf.rdb))
	blobs, err := storage.New(cfg.Storage.Root)
	if err != nil {
		return fmt.Errorf("初始化存储目录: %w", err)
	}
	opts = append(opts, api.WithStorage(blobs))

	srv := &http.Server{
		Addr: cfg.HTTP.Addr,
		Handler: api.New(cfg, logger,
			opts...,
		).Handler(),
		ReadHeaderTimeout: cfg.HTTP.ReadHeaderTimeout,
		ReadTimeout:       cfg.HTTP.ReadTimeout,
		WriteTimeout:      cfg.HTTP.WriteTimeout,
		IdleTimeout:       cfg.HTTP.IdleTimeout,
		MaxHeaderBytes:    1 << 20,
		ErrorLog:          slog.NewLogLogger(logger.Handler(), slog.LevelWarn),
	}

	logger.Info("starting",
		"env", string(cfg.App.Env),
		"version", cfg.App.Version,
		"addr", cfg.HTTP.Addr,
		"dotenv_keys", loaded,
		"mysql", inf.db != nil,
		"redis", inf.rdb != nil,
	)

	if err := serve(ctx, srv, cfg.HTTP.ShutdownTimeout, logger); err != nil {
		return err
	}
	logger.Info("stopped")
	return nil
}

// infra 是进程持有的外部资源。字段不导出：除 setup / shutdown 外没有别处该碰它们。
type infra struct {
	db       *sql.DB
	rdb      *redis.Client
	checkers []api.Checker
}

// setupInfra 建立 MySQL / Redis 连接，并在 AUTO_MIGRATE 打开时执行迁移。
//
// dev 环境下连不上不致命：降级成"无存储模式"，/readyz 报对应依赖 down，
// 前端与 B 仍可继续联调。非 dev 环境必须失败——带着坏存储启动只会把故障
// 推迟到第一个真实请求，而那时的报错信息远不如启动日志清楚。
func setupInfra(ctx context.Context, cfg config.Config, log *slog.Logger) (*infra, error) {
	inf := &infra{}

	db, dbErr := store.OpenMySQL(ctx, cfg.MySQL)
	switch {
	case dbErr != nil && cfg.App.Env != config.EnvDev:
		return nil, dbErr
	case dbErr != nil:
		log.Warn("MySQL 不可用，dev 环境以无存储模式继续", "err", dbErr.Error())
	default:
		inf.db = db
	}

	if inf.db != nil {
		if cfg.App.AutoMigrate {
			applied, mErr := store.Migrate(ctx, inf.db)
			if mErr != nil {
				inf.shutdown(log)
				return nil, mErr
			}
			log.Info("schema 迁移完成", "applied", applied)
		}
		inf.checkers = append(inf.checkers, store.NewMySQLChecker(inf.db))
	}

	rdb, rErr := store.OpenRedis(ctx, cfg.Redis)
	switch {
	case rErr != nil && cfg.App.Env != config.EnvDev:
		inf.shutdown(log)
		return nil, rErr
	case rErr != nil:
		log.Warn("Redis 不可用，dev 环境继续（缓存与限流暂不可用）", "err", rErr.Error())
	default:
		inf.rdb = rdb
		inf.checkers = append(inf.checkers, store.NewRedisChecker(inf.rdb))
	}

	return inf, nil
}

// shutdown 逆序释放资源。
//
// 关闭失败只记日志、不返回错误：此处已在退出路径上，没有上层可以处理这个错误，
// 而连接池未关干净会随进程退出被操作系统回收。记日志是为了在"退出很慢"时
// 能看出是谁卡住了。
func (i *infra) shutdown(log *slog.Logger) {
	if i.rdb != nil {
		if err := i.rdb.Close(); err != nil {
			log.Warn("关闭 Redis 失败", "err", err.Error())
		}
	}
	if i.db != nil {
		if err := i.db.Close(); err != nil {
			log.Warn("关闭 MySQL 失败", "err", err.Error())
		}
	}
}

// serve 启动服务器并等待退出信号。
//
// 只有一个 goroutine 负责 ListenAndServe，它的退出结果写进容量为 1 的 errCh：
// 无论下面的 select 走哪条分支，这个 goroutine 都一定能写出结果并退出，
// 不会永久阻塞在 send 上——这是"裸起 goroutine"必须给出的回收证明。
//
// 关闭顺序：先 Shutdown（等在途请求结束），再读 errCh（拿 ListenAndServe 的返回）。
// 反过来的话，Shutdown 期间的新连接仍会被接受。
func serve(ctx context.Context, srv *http.Server, shutdownTimeout time.Duration, log *slog.Logger) error {
	errCh := make(chan error, 1)
	go func() {
		errCh <- srv.ListenAndServe()
	}()

	select {
	case err := <-errCh:
		if errors.Is(err, http.ErrServerClosed) {
			return nil
		}
		return fmt.Errorf("监听 %s: %w", srv.Addr, err)
	case <-ctx.Done():
		log.Info("shutdown signal received")
	}

	// 用 context.Background 而不是已取消的 ctx：后者会让 Shutdown 立刻放弃等待。
	shutdownCtx, cancel := context.WithTimeout(context.Background(), shutdownTimeout)
	defer cancel()

	if err := srv.Shutdown(shutdownCtx); err != nil {
		return fmt.Errorf("优雅关闭超时（%s）: %w", shutdownTimeout, err)
	}
	if err := <-errCh; err != nil && !errors.Is(err, http.ErrServerClosed) {
		return fmt.Errorf("关闭后读取监听结果: %w", err)
	}
	return nil
}

// newLogger 按配置构造 slog。
//
// 选 slog 而不是 zap：标准库、零依赖，JSON 输出与字段风格足够本项目的规模；
// 多引一个日志库只会多一份要跟着升级的依赖。
func newLogger(cfg config.LogConfig) *slog.Logger {
	var level slog.Level
	if err := level.UnmarshalText([]byte(cfg.Level)); err != nil {
		level = slog.LevelInfo
	}
	opts := &slog.HandlerOptions{Level: level}

	var handler slog.Handler
	if cfg.Format == "json" {
		handler = slog.NewJSONHandler(os.Stdout, opts)
	} else {
		handler = slog.NewTextHandler(os.Stdout, opts)
	}
	return slog.New(handler)
}
