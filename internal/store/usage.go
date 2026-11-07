package store

import (
	"context"
	"database/sql"
	"time"

	"laborlaw/internal/port"
)

// Usage 是 usage_records 的 DAL。
//
// 为什么单独一个类型而不并进六个域：这张表只写不读，读路径是 SQL 报表，
// 业务代码永远不该在请求里查它。把它挂进 port.Store 会让每个 fake 都多实现
// 一个没人调的方法，等 B 签字后再动接口（见 port.UsageStore 的注释）。
type Usage struct {
	db querier
}

// NewUsage 构造计量 DAL。
func NewUsage(db *sql.DB) *Usage { return &Usage{db: db} }

// newUsageOn 在给定连接上构造，供聚合体与事务复用。
func newUsageOn(q querier) *Usage { return &Usage{db: q} }

// UsageStore 是计量写入契约。
//
// 故意不复用 llm.UsageRecorder：DAL 不该反向 import llm（那是上游 SDK 层），
// 而 port.Store 由 B 冻结、Usage 的字段集也还没签字，所以先在 store 内定一个
// 结构等价的小接口，等 B 确认后再统一收口到 port。
type UsageStore interface {
	Record(ctx context.Context, rec port.UsageRecord) error
}

var _ UsageStore = (*Usage)(nil)

// usageInsertSQL 只列需要写的列：id 自增，created_at 有默认值但由调用方给，
// 这样重试或补录时写入的是"实际调用发生的时刻"，而不是落库那一刻。
const usageInsertSQL = `INSERT INTO usage_records
	(trace_id, scene, model, prompt_tokens, completion_tokens, latency_ms, cost_est, created_at)
VALUES (?, ?, ?, ?, ?, ?, ?, ?)`

// Record 写入一条调用计量。
//
// scene 为空直接拒：它是成本报表的分组键，空值会让"咨询花了多少、抽取花了多少"
// 这类问题永远答不出来，而错过的数据补不回来。
func (u *Usage) Record(ctx context.Context, rec port.UsageRecord) error {
	if u == nil || u.db == nil {
		return port.New(port.CodeInternal, "计量存储未配置")
	}
	if rec.Scene == "" {
		return port.New(port.CodeInvalidArgument, "计量记录缺少 scene")
	}
	created := rec.CreatedAt
	if created.IsZero() {
		created = time.Now().UTC()
	}
	if _, err := u.db.ExecContext(ctx, usageInsertSQL,
		rec.TraceID,
		rec.Scene,
		rec.Model,
		rec.PromptTokens,
		rec.CompletionTokens,
		rec.LatencyMS,
		rec.CostEst,
		created,
	); err != nil {
		return port.Wrap(port.CodeInternal, err, "写入模型计量失败")
	}
	return nil
}
