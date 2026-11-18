package store

import (
	"context"
	"testing"

	"laborlaw/internal/id"
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
