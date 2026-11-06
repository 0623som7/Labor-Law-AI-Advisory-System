package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	"laborlaw/internal/port"
)

// withTx 在一个事务里执行 fn，fn 返回 error 时回滚。
//
// 为什么先做包内助手而不是直接实现 port.Store.Tx：Tx 的回调签名要求拿到
// 一个"所有操作都落在同一事务里"的 Store，而现在六个域只有 File 落地。
// 等六个域齐了，Store.Tx 会调用这个助手，把 tx 包成 Store 再交给回调——
// 行为一致，不需要重写事务逻辑。
//
// 三条禁令（与 port.Store.Tx 的注释一致）：回调里不许起 goroutine 用 tx、
// 不许嵌套事务、不许超过 ctx 的预算。
func withTx(ctx context.Context, db *sql.DB, fn func(tx *sql.Tx) error) error {
	tx, err := db.BeginTx(ctx, nil)
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
