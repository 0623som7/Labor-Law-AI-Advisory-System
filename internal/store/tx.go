package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	"laborlaw/internal/port"
)

// querier 是 *sql.DB 与 *sql.Tx 的公共子集。
//
// 引入它是为了让同一个 DAL 既能挂在连接池上，也能挂在事务上：port.Store.Tx 的契约
// 要求回调里所有操作落在同一事务，而 Files 的实现只用到这三个方法。
type querier interface {
	ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error)
	QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error)
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
}

// beginTxer 只有 *sql.DB 与 *sql.Conn 实现，*sql.Tx 不实现。
type beginTxer interface {
	BeginTx(ctx context.Context, opts *sql.TxOptions) (*sql.Tx, error)
}

// withTx 在一个事务里执行 fn，fn 返回 error 时回滚。
//
// 为什么先做包内助手而不是直接实现 port.Store.Tx：Tx 的回调签名要求拿到
// 一个"所有操作都落在同一事务里"的 Store，而六个域目前只有 File 落地。
// DB.Tx 会调用这个助手，把 tx 包成 Store 再交给回调——行为一致，不需要重写事务逻辑。
//
// 三条禁令（与 port.Store.Tx 的注释一致）：回调里不许起 goroutine 用 tx、
// 不许嵌套事务、不许超过 ctx 的预算。
func withTx(ctx context.Context, q querier, fn func(tx *sql.Tx) error) error {
	// 已经在一个事务里：直接把同一个 tx 透传下去，提交/回滚由最外层负责。
	// 不这么做的话，SavePages 一旦在 Store.Tx 的回调里被调用就会撞上
	// "sql: Tx.BeginTx called on a transaction"，而那是个很难看出原因的 500。
	if tx, ok := q.(*sql.Tx); ok {
		return fn(tx)
	}

	begin, ok := q.(beginTxer)
	if !ok {
		return port.New(port.CodeInternal, "该连接不支持事务")
	}

	tx, err := begin.BeginTx(ctx, nil)
	if err != nil {
		return port.Wrap(port.CodeInternal, err, "开启事务失败")
	}

	if err := fn(tx); err != nil {
		// 回滚失败不覆盖主错误：主错误才是业务原因，回滚失败只是收尾问题。
		if rbErr := tx.Rollback(); rbErr != nil && !errors.Is(rbErr, sql.ErrTxDone) {
			return fmt.Errorf("%w（回滚也失败: %v）", err, rbErr)
		}
		return err
	}

	if err := tx.Commit(); err != nil {
		return port.Wrap(port.CodeInternal, err, "提交事务失败")
	}
	return nil
}

// notFound 把 sql.ErrNoRows 折成 40401，其余包成 50001。
//
// 统一在这里做：DAL 的每个 Get 都要判这个，散着写必然有一处漏判，
// 表现是"查不到文件"变成 500，前端弹"服务异常"。
func notFound(err error, format string, args ...any) error {
	if errors.Is(err, sql.ErrNoRows) {
		return port.New(port.CodeNotFound, format, args...)
	}
	return port.Wrap(port.CodeInternal, err, format, args...)
}
