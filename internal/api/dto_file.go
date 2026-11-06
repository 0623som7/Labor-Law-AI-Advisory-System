package api

import (
	"time"

	"laborlaw/internal/port"
)

// fileRecordDTO 是文件元数据在 HTTP 上的形状，字段名逐字来自
// contract/openapi.yaml 的 components.schemas.FileRecord。
//
// 与 parsedDocDTO 同样的理由：port.FileRecord 不打 tag，序列化形状由本层负责。
// created_at 用 time.Time 而不是字符串：RFC 3339 由 encoding/json 生成，
// 手拼格式时区一错，前端 new Date() 就会差 8 小时。
type fileRecordDTO struct {
	ID         string    `json:"id"`
	Filename   string    `json:"filename"`
	Ext        string    `json:"ext"`
	SizeBytes  int64     `json:"size_bytes"`
	PageCount  int       `json:"page_count"`
	OCRUsed    bool      `json:"ocr_used"`
	Confidence float64   `json:"confidence"`
	CreatedAt  time.Time `json:"created_at"`
}

// toFileRecordDTO 把文件记录映射成契约形状。nil 返回零值而不是 panic：
// handler 里每一处都判过 nil，这里再兜一层只为让"某天忘了判"变成一条空响应。
func toFileRecordDTO(rec *port.FileRecord) fileRecordDTO {
	if rec == nil {
		return fileRecordDTO{}
	}
	return fileRecordDTO{
		ID:         rec.ID,
		Filename:   rec.Filename,
		Ext:        rec.Ext,
		SizeBytes:  rec.SizeBytes,
		PageCount:  rec.PageCount,
		OCRUsed:    rec.OCRUsed,
		Confidence: rec.Confidence,
		CreatedAt:  rec.CreatedAt,
	}
}

// 本文件是解析结果在 HTTP 上的形状，字段名逐字来自 contract/openapi.yaml 的
// components.schemas.ParsedDoc / ParsedPage / TextBlock。
//
// 为什么不在 port.ParsedDoc 上加 json tag：port 是 A 与 B 共用且已冻结的契约，
// 加 tag 等于顺手改掉另一侧的序列化行为；而且内部字段名（FileID / CharStart）
// 会原样漏进响应体，前端按 file_id / char_start 取，一个都取不到。
// 先例是 internal/stream/frame.go 的 wireEvent：形状归 HTTP 层，port 只承载类型。
// 这里多写二十行映射，换来的是双方改结构时不会互相牵连。
type textBlockDTO struct {
	ID   string `json:"id"`
	Page int    `json:"page"`
	// BBox 必须是 [4]float64：序列化成 [x0,y0,x1,y1] 数组。
	// 变成对象（{"x0":...}）会让前端 convertToViewportRectangle 的解构静默拿到
	// undefined，画出一个 0×0 的框——错得最安静的一种。
	BBox      [4]float64 `json:"bbox"`
	Text      string     `json:"text"`
	CharStart int        `json:"char_start"`
	CharEnd   int        `json:"char_end"`
}

type parsedPageDTO struct {
	Index  int            `json:"index"`
	Width  float64        `json:"width"`
	Height float64        `json:"height"`
	Blocks []textBlockDTO `json:"blocks"`
}

type parsedDocDTO struct {
	FileID     string          `json:"file_id"`
	PageCount  int             `json:"page_count"`
	OCRUsed    bool            `json:"ocr_used"`
	Confidence float64         `json:"confidence"`
	Pages      []parsedPageDTO `json:"pages"`
}

// toParsedDocDTO 把解析结果映射成契约形状。
//
// 所有切片都初始化成空切片而不是留 nil：契约里 blocks 与 pages 是 type: array，
// 前端对它们直接 .map，收到 null 会抛 TypeError，json 里 null 与 [] 的差别
// 在这个位置是用户可见的白屏。doc 为 nil 时也返回 pages: []，让调用方不必分支。
func toParsedDocDTO(doc *port.ParsedDoc) parsedDocDTO {
	out := parsedDocDTO{Pages: make([]parsedPageDTO, 0)}
	if doc == nil {
		return out
	}

	out.FileID = doc.FileID
	out.PageCount = doc.PageCount
	out.OCRUsed = doc.OCRUsed
	out.Confidence = doc.Confidence
	out.Pages = make([]parsedPageDTO, 0, len(doc.Pages))

	for _, page := range doc.Pages {
		blocks := make([]textBlockDTO, 0, len(page.Blocks))
		for _, b := range page.Blocks {
			blocks = append(blocks, textBlockDTO{
				ID:        b.ID,
				Page:      b.Page,
				BBox:      b.BBox,
				Text:      b.Text,
				CharStart: b.CharStart,
				CharEnd:   b.CharEnd,
			})
		}
		out.Pages = append(out.Pages, parsedPageDTO{
			Index:  page.Index,
			Width:  page.Width,
			Height: page.Height,
			Blocks: blocks,
		})
	}
	return out
}
