package store

import (
	"context"
	"testing"

	"laborlaw/internal/id"
	"laborlaw/internal/port"
)

// TestReviewsCarryFilename 守住"审查记录带出原文件名"。
//
// 缺陷形态：列表与详情都只返回 file_id，前端要显示"审的是哪份合同"
// 只能逐条再查一次文件，一页 20 条就是 20 次往返；而且文件删掉之后
// 前端连兜底文案都拿不到依据。
func TestReviewsCarryFilename(t *testing.T) {
	db := testDB(t)
	ctx := context.Background()

	userID := id.New()
	cleanupUser(t, db, userID)

	files := NewFiles(db)
	rec := newFile(userID)
	rec.Filename = "劳动合同-2026.pdf"
	if err := files.Create(ctx, rec); err != nil {
		t.Fatalf("准备文件记录失败：%v", err)
	}

	s := NewReviews(db)
	rv := newReview(userID)
	rv.FileID = rec.ID
	cleanupReviews(t, db, rv.ID)
	if err := s.Create(ctx, rv); err != nil {
		t.Fatalf("Create: %v", err)
	}

	got, err := s.Get(ctx, userID, rv.ID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.Filename != "劳动合同-2026.pdf" {
		t.Fatalf("Filename = %q, want 劳动合同-2026.pdf", got.Filename)
	}

	page, err := s.ListByUser(ctx, userID, 10, 0)
	if err != nil {
		t.Fatalf("ListByUser: %v", err)
	}
	if len(page) == 0 || page[0].Filename != "劳动合同-2026.pdf" {
		t.Fatalf("列表里的 Filename 也应带出：%+v", page)
	}

	// 文件被删掉之后仍要读得出来，只是文件名为空串（LEFT JOIN 的意义）。
	if _, err := db.ExecContext(ctx, `DELETE FROM files WHERE id = ?`, rec.ID); err != nil {
		t.Fatalf("删除文件记录失败：%v", err)
	}
	got, err = s.Get(ctx, userID, rv.ID)
	if err != nil {
		t.Fatalf("文件删除后 Get 不应失败：%v", err)
	}
	if got.Filename != "" {
		t.Fatalf("文件已删除时 Filename 应为空串，实际 %q", got.Filename)
	}
}

// TestReviewsCarryTaskError 守住"历史行能显示失败原因"。
//
// 缺陷形态：审查失败之后，列表只给 status=failed，用户看到"失败 未评级"，
// 原因只写在 tasks.error 里，前端拿不到——等于什么都没有。
func TestReviewsCarryTaskError(t *testing.T) {
	db := testDB(t)
	ctx := context.Background()

	userID := id.New()
	cleanupUser(t, db, userID)

	tasks := NewTasks(db)
	task := &port.Task{
		ID:      id.New(),
		Type:    "review.run",
		Status:  port.TaskPending,
		Payload: []byte(`{"review_id":"x","file_id":"y","user_id":"` + userID + `"}`),
	}
	if err := tasks.Create(ctx, task); err != nil {
		t.Fatalf("准备任务失败：%v", err)
	}
	reason := "任务执行时出现未预期的错误，请重新发起；若持续失败请提供任务号"
	if err := tasks.Finish(ctx, task.ID, port.TaskFailed, reason); err != nil {
		t.Fatalf("写失败原因失败：%v", err)
	}
	t.Cleanup(func() {
		_, _ = db.ExecContext(ctx, `DELETE FROM tasks WHERE id = ?`, task.ID)
	})

	s := NewReviews(db)
	rv := newReview(userID)
	rv.TaskID = task.ID
	rv.Status = port.TaskFailed
	cleanupReviews(t, db, rv.ID)
	if err := s.Create(ctx, rv); err != nil {
		t.Fatalf("Create: %v", err)
	}

	got, err := s.Get(ctx, userID, rv.ID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.Error != reason {
		t.Fatalf("失败原因没带出来：%q", got.Error)
	}

	// 成功的审查没有 error，不能把别的行串过来。
	ok := newReview(userID)
	cleanupReviews(t, db, ok.ID)
	if err := s.Create(ctx, ok); err != nil {
		t.Fatalf("Create: %v", err)
	}
	gotOK, err := s.Get(ctx, userID, ok.ID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if gotOK.Error != "" {
		t.Fatalf("成功记录不该有失败原因：%q", gotOK.Error)
	}
}
