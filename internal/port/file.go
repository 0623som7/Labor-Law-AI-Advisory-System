package port

import (
	"fmt"
	"math"
)

// ParsedDoc 是 PDF 解析结果，也是 A 与 B 之间唯一的文件形态。
//
// 提供方：A。消费方：B。
// 并发安全：只读值对象，构造完成后可被多个 goroutine 共享。
// 错误：解析失败时 A 返回 AppError（CodeUnsupportedMedia / CodeNoTextLayer / CodeFileTooLarge），
// 绝不返回"空块列表的成功"。
// 变更：坐标协议冻结。只允许加字段，不允许改 BBox 语义。
//
// 为什么 B 拿不到 PDF 本身：坐标错 5pt 要等演示时才穿帮，是全项目最隐蔽的 bug。
// 所以坐标系只有 A 一处产出、前端渲染层一处消费，B 的中转环节只传 block_id + char_span。
type ParsedDoc struct {
	// FileID 对应 files 表主键。
	FileID string
	// PageCount 是总页数，必须等于 len(Pages)。
	PageCount int
	// Pages 按页序排列，下标 0 为第一页。
	Pages []ParsedPage
	// OCRUsed 表示文本层缺失、走了 OCR 兜底。前端需提示"扫描件识别，可能有偏差"。
	OCRUsed bool
	// Confidence 是解析置信度，取值为成功提取字符数 / 页面内容流估算字符数。
	// 低于 config.file.min_confidence（默认 0.6）时 A 直接返回 CodeNoTextLayer，不猜。
	Confidence float64
}

// ParsedPage 是一页的版面数据。
type ParsedPage struct {
	// Index 是页号，从 0 开始，与 PDF 阅读器显示的第 1 页对应 Index=0。
	// TODO(确认): 方案 §3.5 把 Index 误写成 float64，这里按语义取 int。
	Index int
	// Width 是页面宽度，单位 pt（1/72 英寸），来自 MediaBox。
	Width float64
	// Height 是页面高度，单位 pt，来自 MediaBox。
	Height float64
	// Blocks 是本页文本块，按阅读顺序排列（先自上而下，再自左向右）。
	Blocks []TextBlock
}

// TextBlock 是一段带坐标的文本。
//
// 坐标协议（冻结，写进 openapi 的 x-coordinate-system）：
//   - 单位 pt，PDF 用户空间；
//   - 原点左下，y 轴向上；
//   - 值域 [0, page.Width] × [0, page.Height]；
//   - 不做任何像素 / DPI / 屏幕换算，换算只在渲染层（PDF.js viewport）做。
type TextBlock struct {
	// ID 是 block 的稳定标识，B 回传抽取结果时用它定位坐标。
	ID string
	// Page 是所在页号，与 ParsedPage.Index 一致。冗余存放是为了 B 单独传一个 block 时也能定位。
	Page int
	// BBox 是包围盒 [x0, y0, x1, y1]，(x0,y0) 为左下角，(x1,y1) 为右上角，单位 pt。
	// 顺序写死为 x0,y0,x1,y1，与 PDF.js convertToViewportRectangle 的入参顺序一致。
	BBox [4]float64
	// Text 是文本内容，已按阅读顺序拼接，不含页眉页脚剔除逻辑（那是 B 的事）。
	Text string
	// CharStart 是全文连续偏移的起点，跨页连续累计，不允许每页重置。
	CharStart int
	// CharEnd 是全文连续偏移的终点，开区间（CharEnd 指向最后一个字符之后）。
	CharEnd int
}

// 编译期断言：包围盒校验必须能被外部按值调用，B 用它做抽取结果自检。
var _ = TextBlock{}

// Valid 判断 BBox 是否满足坐标协议。
//
// 用途：A 写入 text_blocks 前自检、B 回传 span 时自检、前端渲染前自检。
// 三个地方共用同一份判定，才不会出现"后端认为是正的、前端认为是歪的"。
// 判定内容：四个分量均为有限值；x0<=x1、y0<=y1；(x0,y0) 与 (x1,y1) 落在 [0,width]×[0,height] 内。
func (b TextBlock) Valid(pageWidth, pageHeight float64) error {
	for i, v := range b.BBox {
		if math.IsNaN(v) || math.IsInf(v, 0) {
			return fmt.Errorf("bbox 第 %d 个分量不是有限数", i)
		}
	}
	if b.BBox[0] > b.BBox[2] {
		return fmt.Errorf("bbox x0(%.2f) 大于 x1(%.2f)", b.BBox[0], b.BBox[2])
	}
	if b.BBox[1] > b.BBox[3] {
		return fmt.Errorf("bbox y0(%.2f) 大于 y1(%.2f)，注意原点在左下 y 轴向上", b.BBox[1], b.BBox[3])
	}
	if b.BBox[0] < 0 || b.BBox[2] > pageWidth {
		return fmt.Errorf("bbox x 越界 [%.2f,%.2f]，页宽 %.2f", b.BBox[0], b.BBox[2], pageWidth)
	}
	if b.BBox[1] < 0 || b.BBox[3] > pageHeight {
		return fmt.Errorf("bbox y 越界 [%.2f,%.2f]，页高 %.2f", b.BBox[1], b.BBox[3], pageHeight)
	}
	return nil
}

// CharSpan 返回 [CharStart, CharEnd) 的长度。
//
// 单独暴露是因为 B 抽取失败时最常见的两个症状就是 span 为 0 与 span 跨页，
// 调用方需要能直接断言长度而不必手算。
func (b TextBlock) CharSpan() int {
	n := b.CharEnd - b.CharStart
	if n < 0 {
		return 0
	}
	return n
}
