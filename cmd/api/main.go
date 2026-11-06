// Command api 是 HTTP 进程入口：装配配置、日志、存储与路由，然后进入优雅关闭循环。
//
// 这里几乎没有业务代码是有意的：进程层只负责"把依赖装起来、把信号接住"，
// 每一个 handler 都能在 httptest 里单独跑起来，不依赖这个文件。
package main

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/redis/go-redis/v9"

	"laborlaw/internal/api"
	"laborlaw/internal/config"
	"laborlaw/internal/store"
)

func main() {
	if err := run(); err != nil {
		// 启动期失败时 logger 本身可能还没建起来（日志配置就在配置里），
		// 直接写 stderr 是唯一保证能看到错误的通道。
		fmt.Fprintf(os.Stderr, "api 启动失败: %v\n", err)
		os.Exit(1)
	}
}

// run 是 main 的唯一逻辑体，返回 error 而不是在内部 os.Exit，
// 这样 defer 能正常执行，也便于将来加集成测试。
func run() error {
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

	inf, err := setupInfra(ctx, cfg, logger)
	if err != nil {
		return err
	}
	defer inf.shutdown(logger)

	srv := &http.Server{
		Addr:              cfg.HTTP.Addr,
		Handler:           api.New(cfg, logger, inf.checkers...).Handler(),
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
