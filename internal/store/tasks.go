package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"strings"

	"laborlaw/internal/port"
)

// taskColumns 是 tasks 表的读取列序，与 scanTask 一一对应。
const taskColumns = `id, type, status, payload, progress, stage, error, attempt, created_at, updated_at`

// Tasks 是 port.TaskStore 的 MySQL 实现。
type Tasks struct{ db querier }

// NewTasks 构造任务域 DAL。
func NewTasks(db *sql.DB) *Tasks { return &Tasks{db: db} }

// newTasksOn 构造绑定在任意连接（连接池或事务）上的任务域 DAL。
func newTasksOn(q querier) *Tasks { return &Tasks{db: q} }

var _ port.TaskStore = (*Tasks)(nil)

// Create 插入任务，初始状态必须是 pending。
//
// 拒绝非 pending 的初值：一个"出生即成功"的任务会让 worker 的失败重试、
// 启动对账都失去意义——那些逻辑全靠状态机从 pending 开始推。
func (t *Tasks) Create(ctx context.Context, task *port.Task) error {
	if task == nil || task.ID == "" || task.Type == "" {
		return port.New(port.CodeInvalidArgument, "任务缺少 id 或 type")
	}
	if task.Status == "" {
		task.Status = port.TaskPending
	}
	if task.Status != port.TaskPending {
		return port.New(port.CodeConflict, "任务初始状态必须是 %s，收到 %s", port.TaskPending, task.Status)
	}
	payload := task.Payload
	if len(payload) == 0 {
		payload = []byte("{}")
	}
	if !json.Valid(payload) {
		return port.New(port.CodeInvalidArgument, "任务载荷不是合法的 JSON")
	}

	_, err := t.db.ExecContext(ctx, `INSERT INTO tasks (id, type, status, payload, progress, stage, error, attempt)
		VALUES (?, ?, ?, ?, ?, ?, '', ?)`,
		task.ID, task.Type, string(task.Status), string(payload), task.Progress, task.Stage, task.Attempt)
	if err != nil {
		return port.Wrap(port.CodeInternal, err, "写入任务失败")
	}
	return nil
}

// Get 按 ID 查任务。
func (t *Tasks) Get(ctx context.Context, taskID string) (*port.Task, error) {
	if strings.TrimSpace(taskID) == "" {
		return nil, port.New(port.CodeInvalidArgument, "任务 ID 为空")
	}
	row := t.db.QueryRowContext(ctx, `SELECT `+taskColumns+` FROM tasks WHERE id = ?`, taskID)
	return scanTaskRow(row)
}

// UpdateProgress 覆盖写进度与阶段文案。
//
// 顺带把 pending 推成 running：port.TaskStore 没有单独的"开始执行"方法，
// 而 Finish 的状态机要求 running 才能转 succeeded——如果进度不回写状态，
// 任务会永远停在 pending，最后 Finish 被判非法转移（409），
// 前端看到的是"任务一直排队"而不是"已完成但没落库"。
//
// percent 在这里再夹一次：调用方（worker 的节流器）已经夹过，
// 但 DAL 是唯一守卫，任何新调用方都不该能写进 120。
func (t *Tasks) UpdateProgress(ctx context.Context, taskID string, percent int, stage string) error {
	if strings.TrimSpace(taskID) == "" {
		return port.New(port.CodeInvalidArgument, "任务 ID 为空")
	}
	if percent < 0 {
		percent = 0
	}
	if percent > 100 {
		percent = 100
	}

	res, err := t.db.ExecContext(ctx, `UPDATE tasks SET
			progress = ?, stage = ?,
			status = IF(status = 'pending', 'running', status),
			updated_at = CURRENT_TIMESTAMP(3)
		WHERE id = ?`, percent, stage, taskID)
	if err != nil {
		return port.Wrap(port.CodeInternal, err, "更新任务进度失败")
	}
	affected, err := res.RowsAffected()
	if err != nil {
		return port.Wrap(port.CodeInternal, err, "读取更新结果失败")
	}
	// 命中 0 行可能是"任务不存在"，也可能是"进度与状态都没变"（MySQL 默认不计数）。
	// 用一次存在性查询区分，否则重复的同一进度会被误报成 404。
	if affected == 0 {
		var exists int
		if err := t.db.QueryRowContext(ctx, `SELECT 1 FROM tasks WHERE id = ?`, taskID).Scan(&exists); err != nil {
			return notFound(err, "任务不存在")
		}
	}
	return nil
}

// Finish 把任务置为终态。
//
// 先读后写而不是一条条件 UPDATE：状态机规则（TaskStatus.CanTransition）
// 是 port 层的契约，DAL 不该复制一份；先读出来判完再写，非法转移的报错
// 才能带上"当前状态 → 目标状态"，运维看得懂。
func (t *Tasks) Finish(ctx context.Context, taskID string, status port.TaskStatus, message string) error {
	if strings.TrimSpace(taskID) == "" {
		return port.New(port.CodeInvalidArgument, "任务 ID 为空")
	}
	if !status.Terminal() {
		return port.New(port.CodeInvalidArgument, "目标状态 %s 不是终态", status)
	}

	cur, err := t.Get(ctx, taskID)
	if err != nil {
		return err
	}
	if !cur.Status.CanTransition(status) {
		return port.New(port.CodeConflict, "任务状态不能从 %s 转为 %s", cur.Status, status)
	}

	// error 列是 VARCHAR(255)：超长直接截断而不是让 UPDATE 失败。
	// 它是"可展示文案"，截断只会丢细节，不会丢原因。
	message = strings.TrimSpace(message)
	if len([]rune(message)) > 255 {
		message = string([]rune(message)[:255])
	}

	if _, err := t.db.ExecContext(ctx, `UPDATE tasks SET
			status = ?, error = ?, updated_at = CURRENT_TIMESTAMP(3)
		WHERE id = ?`, string(status), message, taskID); err != nil {
		return port.Wrap(port.CodeInternal, err, "更新任务状态失败")
	}
	return nil
}

// ListByStatus 按状态取任务，updated_at 倒序。
func (t *Tasks) ListByStatus(ctx context.Context, status port.TaskStatus, limit int) ([]port.Task, error) {
	if limit <= 0 {
		limit = 50
	}
	rows, err := t.db.QueryContext(ctx,
		`SELECT `+taskColumns+` FROM tasks WHERE status = ?
		 ORDER BY updated_at DESC, id DESC LIMIT ?`, string(status), limit)
	if err != nil {
		return nil, port.Wrap(port.CodeInternal, err, "查询任务失败")
	}
	defer rows.Close()

	out := make([]port.Task, 0, limit)
	for rows.Next() {
		task, err := scanTaskRows(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *task)
	}
	if err := rows.Err(); err != nil {
		return nil, port.Wrap(port.CodeInternal, err, "遍历任务失败")
	}
	return out, nil
}

// FailRunning 把库里所有 pending 与 running 任务标为 failed，返回受影响行数。
//
// worker 进程崩溃后这两个状态都是脏状态：running 的没人会再推进，
// pending 的还排在队列里没人领——重启后队列是空的，它们会永远停在那里。
// 只在启动时调用一次，reason 写进 error 列供用户看到"服务重启，请重新提交"。
//
// 前提是单实例部署：多副本共享一个库时，启动时这样清理会误杀另一个副本
// 正在跑的任务。真要横向扩容，得先给 tasks 加一个 owner 列再按副本过滤。
func (t *Tasks) FailRunning(ctx context.Context, reason string) (int64, error) {
	reason = strings.TrimSpace(reason)
	if reason == "" {
		reason = "服务重启，任务中断"
	}
	if len([]rune(reason)) > 255 {
		reason = string([]rune(reason)[:255])
	}
	res, err := t.db.ExecContext(ctx,
		`UPDATE tasks SET status = 'failed', error = ?, updated_at = CURRENT_TIMESTAMP(3)
		 WHERE status IN ('pending', 'running')`, reason)
	if err != nil {
		return 0, port.Wrap(port.CodeInternal, err, "清理中断任务失败")
	}
	affected, err := res.RowsAffected()
	if err != nil {
		return 0, port.Wrap(port.CodeInternal, err, "读取清理结果失败")
	}
	return affected, nil
}

// scanTaskRow 扫一行任务（QueryRow 版本）。
func scanTaskRow(row *sql.Row) (*port.Task, error) {
	return scanTaskFrom(row, "任务不存在")
}

// scanTaskRows 扫一行任务（Query 版本）。
func scanTaskRows(rows *sql.Rows) (*port.Task, error) {
	return scanTaskFrom(rows, "读取任务失败")
}

// scanTaskFrom 把一行扫进 port.Task。
func scanTaskFrom(s scanner, missing string) (*port.Task, error) {
	var (
		task    port.Task
		status  string
		payload []byte
	)
	if err := s.Scan(&task.ID, &task.Type, &status, &payload, &task.Progress,
		&task.Stage, &task.Error, &task.Attempt, &task.CreatedAt, &task.UpdatedAt); err != nil {
		return nil, notFound(err, "%s", missing)
	}
	task.Status = port.TaskStatus(status)
	task.Payload = payload
	return &task, nil
}
