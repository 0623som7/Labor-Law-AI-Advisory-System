package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"

	// 驱动以副作用方式注册，导入名必须为空。
	_ "github.com/go-sql-driver/mysql"

	"laborlaw/internal/config"
)

// openTimeout 是建连探测的预算。
//
// 启动期探测必须短：MySQL 挂在启动路径上会让进程既不 ready 也不 crash，
// 表现为"部署完成但服务没上线"，比直接失败难查得多。
const openTimeout = 5 * time.Second

// checkTimeout 是单次就绪探针的预算。
const checkTimeout = 2 * time.Second

// OpenMySQL 建立连接池并探测一次。
//
// 只 Open 不 Ping 是常见错误：sql.Open 不建连，第一次真正查询才暴露密码错、
// 网络不通、库不存在等问题，而那些错误会以"某个接口 500"的形式出现在联调末期。
func OpenMySQL(ctx context.Context, cfg config.MySQLConfig) (*sql.DB, error) {
	db, err := sql.Open("mysql", cfg.DSN)
	if err != nil {
		return nil, fmt.Errorf("解析 MySQL DSN: %w", redact(cfg.DSN, err))
	}

	db.SetMaxOpenConns(cfg.MaxOpenConns)
	db.SetMaxIdleConns(cfg.MaxIdleConns)
	db.SetConnMaxLifetime(cfg.ConnMaxLifetime)

	pingCtx, cancel := context.WithTimeout(ctx, openTimeout)
	defer cancel()
	if err := db.PingContext(pingCtx); err != nil {
		wrapped := fmt.Errorf("连接 MySQL 失败: %w", redact(cfg.DSN, err))
		if closeErr := db.Close(); closeErr != nil {
			return nil, fmt.Errorf("%w（关闭连接池时又出错: %v）", wrapped, closeErr)
		}
		return nil, wrapped
	}
	return db, nil
}

// MySQLChecker 是 /readyz 的 MySQL 探针。
//
// 独立成类型而不是匿名函数：main 需要把它塞进 []api.Checker，
// 而 store 不 import api——方法集满足接口即可，不需要编译期耦合。
type MySQLChecker struct {
	db *sql.DB
}

// NewMySQLChecker 构造探针。db 为 nil 时 Check 会返回错误而不是 panic。
func NewMySQLChecker(db *sql.DB) MySQLChecker {
	return MySQLChecker{db: db}
}

// Name 返回探针名，出现在 /readyz 的 checks 里。
func (c MySQLChecker) Name() string { return "mysql" }

// Check 用一次 Ping 判断连接是否可用。
func (c MySQLChecker) Check(ctx context.Context) error {
	if c.db == nil {
		return errors.New("mysql: 未初始化")
	}
	ctx, cancel := context.WithTimeout(ctx, checkTimeout)
	defer cancel()
	if err := c.db.PingContext(ctx); err != nil {
		return fmt.Errorf("mysql ping: %w", err)
	}
	return nil
}

// redact 把错误文本里出现的 DSN 原文替换成 ***。
//
// 驱动在 DSN 解析失败时会把整串（含密码）写进错误，直接透传就是一次凭据泄漏，
// 而且它通常发生在启动日志里、最容易被贴进群里求助。
func redact(dsn string, err error) error {
	if err == nil || dsn == "" {
		return err
	}
	return errors.New(strings.ReplaceAll(err.Error(), dsn, "***"))
}
