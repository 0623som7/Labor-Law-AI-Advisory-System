package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"os"
	"strings"
	"testing"

	"laborlaw/internal/id"
	"laborlaw/internal/port"
)

// 这些是集成测试：需要一个真实 MySQL。
//
// 用环境变量开关而不是 build tag：CI 里没有 MySQL，`go test ./...` 必须能跑；
// 本地起一个库、设上 TEST_MYSQL_DSN，同一份测试就变成真 SQL 的验证。
func testDB(t *testing.T) *sql.DB {
	t.Helper()

	dsn := os.Getenv("TEST_MYSQL_DSN")
	if dsn == "" {
		t.Skip("TEST_MYSQL_DSN 未设置，跳过集成测试")
	}

	db, err := sql.Open("mysql", dsn)
	if err != nil {
		t.Fatalf("打开数据库失败: %v", err)
	}
	if err := db.PingContext(context.Background()); err != nil {
		t.Fatalf("连接数据库失败: %v", err)
	}
	if _, err := Migrate(context.Background(), db); err != nil {
		t.Fatalf("执行迁移失败: %v", err)
	}

	t.Cleanup(func() {
		if err := db.Close(); err != nil {
			t.Logf("关闭数据库失败: %v", err)
		}
	})
	return db
}

// cleanupUser 清掉某用户的全部数据，用例之间互不影响。
func cleanupUser(t *testing.T, db *sql.DB, userID string) {
	t.Helper()
	t.Cleanup(func() {
		ctx := context.Background()
		for _, q := range []string{
			`DELETE FROM text_blocks WHERE file_id IN (SELECT id FROM files WHERE user_id = ?)`,
			`DELETE FROM file_pages WHERE file_id IN (SELECT id FROM files WHERE user_id = ?)`,
			`DELETE FROM files WHERE user_id = ?`,
		} {
			if _, err := db.ExecContext(ctx, q, userID); err != nil {
				t.Logf("清理失败: %v", err)
			}
		}
	})
}

func newFile(userID string) *port.FileRecord {
	return &port.FileRecord{
		ID:          id.New(),
		UserID:      userID,
		Filename:    "劳动合同.pdf",
		Ext:         "pdf",
		SizeBytes:   12345,
		SHA256:      id.New() + id.New(),
		PageCount:   0,
		StoragePath: "u/f/original.pdf",
		OCRUsed:     false,
		Confidence:  0,
	}
}

func TestFilesCreateGet(t *testing.T) {
	db := testDB(t)
	userID := id.New()
	cleanupUser(t, db, userID)

	files := NewFiles(db)
	rec := newFile(userID)

	if err := files.Create(context.Background(), rec); err != nil {
		t.Fatalf("Create() 返回错误: %v", err)
	}

	got, err := files.Get(context.Background(), userID, rec.ID)
	if err != nil {
		t.Fatalf("Get() 返回错误: %v", err)
	}
	if got.Filename != "劳动合同.pdf" || got.SizeBytes != 12345 || got.SHA256 != rec.SHA256 {
		t.Fatalf("读回内容不一致: %+v", got)
	}
	if got.CreatedAt.IsZero() {
		t.Fatal("created_at 应当由 DB 默认值填充")
	}

	// 换一个用户查同一个文件：必须是 40401，不能泄露"这个文件存在"。
	_, err = files.Get(context.Background(), id.New(), rec.ID)
	if code := port.CodeOf(err); code != port.CodeNotFound {
		t.Fatalf("越权查询错误码 = %v, want 40401", code)
	}
}

func TestFilesCreateRejectsDuplicateDigest(t *testing.T) {
	db := testDB(t)
	userID := id.New()
	cleanupUser(t, db, userID)

	files := NewFiles(db)
	first := newFile(userID)
	second := newFile(userID)
	second.SHA256 = first.SHA256 // 同一用户 + 同一哈希

	if err := files.Create(context.Background(), first); err != nil {
		t.Fatalf("首次 Create() 失败: %v", err)
	}
	err := files.Create(context.Background(), second)
	if code := port.CodeOf(err); code != port.CodeConflict {
		t.Fatalf("重复上传错误码 = %v, want 40901", code)
	}
}

func TestFilesGetByDigest(t *testing.T) {
	db := testDB(t)
	userID := id.New()
	cleanupUser(t, db, userID)

	files := NewFiles(db)
	rec := newFile(userID)
	if err := files.Create(context.Background(), rec); err != nil {
		t.Fatalf("Create() 失败: %v", err)
	}

	got, err := files.GetByDigest(context.Background(), userID, rec.SHA256)
	if err != nil {
		t.Fatalf("GetByDigest() 返回错误: %v", err)
	}
	if got.ID != rec.ID {
		t.Fatalf("ID = %s, want %s", got.ID, rec.ID)
	}

	_, err = files.GetByDigest(context.Background(), userID, "deadbeef")
	if code := port.CodeOf(err); code != port.CodeNotFound {
		t.Fatalf("未命中错误码 = %v, want 40401", code)
	}
}

func TestFilesUpdate(t *testing.T) {
	db := testDB(t)
	userID := id.New()
	cleanupUser(t, db, userID)

	files := NewFiles(db)
	rec := newFile(userID)
	if err := files.Create(context.Background(), rec); err != nil {
		t.Fatalf("Create() 失败: %v", err)
	}

	rec.PageCount = 9
	rec.Confidence = 0.98
	rec.OCRUsed = true
	if err := files.Update(context.Background(), rec); err != nil {
		t.Fatalf("Update() 返回错误: %v", err)
	}

	got, err := files.Get(context.Background(), userID, rec.ID)
	if err != nil {
		t.Fatalf("Get() 失败: %v", err)
	}
	if got.PageCount != 9 || got.Confidence < 0.97 || !got.OCRUsed {
		t.Fatalf("更新没有生效: %+v", got)
	}

	// 更新一个不存在的文件必须 40401，而不是静默成功。
	ghost := newFile(userID)
	ghost.ID = id.New()
	if err := files.Update(context.Background(), ghost); port.CodeOf(err) != port.CodeNotFound {
		t.Fatalf("更新不存在的文件错误码 = %v, want 40401", port.CodeOf(err))
	}
}

func samplePages() []port.ParsedPage {
	return []port.ParsedPage{
		{
			Index: 0, Width: 595.32, Height: 841.92,
			Blocks: []port.TextBlock{
				{ID: "b1", Page: 0, BBox: [4]float64{72, 700, 300, 720}, Text: "甲方：某某公司", CharStart: 0, CharEnd: 7},
				{ID: "b2", Page: 0, BBox: [4]float64{72, 660, 320, 680}, Text: "乙方：张三", CharStart: 7, CharEnd: 12},
			},
		},
		{
			Index: 1, Width: 595.32, Height: 841.92,
			Blocks: []port.TextBlock{
				{ID: "b3", Page: 1, BBox: [4]float64{72, 700, 400, 720}, Text: "试用期三个月", CharStart: 12, CharEnd: 18},
			},
		},
	}
}

func TestFilesSaveAndLoadPages(t *testing.T) {
	db := testDB(t)
	userID := id.New()
	cleanupUser(t, db, userID)

	files := NewFiles(db)
	rec := newFile(userID)
	if err := files.Create(context.Background(), rec); err != nil {
		t.Fatalf("Create() 失败: %v", err)
	}

	want := samplePages()
	if err := files.SavePages(context.Background(), rec.ID, want); err != nil {
		t.Fatalf("SavePages() 返回错误: %v", err)
	}

	got, err := files.LoadPages(context.Background(), rec.ID)
	if err != nil {
		t.Fatalf("LoadPages() 返回错误: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("页数 = %d, want 2", len(got))
	}
	if got[0].Width != 595.32 || got[0].Height != 841.92 {
		t.Fatalf("页面尺寸不一致: %+v", got[0])
	}
	if len(got[0].Blocks) != 2 || len(got[1].Blocks) != 1 {
		t.Fatalf("文本块数 = %d/%d, want 2/1", len(got[0].Blocks), len(got[1].Blocks))
	}

	first := got[0].Blocks[0]
	if first.Text != "甲方：某某公司" || first.CharStart != 0 || first.CharEnd != 7 {
		t.Fatalf("文本块内容不一致: %+v", first)
	}
	if first.BBox != [4]float64{72, 700, 300, 720} {
		t.Fatalf("坐标不一致: %v", first.BBox)
	}
	// B 依赖 block_id 回传坐标，ID 必须原样保留。
	if first.ID != "b1" {
		t.Fatalf("BlockID = %q, want b1", first.ID)
	}
}

// TestFilesSavePagesIdempotent 守住"先删后插"：重复解析不能出现两倍文本块。
func TestFilesSavePagesIdempotent(t *testing.T) {
	db := testDB(t)
	userID := id.New()
	cleanupUser(t, db, userID)

	files := NewFiles(db)
	rec := newFile(userID)
	if err := files.Create(context.Background(), rec); err != nil {
		t.Fatalf("Create() 失败: %v", err)
	}

	if err := files.SavePages(context.Background(), rec.ID, samplePages()); err != nil {
		t.Fatalf("第一次 SavePages() 失败: %v", err)
	}
	// 第二次只写一页，旧的第二页必须被清掉。
	if err := files.SavePages(context.Background(), rec.ID, samplePages()[:1]); err != nil {
		t.Fatalf("第二次 SavePages() 失败: %v", err)
	}

	got, err := files.LoadPages(context.Background(), rec.ID)
	if err != nil {
		t.Fatalf("LoadPages() 失败: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("页数 = %d, want 1（旧页没被清掉）", len(got))
	}
	if len(got[0].Blocks) != 2 {
		t.Fatalf("文本块数 = %d, want 2（重复插入会把这里变成 4）", len(got[0].Blocks))
	}
}

func TestFilesLoadPagesNotFound(t *testing.T) {
	db := testDB(t)

	files := NewFiles(db)
	_, err := files.LoadPages(context.Background(), id.New())
	if code := port.CodeOf(err); code != port.CodeNotFound {
		t.Fatalf("未解析文件的错误码 = %v, want 40401", code)
	}
}

// TestFilesLoadPagesEmptyBlocksIsArray 守住"无文本页的 blocks 不是 null"。
//
// 缺陷形态：LoadPages 用 blocks[p.id] 直接取值，没有块的页拿到 nil 切片，
// 序列化成 JSON null。前端 pages[i].blocks.map(...) 一碰就抛
// "Cannot read properties of null"，一个没有文本层的扫描页就能白屏整页。
func TestFilesLoadPagesEmptyBlocksIsArray(t *testing.T) {
	db := testDB(t)
	userID := id.New()
	cleanupUser(t, db, userID)

	files := NewFiles(db)
	rec := newFile(userID)
	if err := files.Create(context.Background(), rec); err != nil {
		t.Fatalf("Create() 失败: %v", err)
	}

	want := []port.ParsedPage{
		{
			Index: 0, Width: 595.32, Height: 841.92,
			Blocks: []port.TextBlock{
				{ID: "e1", Page: 0, BBox: [4]float64{72, 700, 300, 720}, Text: "有字的页", CharStart: 0, CharEnd: 4},
			},
		},
		// 第二页整页是扫描图，解析层不会给出任何块。
		{Index: 1, Width: 595.32, Height: 841.92},
	}
	if err := files.SavePages(context.Background(), rec.ID, want); err != nil {
		t.Fatalf("SavePages() 返回错误: %v", err)
	}

	got, err := files.LoadPages(context.Background(), rec.ID)
	if err != nil {
		t.Fatalf("LoadPages() 返回错误: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("页数 = %d, want 2", len(got))
	}
	if got[1].Blocks == nil {
		t.Fatal("无文本页的 Blocks 是 nil，序列化会变成 null")
	}
	if len(got[1].Blocks) != 0 {
		t.Fatalf("无文本页的 Blocks 长度 = %d, want 0", len(got[1].Blocks))
	}

	raw, err := json.Marshal(got[1])
	if err != nil {
		t.Fatalf("序列化失败: %v", err)
	}
	if !strings.Contains(string(raw), `"Blocks":[]`) {
		t.Fatalf("无文本页序列化 = %s, want 含 \"Blocks\":[]", raw)
	}
}

// TestFilesFinishParseAtomic 守住"元数据与页面同生共死"。
//
// 缺陷形态：finishParse 先 Update(page_count) 再 SavePages，第二次写失败就留下
// page_count＞0 但没有页面的记录——LoadPages 报 40401，重传又因为 page_count＞0
// 被当成"已经解析过"，这份合同从此永远没有文本层。
func TestFilesFinishParseAtomic(t *testing.T) {
	db := testDB(t)
	userID := id.New()
	cleanupUser(t, db, userID)

	files := NewFiles(db)
	rec := newFile(userID)
	ctx := context.Background()
	if err := files.Create(ctx, rec); err != nil {
		t.Fatalf("Create() 失败: %v", err)
	}

	// 写页面阶段失败：两个块用同一个 block_id，第二条 INSERT 撞主键。
	broken := samplePages()
	broken[0].Blocks[1].ID = broken[0].Blocks[0].ID
	err := files.FinishParse(ctx, rec.ID, 2, true, 0.9, broken)
	if err == nil {
		t.Fatal("FinishParse 本应失败，实际成功了")
	}

	got, gerr := files.Get(ctx, userID, rec.ID)
	if gerr != nil {
		t.Fatalf("Get() 失败: %v", gerr)
	}
	if got.PageCount != 0 {
		t.Fatalf("失败后 page_count = %d, want 0（元数据必须跟着页面一起回滚）", got.PageCount)
	}
	if got.OCRUsed {
		t.Fatal("失败后 ocr_used = true，元数据没有回滚")
	}
	if _, lerr := files.LoadPages(ctx, rec.ID); port.CodeOf(lerr) != port.CodeNotFound {
		t.Fatalf("失败后 LoadPages 错误码 = %v, want 40401（不该留下半份页面）", port.CodeOf(lerr))
	}

	// 正常路径：一次调用同时写元数据与页面。
	if err := files.FinishParse(ctx, rec.ID, 2, false, 0.97, samplePages()); err != nil {
		t.Fatalf("FinishParse() 失败: %v", err)
	}
	got, gerr = files.Get(ctx, userID, rec.ID)
	if gerr != nil {
		t.Fatalf("Get() 失败: %v", gerr)
	}
	if got.PageCount != 2 || got.OCRUsed || got.Confidence < 0.96 || got.Confidence > 0.98 {
		t.Fatalf("元数据回写不对: page_count=%d ocr=%v confidence=%v", got.PageCount, got.OCRUsed, got.Confidence)
	}
	pages, err := files.LoadPages(ctx, rec.ID)
	if err != nil {
		t.Fatalf("LoadPages() 失败: %v", err)
	}
	if len(pages) != 2 {
		t.Fatalf("页数 = %d, want 2", len(pages))
	}

	// 不存在的文件不能静默成功。
	if err := files.FinishParse(ctx, id.New(), 1, false, 0.5, samplePages()[:1]); port.CodeOf(err) != port.CodeNotFound {
		t.Fatalf("不存在的文件错误码 = %v, want 40401", port.CodeOf(err))
	}
}
