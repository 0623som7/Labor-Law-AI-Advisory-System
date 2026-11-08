package store

import (
	"context"
	"database/sql"
	"strings"
	"testing"
	"time"

	"laborlaw/internal/id"
	"laborlaw/internal/port"
)

// 审查与风险的 DAL 同样走 TEST_MYSQL_DSN 开关（见 files_test.go 的说明）。
// 这里刻意覆盖三类容易写错的语义：先删后插的幂等、空切片不是 nil、
// 以及"更新一条不存在的记录必须 40401 而不是静默成功"。

func cleanupReviews(t *testing.T, db *sql.DB, reviewIDs ...string) {
	t.Helper()
	t.Cleanup(func() {
		ctx := context.Background()
		for _, rid := range reviewIDs {
			for _, q := range []string{`DELETE FROM findings WHERE review_id = ?`, `DELETE FROM reviews WHERE id = ?`} {
				if _, err := db.ExecContext(ctx, q, rid); err != nil {
					t.Logf("清理失败: %v", err)
				}
			}
		}
	})
}

func newReview(userID string) *port.Review {
	return &port.Review{
		ID:             id.New(),
		FileID:         id.New(),
		UserID:         userID,
		Status:         port.TaskPending,
		RiskLevel:      port.SeverityHigh,
		Score:          0,
		ClausePassRate: 0.875,
		Counts:         port.ReviewCounts{High: 5, Medium: 3, Low: 0},
		TaskID:         id.New(),
	}
}

func TestReviewsCreateGet(t *testing.T) {
	db := testDB(t)
	rv := newReview(id.New())
	cleanupReviews(t, db, rv.ID)

	s := NewReviews(db)
	ctx := context.Background()

	if err := s.Create(ctx, rv); err != nil {
		t.Fatalf("Create: %v", err)
	}
	got, err := s.Get(ctx, rv.UserID, rv.ID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.FileID != rv.FileID || got.UserID != rv.UserID || got.TaskID != rv.TaskID {
		t.Fatalf("字段错位: %+v", got)
	}
	if got.Status != port.TaskPending || got.RiskLevel != port.SeverityHigh {
		t.Fatalf("status/risk = %s/%s", got.Status, got.RiskLevel)
	}
	if got.Counts != (port.ReviewCounts{High: 5, Medium: 3}) {
		t.Fatalf("counts = %+v", got.Counts)
	}
	// DECIMAL(5,4) 回来是字符串，扫成 float64 必须精确到 4 位。
	if got.ClausePassRate != 0.875 {
		t.Fatalf("clause_pass_rate = %v，期望 0.875", got.ClausePassRate)
	}
	if got.CreatedAt.IsZero() || got.UpdatedAt.IsZero() {
		t.Fatalf("时间戳没回填: %+v", got)
	}
}

func TestReviewsCreateDuplicateIs409(t *testing.T) {
	db := testDB(t)
	rv := newReview(id.New())
	cleanupReviews(t, db, rv.ID)
	s := NewReviews(db)

	if err := s.Create(context.Background(), rv); err != nil {
		t.Fatalf("首次 Create: %v", err)
	}
	err := s.Create(context.Background(), rv)
	if !port.IsCode(err, port.CodeConflict) {
		t.Fatalf("重复主键应返回 40901，实际: %v", err)
	}
}

func TestReviewsGetOtherUserIs404(t *testing.T) {
	db := testDB(t)
	rv := newReview(id.New())
	cleanupReviews(t, db, rv.ID)
	s := NewReviews(db)
	ctx := context.Background()
	if err := s.Create(ctx, rv); err != nil {
		t.Fatalf("Create: %v", err)
	}

	_, err := s.Get(ctx, id.New(), rv.ID)
	if !port.IsCode(err, port.CodeNotFound) {
		t.Fatalf("别人的审查应当 40401，实际: %v", err)
	}
}

func TestReviewsGetByFileNewest(t *testing.T) {
	db := testDB(t)
	userID, fileID := id.New(), id.New()
	first := newReview(userID)
	first.FileID = fileID
	second := newReview(userID)
	second.FileID = fileID
	cleanupReviews(t, db, first.ID, second.ID)

	s := NewReviews(db)
	ctx := context.Background()
	if err := s.Create(ctx, first); err != nil {
		t.Fatalf("Create first: %v", err)
	}
	// 0003 之后同一份合同同时只能有一条 pending/running，所以先把第一条收尾，
	// 再建"更近的一次审查"——这正是真实的重复审查路径（规则更新后重跑）。
	if err := s.Update(ctx, &port.Review{ID: first.ID, Status: port.TaskFailed, UpdatedAt: time.Now().UTC()}); err != nil {
		t.Fatalf("收尾第一条: %v", err)
	}
	// created_at 精度是毫秒，连插两条可能撞同一毫秒；显式错开，避免用 id 兜底排序掩盖真实顺序。
	time.Sleep(5 * time.Millisecond)
	if err := s.Create(ctx, second); err != nil {
		t.Fatalf("Create second: %v", err)
	}

	got, err := s.GetByFile(ctx, userID, fileID)
	if err != nil {
		t.Fatalf("GetByFile: %v", err)
	}
	if got.ID != second.ID {
		t.Fatalf("GetByFile 返回 %s，期望最新的 %s", got.ID, second.ID)
	}

	if _, err := s.GetByFile(ctx, userID, id.New()); !port.IsCode(err, port.CodeNotFound) {
		t.Fatalf("没有审查的文件应当 40401，实际: %v", err)
	}
}

func TestReviewsUpdate(t *testing.T) {
	db := testDB(t)
	rv := newReview(id.New())
	cleanupReviews(t, db, rv.ID)
	s := NewReviews(db)
	ctx := context.Background()
	if err := s.Create(ctx, rv); err != nil {
		t.Fatalf("Create: %v", err)
	}

	rv.Status = port.TaskSucceeded
	rv.RiskLevel = port.SeverityMedium
	rv.Score = 65
	rv.ClausePassRate = 0.5
	rv.Counts = port.ReviewCounts{High: 1, Medium: 2, Low: 3}
	if err := s.Update(ctx, rv); err != nil {
		t.Fatalf("Update: %v", err)
	}

	got, err := s.Get(ctx, rv.UserID, rv.ID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.Status != port.TaskSucceeded || got.Score != 65 || got.RiskLevel != port.SeverityMedium {
		t.Fatalf("更新未生效: %+v", got)
	}
	if got.Counts != (port.ReviewCounts{High: 1, Medium: 2, Low: 3}) || got.ClausePassRate != 0.5 {
		t.Fatalf("counts/pass_rate 未生效: %+v", got)
	}

	missing := *rv
	missing.ID = id.New()
	if err := s.Update(ctx, &missing); !port.IsCode(err, port.CodeNotFound) {
		t.Fatalf("更新不存在的审查应当 40401，实际: %v", err)
	}
}

func sampleFindings(reviewID string) []port.Finding {
	impact := 12000.0
	return []port.Finding{
		{
			ID:        id.New(),
			ReviewID:  reviewID,
			RuleID:    "R-DEPOSIT",
			Severity:  port.SeverityHigh,
			Title:     "违法收取押金",
			ClauseRef: port.ClauseRef{Section: "第八条", Page: 0, BBox: [4]float64{72, 640, 520, 668}},
			Evidence:  "乙方向甲方缴纳押金 2000 元",
			LegalBasis: []port.LegalBasis{
				{Law: "中华人民共和国劳动合同法", Article: "第九条", Text: "用人单位招用劳动者，不得扣押劳动者的居民身份证和其他证件，不得要求劳动者提供担保或者以其他名义向劳动者收取财物。"},
			},
			Suggestion: port.Suggestion{Type: "delete", Original: "乙方向甲方缴纳押金 2000 元", Revised: "", Reason: "违法条款，应删除"},
		},
		{
			ID:             id.New(),
			ReviewID:       reviewID,
			RuleID:         "R-WAGE-UNDER-MIN",
			Severity:       port.SeverityMedium,
			Title:          "工资低于最低标准",
			ClauseRef:      port.ClauseRef{Section: "第十二条", Page: 1, BBox: [4]float64{72, 300, 520, 328}},
			Evidence:       "月工资 1500 元",
			LegalBasis:     []port.LegalBasis{},
			Suggestion:     port.Suggestion{Type: "rewrite", Original: "月工资 1500 元", Revised: "月工资 1900 元", Reason: "补齐至最低工资"},
			EconomicImpact: &impact,
		},
	}
}

func TestReviewsSaveFindingsIdempotent(t *testing.T) {
	db := testDB(t)
	rv := newReview(id.New())
	cleanupReviews(t, db, rv.ID)
	s := NewReviews(db)
	ctx := context.Background()
	if err := s.Create(ctx, rv); err != nil {
		t.Fatalf("Create: %v", err)
	}

	items := sampleFindings(rv.ID)
	if err := s.SaveFindings(ctx, rv.ID, items); err != nil {
		t.Fatalf("SaveFindings: %v", err)
	}
	// 重跑一次（worker 重试会走这里）：必须是"替换"而不是"追加"。
	if err := s.SaveFindings(ctx, rv.ID, items); err != nil {
		t.Fatalf("重复 SaveFindings: %v", err)
	}

	var count int
	if err := db.QueryRowContext(ctx, `SELECT COUNT(*) FROM findings WHERE review_id = ?`, rv.ID).Scan(&count); err != nil {
		t.Fatalf("计数失败: %v", err)
	}
	if count != len(items) {
		t.Fatalf("findings 行数 = %d，期望 %d（先删后插失效）", count, len(items))
	}

	got, err := s.ListFindings(ctx, rv.ID)
	if err != nil {
		t.Fatalf("ListFindings: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("ListFindings 返回 %d 条", len(got))
	}
	// 排序：high 在前。
	if got[0].Severity != port.SeverityHigh || got[1].Severity != port.SeverityMedium {
		t.Fatalf("排序错误: %s, %s", got[0].Severity, got[1].Severity)
	}
	high := got[0]
	if high.ClauseRef.Section != "第八条" || high.ClauseRef.BBox != ([4]float64{72, 640, 520, 668}) {
		t.Fatalf("clause_ref 没还原: %+v", high.ClauseRef)
	}
	if len(high.LegalBasis) != 1 || high.LegalBasis[0].Article != "第九条" {
		t.Fatalf("legal_basis 没还原: %+v", high.LegalBasis)
	}
	if high.Suggestion.Type != "delete" {
		t.Fatalf("suggestion 没还原: %+v", high.Suggestion)
	}
	if got[1].EconomicImpact == nil || *got[1].EconomicImpact != 12000 {
		t.Fatalf("economic_impact 没还原: %+v", got[1].EconomicImpact)
	}
	if got[1].LegalBasis == nil {
		t.Fatal("空 legal_basis 应当是 []，不是 nil")
	}
}

func TestReviewsListFindingsEmptyIsArray(t *testing.T) {
	db := testDB(t)
	rv := newReview(id.New())
	cleanupReviews(t, db, rv.ID)
	s := NewReviews(db)
	ctx := context.Background()
	if err := s.Create(ctx, rv); err != nil {
		t.Fatalf("Create: %v", err)
	}

	got, err := s.ListFindings(ctx, rv.ID)
	if err != nil {
		t.Fatalf("ListFindings: %v", err)
	}
	if got == nil || len(got) != 0 {
		t.Fatalf("空结果应当是空切片，实际: %#v", got)
	}
}

func TestReviewsListByUserPaging(t *testing.T) {
	db := testDB(t)
	userID := id.New()
	a, b, c := newReview(userID), newReview(userID), newReview(userID)
	cleanupReviews(t, db, a.ID, b.ID, c.ID)
	s := NewReviews(db)
	ctx := context.Background()
	for _, rv := range []*port.Review{a, b, c} {
		if err := s.Create(ctx, rv); err != nil {
			t.Fatalf("Create: %v", err)
		}
		time.Sleep(5 * time.Millisecond)
	}

	page, err := s.ListByUser(ctx, userID, 2, 0)
	if err != nil {
		t.Fatalf("ListByUser: %v", err)
	}
	if len(page) != 2 || page[0].ID != c.ID || page[1].ID != b.ID {
		t.Fatalf("第一页顺序错误: %+v", page)
	}

	page, err = s.ListByUser(ctx, userID, 2, 2)
	if err != nil {
		t.Fatalf("ListByUser offset: %v", err)
	}
	if len(page) != 1 || page[0].ID != a.ID {
		t.Fatalf("第二页错误: %+v", page)
	}

	empty, err := s.ListByUser(ctx, id.New(), 10, 0)
	if err != nil {
		t.Fatalf("ListByUser 空: %v", err)
	}
	if empty == nil {
		t.Fatal("空结果应当是空切片，不是 nil")
	}
}

// TestReviewsCreateRejectsConcurrentActive 守住 0003 迁移的部分唯一约束：
// 同一 (user, file) 最多一条 pending/running，落终态后释放。
//
// 这条约束是并发审查的唯一拦截点——api 层的 GetByFile 查重与 Create 不在同一
// 事务里（中间还夹着建 task、入池），双击按钮时两个请求都能查到"没有记录"。
func TestReviewsCreateRejectsConcurrentActive(t *testing.T) {
	db := testDB(t)
	s := NewReviews(db)
	ctx := context.Background()

	userID := id.New()
	first := newReview(userID)
	second := newReview(userID)
	second.FileID = first.FileID // 同一份合同
	cleanupReviews(t, db, first.ID, second.ID)

	if err := s.Create(ctx, first); err != nil {
		t.Fatalf("首次 Create: %v", err)
	}
	err := s.Create(ctx, second)
	if err == nil {
		t.Fatal("同一合同已有 pending 审查时，第二条必须被唯一键拦住")
	}
	if code := port.CodeOf(err); code != port.CodeConflict {
		t.Fatalf("错误码 = %v, want 40901", code)
	}
	if msg := port.AsAppError(err).Message; !strings.Contains(msg, "正在审查中") {
		t.Fatalf("文案 = %q, 应提示正在审查中", msg)
	}

	// 第一条落终态：active 生成列变 NULL，唯一约束自动释放，
	// 历史多条审查是允许的（规则更新后重跑）。
	if err := s.Update(ctx, &port.Review{
		ID:        first.ID,
		Status:    port.TaskSucceeded,
		RiskLevel: port.SeverityHigh,
		Score:     80,
		UpdatedAt: time.Now().UTC(),
	}); err != nil {
		t.Fatalf("把第一条改成 succeeded: %v", err)
	}
	if err := s.Create(ctx, second); err != nil {
		t.Fatalf("终态之后应允许为同一合同新建审查: %v", err)
	}
}

// TestReviewsActiveUniqueAllowsHistory 守住"历史多条"这一半：
// 多条 succeeded/failed 并存时唯一键不能挡路。
func TestReviewsActiveUniqueAllowsHistory(t *testing.T) {
	db := testDB(t)
	s := NewReviews(db)
	ctx := context.Background()

	userID := id.New()
	fileID := id.New()
	var ids []string
	for _, status := range []port.TaskStatus{port.TaskSucceeded, port.TaskFailed, port.TaskCancelled} {
		rv := &port.Review{ID: id.New(), FileID: fileID, UserID: userID, Status: status, TaskID: id.New()}
		ids = append(ids, rv.ID)
		if err := s.Create(ctx, rv); err != nil {
			t.Fatalf("历史记录 %s 应能插入: %v", status, err)
		}
	}
	cleanupReviews(t, db, ids...)
}
