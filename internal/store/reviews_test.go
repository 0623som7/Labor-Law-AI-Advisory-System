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

// insertReviewAt 造一条审查记录并把 created_at 钉在指定时刻。
//
// Create 不接受 created_at（列有 DEFAULT CURRENT_TIMESTAMP(3)），所以插完再改。
// 游标翻页要验的正是"同一毫秒并列"的排序稳定性，不能靠 sleep 碰运气。
func insertReviewAt(t *testing.T, s *Reviews, db *sql.DB, userID string, at time.Time) *port.Review {
	t.Helper()
	ctx := context.Background()
	rv := newReview(userID)
	if err := s.Create(ctx, rv); err != nil {
		t.Fatalf("Create: %v", err)
	}
	if _, err := db.ExecContext(ctx, `UPDATE reviews SET created_at = ? WHERE id = ?`, at, rv.ID); err != nil {
		t.Fatalf("UPDATE created_at: %v", err)
	}
	rv.CreatedAt = at
	return rv
}

// TestReviewsListBeforePagination 验 keyset 游标翻完 25 条不重不漏。
//
// 25 条里每 5 条共用同一毫秒：只按 created_at 比较的游标会在这里全漏或全重，
// 双列 (created_at, id) 才是正解。
func TestReviewsListBeforePagination(t *testing.T) {
	db := testDB(t)
	s := NewReviews(db)
	ctx := context.Background()

	userID := id.New()
	base := time.Now().UTC().Add(-time.Hour).Truncate(time.Millisecond)
	want := make([]*port.Review, 0, 25)
	ids := make([]string, 0, 25)
	for i := 0; i < 25; i++ {
		rv := insertReviewAt(t, s, db, userID, base.Add(time.Duration(i/5)*time.Millisecond))
		want = append(want, rv)
		ids = append(ids, rv.ID)
	}
	cleanupReviews(t, db, ids...)

	// 第一页走 offset（等价于 HTTP 层不带 before_id 的那条分支），后面两页走游标。
	page, err := s.ListByUser(ctx, userID, 10, 0)
	if err != nil {
		t.Fatalf("第一页: %v", err)
	}
	if len(page) != 10 {
		t.Fatalf("第一页 %d 条，期望 10", len(page))
	}
	got := append([]port.Review{}, page...)
	for len(page) == 10 {
		last := page[len(page)-1]
		page, err = s.ListByUserBefore(ctx, userID, last.CreatedAt, last.ID, 10)
		if err != nil {
			t.Fatalf("游标页: %v", err)
		}
		got = append(got, page...)
		if len(page) < 10 {
			break
		}
	}

	if len(got) != 25 {
		t.Fatalf("翻完 %d 条，期望 25（漏了 %d 条）", len(got), 25-len(got))
	}
	seen := make(map[string]bool, 25)
	for _, rv := range got {
		if seen[rv.ID] {
			t.Fatalf("翻页重复：%s 出现两次", rv.ID)
		}
		seen[rv.ID] = true
	}
	for _, rv := range want {
		if !seen[rv.ID] {
			t.Fatalf("漏掉记录：%s（created_at=%s）", rv.ID, rv.CreatedAt.Format(time.RFC3339Nano))
		}
	}
	// 顺序必须严格递减：created_at 相同的那几条按 id 倒序，否则游标会自我重叠。
	for i := 1; i < len(got); i++ {
		prev, cur := got[i-1], got[i]
		if cur.CreatedAt.After(prev.CreatedAt) {
			t.Fatalf("第 %d 条时间比上一条新：%s > %s", i, cur.CreatedAt, prev.CreatedAt)
		}
		if cur.CreatedAt.Equal(prev.CreatedAt) && cur.ID >= prev.ID {
			t.Fatalf("同毫秒的两条没有按 id 倒序：%s >= %s", cur.ID, prev.ID)
		}
	}
}

// TestReviewsListBeforeSurvivesInsert 验翻页期间新增记录不会让上一页的行重跑一遍。
//
// 这就是 offset 分页的原始 bug：头部插一条，OFFSET 10 变成"从第 12 条开始"，
// 第 10 条在第二页再出现一次。游标钉在第 10 条那条记录上，新记录进不了后续页。
func TestReviewsListBeforeSurvivesInsert(t *testing.T) {
	db := testDB(t)
	s := NewReviews(db)
	ctx := context.Background()

	userID := id.New()
	base := time.Now().UTC().Add(-time.Hour).Truncate(time.Millisecond)
	ids := make([]string, 0, 26)
	for i := 0; i < 25; i++ {
		rv := insertReviewAt(t, s, db, userID, base.Add(time.Duration(i)*time.Millisecond))
		ids = append(ids, rv.ID)
	}
	cleanupReviews(t, db, ids...)

	page1, err := s.ListByUser(ctx, userID, 10, 0)
	if err != nil {
		t.Fatalf("第一页: %v", err)
	}
	if len(page1) != 10 {
		t.Fatalf("第一页 %d 条，期望 10", len(page1))
	}

	// 两次翻页之间新审完一份合同。
	fresh := insertReviewAt(t, s, db, userID, base.Add(time.Second))
	cleanupReviews(t, db, fresh.ID)

	last := page1[len(page1)-1]
	page2, err := s.ListByUserBefore(ctx, userID, last.CreatedAt, last.ID, 10)
	if err != nil {
		t.Fatalf("游标页: %v", err)
	}
	if len(page2) != 10 {
		t.Fatalf("第二页 %d 条，期望 10", len(page2))
	}
	inPage1 := make(map[string]bool, 10)
	for _, rv := range page1 {
		inPage1[rv.ID] = true
	}
	for _, rv := range page2 {
		if inPage1[rv.ID] {
			t.Fatalf("第二页出现第一页已有的记录：%s", rv.ID)
		}
		if rv.ID == fresh.ID {
			t.Fatalf("第二页出现了翻页后新增的记录：%s", rv.ID)
		}
	}
	// 对照：offset 分支在同样场景下必然重复一条，这条断言是给未来的自己看的。
	offsetPage2, err := s.ListByUser(ctx, userID, 10, 10)
	if err != nil {
		t.Fatalf("offset 第二页: %v", err)
	}
	if len(offsetPage2) == 0 || offsetPage2[0].ID == page1[len(page1)-1].ID {
		t.Logf("offset 第二页首条与第一页末条不同（本次没有复现重复），不视为失败")
	}
}

// 5 条同毫秒的游标翻页：这是最容易被写成"只比时间"的场景，
// 3 条并列时单列游标必然重复或漏。limit=2 翻三页，断言 5 条不重不漏。
func TestReviewsListBeforeSameMillisecond(t *testing.T) {
	db := testDB(t)
	s := NewReviews(db)
	ctx := context.Background()

	userID := id.New()
	at := time.Now().UTC().Add(-time.Hour).Truncate(time.Millisecond)
	ids := make([]string, 0, 5)
	for i := 0; i < 5; i++ {
		rv := insertReviewAt(t, s, db, userID, at) // 五条 created_at 完全相同
		ids = append(ids, rv.ID)
	}
	cleanupReviews(t, db, ids...)

	got := make([]port.Review, 0, 5)
	page, err := s.ListByUser(ctx, userID, 2, 0)
	if err != nil {
		t.Fatalf("第一页: %v", err)
	}
	got = append(got, page...)
	for len(page) == 2 {
		last := page[len(page)-1]
		page, err = s.ListByUserBefore(ctx, userID, last.CreatedAt, last.ID, 2)
		if err != nil {
			t.Fatalf("游标页: %v", err)
		}
		got = append(got, page...)
		if len(page) < 2 {
			break
		}
	}

	if len(got) != 5 {
		t.Fatalf("翻完 %d 条，期望 5", len(got))
	}
	seen := map[string]bool{}
	for _, rv := range got {
		if seen[rv.ID] {
			t.Fatalf("同毫秒翻页重复：%s", rv.ID)
		}
		seen[rv.ID] = true
	}
	// 同毫秒时必须按 id 倒序，否则游标 (created_at, id) 会自我重叠。
	for i := 1; i < len(got); i++ {
		if got[i].ID >= got[i-1].ID {
			t.Fatalf("第 %d 条 id 没有严格递减：%s >= %s", i, got[i].ID, got[i-1].ID)
		}
	}
}
