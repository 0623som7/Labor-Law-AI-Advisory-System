package store

import (
	"context"
	"database/sql"
	"strings"

	"laborlaw/internal/id"
	"laborlaw/internal/port"
)

// fileColumns 是 files 表的读取列序，与 scanFile 一一对应。
const fileColumns = `id, user_id, filename, ext, size_bytes, sha256,
	page_count, storage_path, ocr_used, confidence, created_at, updated_at`

// Files 是 port.FileStore 的 MySQL 实现。
type Files struct {
	db *sql.DB
}

// NewFiles 构造文件域 DAL。
func NewFiles(db *sql.DB) *Files {
	return &Files{db: db}
}

// 编译期断言：实现必须与 B 依赖的接口一致。
var _ port.FileStore = (*Files)(nil)

// Create 插入文件元数据。
//
// 时间列交给 DB 默认值：应用层传时间会让"同一份文件在两台机器上 created_at 差 8 小时"
// 这类问题在跨时区部署时才暴露。
func (f *Files) Create(ctx context.Context, rec *port.FileRecord) error {
	if rec == nil || rec.ID == "" || rec.UserID == "" {
		return port.New(port.CodeInvalidArgument, "文件记录缺少 id 或 user_id")
	}

	_, err := f.db.ExecContext(ctx, `INSERT INTO files
		(id, user_id, filename, ext, size_bytes, sha256, page_count, storage_path, ocr_used, confidence)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		rec.ID, rec.UserID, rec.Filename, rec.Ext, rec.SizeBytes, rec.SHA256,
		rec.PageCount, rec.StoragePath, rec.OCRUsed, rec.Confidence,
	)
	if err != nil {
		// 唯一索引 (user_id, sha256) 冲突：并发上传同一文件时 GetByDigest 可能都没命中，
		// 由数据库兜底，翻译成 40901 让调用方改用已存在的记录。
		if strings.Contains(err.Error(), "Duplicate entry") {
			return port.Wrap(port.CodeConflict, err, "同一文件已上传过")
		}
		return port.Wrap(port.CodeInternal, err, "写入文件记录失败")
	}
	return nil
}

// Get 按 ID 查文件，同时按 userID 过滤。
//
// 归属必须是查询条件而不是查完再判断：少写一次 WHERE 就是一次越权。
func (f *Files) Get(ctx context.Context, userID, fileID string) (*port.FileRecord, error) {
	row := f.db.QueryRowContext(ctx,
		`SELECT `+fileColumns+` FROM files WHERE id = ? AND user_id = ?`, fileID, userID)

	rec, err := scanFile(row)
	if err != nil {
		return nil, notFound(err, "文件不存在")
	}
	return rec, nil
}

// GetByDigest 按内容哈希查同用户已解析过的同一文件。
func (f *Files) GetByDigest(ctx context.Context, userID, sha256 string) (*port.FileRecord, error) {
	if sha256 == "" {
		return nil, port.New(port.CodeInvalidArgument, "sha256 为空")
	}

	row := f.db.QueryRowContext(ctx,
		`SELECT `+fileColumns+` FROM files WHERE user_id = ? AND sha256 = ?`, userID, sha256)

	rec, err := scanFile(row)
	if err != nil {
		return nil, notFound(err, "文件不存在")
	}
	return rec, nil
}

// Update 覆盖文件的可变字段。
//
// 用 RowsAffected 判断"是否真的存在"：UPDATE 命中 0 行不是错误，但对调用方来说
// 等于"这个文件不属于你或不存在"，必须报 40401，否则会出现"更新成功但什么都没变"。
func (f *Files) Update(ctx context.Context, rec *port.FileRecord) error {
	if rec == nil || rec.ID == "" || rec.UserID == "" {
		return port.New(port.CodeInvalidArgument, "文件记录缺少 id 或 user_id")
	}

	res, err := f.db.ExecContext(ctx, `UPDATE files SET
			filename = ?, ext = ?, size_bytes = ?, page_count = ?, storage_path = ?,
			ocr_used = ?, confidence = ?, updated_at = CURRENT_TIMESTAMP(3)
		WHERE id = ? AND user_id = ?`,
		rec.Filename, rec.Ext, rec.SizeBytes, rec.PageCount, rec.StoragePath,
		rec.OCRUsed, rec.Confidence, rec.ID, rec.UserID,
	)
	if err != nil {
		return port.Wrap(port.CodeInternal, err, "更新文件记录失败")
	}
	affected, err := res.RowsAffected()
	if err != nil {
		return port.Wrap(port.CodeInternal, err, "读取更新行数失败")
	}
	if affected == 0 {
		return port.New(port.CodeNotFound, "文件不存在")
	}
	return nil
}

// SavePages 写入解析结果，同一 fileID 先删后插，保证幂等。
//
// 整个操作在一个事务里：删完旧数据、插到一半失败时，用户看到的应当是
// "这次解析没生效"，而不是"半份页面 + 半份文本块"。
func (f *Files) SavePages(ctx context.Context, fileID string, pages []port.ParsedPage) error {
	if fileID == "" {
		return port.New(port.CodeInvalidArgument, "file_id 为空")
	}

	return withTx(ctx, f.db, func(tx *sql.Tx) error {
		if _, err := tx.ExecContext(ctx, `DELETE FROM text_blocks WHERE file_id = ?`, fileID); err != nil {
			return port.Wrap(port.CodeInternal, err, "清理旧文本块失败")
		}
		if _, err := tx.ExecContext(ctx, `DELETE FROM file_pages WHERE file_id = ?`, fileID); err != nil {
			return port.Wrap(port.CodeInternal, err, "清理旧页面失败")
		}

		for _, page := range pages {
			pageID := id.New()
			if _, err := tx.ExecContext(ctx,
				`INSERT INTO file_pages (id, file_id, page_no, width, height) VALUES (?, ?, ?, ?, ?)`,
				pageID, fileID, page.Index, page.Width, page.Height,
			); err != nil {
				return port.Wrap(port.CodeInternal, err, "写入第 %d 页失败", page.Index)
			}

			for _, block := range page.Blocks {
				// block_id 必须沿用解析层给的值：B 会把 finding 的坐标回填成
				// block_id + char_span，重新生成 ID 等于把这条链路剪断。
				blockID := block.ID
				if blockID == "" {
					blockID = id.New()
				}

				if _, err := tx.ExecContext(ctx, `INSERT INTO text_blocks
						(id, page_id, file_id, page_no, bbox_x0, bbox_y0, bbox_x1, bbox_y1,
						 text, char_start, char_end)
					VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
					blockID, pageID, fileID, page.Index,
					block.BBox[0], block.BBox[1], block.BBox[2], block.BBox[3],
					block.Text, block.CharStart, block.CharEnd,
				); err != nil {
					return port.Wrap(port.CodeInternal, err, "写入第 %d 页文本块失败", page.Index)
				}
			}
		}
		return nil
	})
}

// LoadPages 读回整份文档的解析结果。
//
// 两次查询而不是 JOIN：JOIN 会把每一行页面重复成"页数 × 块数"行，
// 2G 机器上解析 50 页文档时这个放大很可观。查询参数化的代价远小于内存。
func (f *Files) LoadPages(ctx context.Context, fileID string) ([]port.ParsedPage, error) {
	if fileID == "" {
		return nil, port.New(port.CodeInvalidArgument, "file_id 为空")
	}

	rows, err := f.db.QueryContext(ctx,
		`SELECT id, page_no, width, height FROM file_pages WHERE file_id = ? ORDER BY page_no`, fileID)
	if err != nil {
		return nil, port.Wrap(port.CodeInternal, err, "查询页面失败")
	}
	defer rows.Close()

	type pageRow struct {
		id     string
		index  int
		width  float64
		height float64
	}
	var pages []pageRow
	for rows.Next() {
		var p pageRow
		if err := rows.Scan(&p.id, &p.index, &p.width, &p.height); err != nil {
			return nil, port.Wrap(port.CodeInternal, err, "扫描页面失败")
		}
		pages = append(pages, p)
	}
	if err := rows.Err(); err != nil {
		return nil, port.Wrap(port.CodeInternal, err, "遍历页面失败")
	}
	if len(pages) == 0 {
		return nil, port.New(port.CodeNotFound, "该文件尚未解析")
	}

	blocks, err := f.loadBlocks(ctx, fileID)
	if err != nil {
		return nil, err
	}

	out := make([]port.ParsedPage, 0, len(pages))
	for _, p := range pages {
		out = append(out, port.ParsedPage{
			Index:  p.index,
			Width:  p.width,
			Height: p.height,
			Blocks: blocks[p.id],
		})
	}
	return out, nil
}

// loadBlocks 按页分组读回文本块，组内按 char_start 排序。
func (f *Files) loadBlocks(ctx context.Context, fileID string) (map[string][]port.TextBlock, error) {
	rows, err := f.db.QueryContext(ctx, `SELECT
			page_id, id, page_no, bbox_x0, bbox_y0, bbox_x1, bbox_y1, text, char_start, char_end
		FROM text_blocks WHERE file_id = ? ORDER BY page_no, char_start`, fileID)
	if err != nil {
		return nil, port.Wrap(port.CodeInternal, err, "查询文本块失败")
	}
	defer rows.Close()

	out := make(map[string][]port.TextBlock)
	for rows.Next() {
		var (
			pageID string
			b      port.TextBlock
		)
		if err := rows.Scan(&pageID, &b.ID, &b.Page, &b.BBox[0], &b.BBox[1], &b.BBox[2], &b.BBox[3],
			&b.Text, &b.CharStart, &b.CharEnd); err != nil {
			return nil, port.Wrap(port.CodeInternal, err, "扫描文本块失败")
		}
		out[pageID] = append(out[pageID], b)
	}
	if err := rows.Err(); err != nil {
		return nil, port.Wrap(port.CodeInternal, err, "遍历文本块失败")
	}
	return out, nil
}

// scanFile 把一行扫进 FileRecord。
func scanFile(row interface{ Scan(dest ...any) error }) (*port.FileRecord, error) {
	var rec port.FileRecord
	err := row.Scan(&rec.ID, &rec.UserID, &rec.Filename, &rec.Ext, &rec.SizeBytes, &rec.SHA256,
		&rec.PageCount, &rec.StoragePath, &rec.OCRUsed, &rec.Confidence,
		&rec.CreatedAt, &rec.UpdatedAt)
	if err != nil {
		return nil, err
	}
	return &rec, nil
}
