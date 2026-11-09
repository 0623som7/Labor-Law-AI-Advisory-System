package store

import (
	"context"
	"database/sql"

	"laborlaw/internal/port"
)

// FailStaleReviews 把「所属任务已经失败」的审查记录收尾成 failed，返回受影响行数。
//
// 为什么需要它：createReview 发现已有 review 且其任务不是终态时会直接复用
// （回 202 + 老 task_id），这是为了"同一份文件并发点两次只看一次"。
// 但进程重启后老任务是 failed、review 还停在 pending/running，
// 前端就会拿着一个永远不会前进的 task_id 轮询到超时，再点还是同一条——
// 用户彻底卡死，只能重新上传一份合同。
//
// 为什么用 JOIN 而不是一把梭更新所有 pending/running：
// 只收拾「任务已经失败了」的那些。任务还活着的 review 是真在跑，
// 一根手指都不能碰。
func FailStaleReviews(ctx context.Context, db *sql.DB) (int64, error) {
	if db == nil {
		return 0, nil
	}
	// task_id 为空串的 review 不会被 JOIN 命中：这类行是历史数据或正在创建中的记录，
	// 没有任务可对账，保持原状比误判成失败安全。
	res, err := db.ExecContext(ctx, `UPDATE reviews rv
		JOIN tasks t ON t.id = rv.task_id
		SET rv.status = 'failed', rv.updated_at = CURRENT_TIMESTAMP(3)
		WHERE rv.status IN ('pending', 'running') AND t.status = 'failed'`)
	if err != nil {
		return 0, port.Wrap(port.CodeInternal, err, "清理中断审查失败")
	}
	affected, err := res.RowsAffected()
	if err != nil {
		return 0, port.Wrap(port.CodeInternal, err, "读取清理结果失败")
	}
	return affected, nil
}
