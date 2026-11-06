package store

import (
	"context"
	"database/sql"

	"laborlaw/internal/port"
)

// DB 是 port.Store 的 MySQL 实现。
//
// 两个连接字段互斥：tx 非空表示这是一个"事务内的 Store"，所有操作必须落在同一连接；
// 否则走 db 连接池。不要把它当成可以随便复制的结构体——Tx 内拿到的实例绑定在
// 单条连接上，跨 goroutine 传递是未定义行为（与 port.Store 的注释一致）。
type DB struct {
	db *sql.DB
	tx *sql.Tx
}

// 编译期断言：六域 + Tx 必须与 B 依赖的契约一致。
var _ port.Store = (*DB)(nil)

// New 构造聚合体。db 为 nil 时各域返回"始终报错"的实现而不是 nil——
// nil 接口在 B 手里会变成 panic，而 panic 在 recovery 之后只剩一条 500 日志，
// 查起来比一条"尚未配置 MySQL"的业务错误难十倍。
func New(db *sql.DB) *DB {
	return &DB{db: db}
}

// q 返回当前应当使用的连接：事务内是 tx，否则是连接池。
func (s *DB) q() querier {
	if s.tx != nil {
		return s.tx
	}
	if s.db == nil {
		return nil
	}
	return s.db
}

// File 返回文件与解析结果域。
func (s *DB) File() port.FileStore {
	q := s.q()
	if q == nil {
		return unimplementedFile{}
	}
	return newFilesOn(q)
}

// Tx 在一个数据库事务里执行 fn，fn 拿到的 Store 的所有操作落在同一事务。
//
// 嵌套调用直接报 50001 而不是静默复用外层事务：静默复用会让内层"回滚"变成
// 什么都不做，调用方以为失败被撤销了，实际数据还在。
func (s *DB) Tx(ctx context.Context, fn func(port.Store) error) error {
	if s.tx != nil {
		return port.New(port.CodeInternal, "不支持嵌套事务")
	}
	if s.db == nil {
		return port.New(port.CodeInternal, "MySQL 未配置，事务不可用")
	}
	return withTx(ctx, s.db, func(tx *sql.Tx) error {
		return fn(&DB{db: s.db, tx: tx})
	})
}

// Session 返回会话域。
func (s *DB) Session() port.SessionStore { return unimplementedSession{} }

// Message 返回消息域。
func (s *DB) Message() port.MessageStore { return unimplementedMessage{} }

// Review 返回审查域。
func (s *DB) Review() port.ReviewStore { return unimplementedReview{} }

// Statute 返回法条/判例/向量域。
func (s *DB) Statute() port.StatuteStore { return unimplementedStatute{} }

// Task 返回任务域。
func (s *DB) Task() port.TaskStore { return unimplementedTask{} }
