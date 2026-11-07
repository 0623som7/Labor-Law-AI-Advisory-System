package store

import (
	"context"
	"database/sql"
	"testing"
	"time"

	"laborlaw/internal/id"
	"laborlaw/internal/port"
)

// 计量表同样走 TEST_MYSQL_DSN 开关（见 files_test.go 的说明）。
// 这里只需要验证"写进去的行与传进去的记录一致"以及两条拒绝分支：
// 读路径是报表 SQL，不在 DAL 里。

func cleanupUsage(t *testing.T, db *sql.DB, traceID string) {
	t.Helper()
	t.Cleanup(func() {
		if _, err := db.ExecContext(context.Background(), `DELETE FROM usage_records WHERE trace_id = ?`, traceID); err != nil {
			t.Logf("清理失败: %v", err)
		}
	})
}

func TestUsageRecord(t *testing.T) {
	db := testDB(t)
	traceID := id.New()
	cleanupUsage(t, db, traceID)

	d := NewUsage(db)
	rec := port.UsageRecord{
		TraceID:          traceID,
		Scene:            "consult",
		Model:            "qwen3.8-max",
		PromptTokens:     1200,
		CompletionTokens: 345,
		LatencyMS:        2345,
		CostEst:          0.012345,
	}
	if err := d.Record(context.Background(), rec); err != nil {
		t.Fatalf("写入计量失败: %v", err)
	}

	var (
		scene, model             string
		promptTokens, completion int
		latencyMS                int
		cost                     float64
		createdAt                time.Time
	)
	err := db.QueryRowContext(context.Background(), `SELECT scene, model, prompt_tokens, completion_tokens, latency_ms, cost_est, created_at
		FROM usage_records WHERE trace_id = ?`, traceID).
		Scan(&scene, &model, &promptTokens, &completion, &latencyMS, &cost, &createdAt)
	if err != nil {
		t.Fatalf("回读计量失败: %v", err)
	}

	if scene != rec.Scene || model != rec.Model {
		t.Errorf("scene/model 回读不一致: %q/%q", scene, model)
	}
	if promptTokens != rec.PromptTokens || completion != rec.CompletionTokens || latencyMS != rec.LatencyMS {
		t.Errorf("tokens/latency 回读不一致: %d/%d/%d", promptTokens, completion, latencyMS)
	}
	if diff := cost - rec.CostEst; diff > 1e-6 || diff < -1e-6 {
		t.Errorf("cost_est 回读不一致: %v != %v", cost, rec.CostEst)
	}
	// created_at 零值时由 DAL 补当前时刻：报表按天聚合，缺这一列的行等于不存在。
	if createdAt.IsZero() || time.Since(createdAt) > time.Minute {
		t.Errorf("created_at 未按当前时刻补全: %v", createdAt)
	}
}

func TestUsageRecordKeepsGivenTime(t *testing.T) {
	db := testDB(t)
	traceID := id.New()
	cleanupUsage(t, db, traceID)

	// 上游调用发生在上传/重试之前，补录时写的是那一刻，而不是落库那一刻。
	when := time.Date(2026, 3, 1, 9, 30, 0, 0, time.UTC)
	if err := NewUsage(db).Record(context.Background(), port.UsageRecord{
		TraceID:   traceID,
		Scene:     "extract",
		Model:     "qwen3.8-max",
		CreatedAt: when,
	}); err != nil {
		t.Fatalf("写入计量失败: %v", err)
	}

	var got time.Time
	if err := db.QueryRowContext(context.Background(),
		`SELECT created_at FROM usage_records WHERE trace_id = ?`, traceID).Scan(&got); err != nil {
		t.Fatalf("回读计量失败: %v", err)
	}
	if !got.Equal(when) {
		t.Errorf("created_at 应保留调用方给的时间: %v != %v", got, when)
	}
}

func TestUsageRecordRejectsEmptyScene(t *testing.T) {
	db := testDB(t)
	err := NewUsage(db).Record(context.Background(), port.UsageRecord{TraceID: id.New()})
	if !port.IsCode(err, port.CodeInvalidArgument) {
		t.Fatalf("scene 为空应返回 40001，实际: %v", err)
	}
}
