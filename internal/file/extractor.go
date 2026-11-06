package file

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os/exec"
	"strconv"
	"strings"
	"time"

	"laborlaw/internal/config"
	"laborlaw/internal/port"
)

// Extractor 用 poppler 的 pdftotext 把 PDF 解析成 port.ParsedDoc。
//
// 为什么走外部进程而不是纯 Go 库：中文 PDF 大量使用 CID 字体 + ToUnicode 映射，
// 纯 Go 解析器对它的支持参差不齐，最常见的失败不是报错，而是"提取出一串私用区
// 字符"——置信度看起来正常，规则一条都命中不了。poppler 在这个场景上有十几年积累。
//
// 代价是部署多一个二进制：镜像里要装 poppler-utils，Windows 开发机要装 poppler。
type Extractor struct {
	bin              string
	timeout          time.Duration
	maxPages         int
	maxBlocksPerPage int
	minConfidence    float64
	run              runner
	log              *slog.Logger
}

// NewExtractor 从配置构造解析器。
func NewExtractor(cfg config.Config) *Extractor {
	bin := strings.TrimSpace(cfg.File.PdfToTextPath)
	if bin == "" {
		bin = "pdftotext"
	}
	timeout := cfg.File.ExtractTimeout
	if timeout <= 0 {
		timeout = 30 * time.Second
	}
	maxPages := cfg.Storage.MaxPages
	if maxPages <= 0 {
		maxPages = 50
	}
	maxBlocks := cfg.File.MaxBlocksPerPage
	if maxBlocks <= 0 {
		maxBlocks = 500
	}

	return &Extractor{
		bin:              bin,
		timeout:          timeout,
		maxPages:         maxPages,
		maxBlocksPerPage: maxBlocks,
		minConfidence:    cfg.File.MinConfidence,
		run:              execRunner{},
		log:              slog.Default(),
	}
}

// logger 兜底到 slog.Default()：Extractor 可能被测试或别的包以零值方式构造，
// nil 解引用会把"低置信度警告"变成一次 500，比不打日志糟得多。
func (e *Extractor) logger() *slog.Logger {
	if e.log == nil {
		return slog.Default()
	}
	return e.log
}

// Parse 解析一份 PDF，返回可直接落库的 ParsedDoc。
//
// 返回值约定：**error 非 nil 时 doc 仍可能非 nil**（整份无文本层、页数超限的情形），
// 调用方可以据此把 confidence / page_count 落库，让用户在"解析过但没文本层"
// 和"从没解析过"之间能区分开。
//
// 文本层判定规则（真实合同常带扫描附件，一页扫描不该让整份不可用）：
//  1. 首页（Index==0）没有任何文本块 —— 认定整份是扫描件，返回 (doc, 42201)。
//     首页无字才是"这份文档是图片"的可靠信号：正文首页永远有内容，
//     而签字页、骑缝章页、附件经常是扫描进去的。
//  2. 首页有文字、但"有文字的页 / 总页数"低于 File.MinConfidence —— 仍然返回
//     (doc, nil)，只打一条 Warn。覆盖率低是数据质量问题，不是拒绝服务的理由；
//     OCRUsed 保持 false，本实现没有跑 OCR，置 true 是撒谎。
//  3. 占比正常 —— 行为不变。
func (e *Extractor) Parse(ctx context.Context, fileID, path string) (*port.ParsedDoc, error) {
	if strings.TrimSpace(path) == "" {
		return nil, port.New(port.CodeInvalidArgument, "文件路径为空")
	}

	callCtx, cancel := context.WithTimeout(ctx, e.timeout)
	defer cancel()

	// -l maxPages+1：多要一页用来判断"是否超限"，同时天然限制了输出体积。
	// 不加 -l 的话，一份 5000 页的 PDF 会把 XML 全部输出到内存里。
	out, err := e.run.Run(callCtx, e.bin,
		"-bbox-layout",
		"-enc", "UTF-8",
		"-q",
		"-f", "1",
		"-l", strconv.Itoa(e.maxPages+1),
		path,
		"-",
	)
	if err != nil {
		return nil, e.mapRunError(err)
	}

	pages, err := parsePoppler(out)
	if err != nil {
		return nil, port.Wrap(port.CodeUnsupportedMedia, err, "PDF 无法解析")
	}
	if len(pages) > e.maxPages {
		return nil, port.New(port.CodeFileTooLarge, "PDF 超过 %d 页", e.maxPages)
	}

	doc := &port.ParsedDoc{FileID: fileID, PageCount: len(pages)}
	doc.Pages = make([]port.ParsedPage, 0, len(pages))

	// offset 跨页累计，绝不每页重置：B 回传的是 block_id + char_span，
	// 偏移错一位，标注框就会框到相邻的字。
	offset := 0
	pagesWithText := 0

	for pi, p := range pages {
		blocks := make([]port.TextBlock, 0, len(p.Lines))
		for li, line := range p.Lines {
			if len(blocks) >= e.maxBlocksPerPage {
				break
			}
			runes := len([]rune(line.Text))
			blocks = append(blocks, port.TextBlock{
				// 上限 40 字符（迁移 0002）；不许改成 ULID —— B 靠 "fileID-页-行" 反推位置。
				ID:        fmt.Sprintf("%s-%d-%d", fileID, pi, li),
				Page:      pi,
				BBox:      line.BBox,
				Text:      line.Text,
				CharStart: offset,
				CharEnd:   offset + runes,
			})
			offset += runes
		}
		if len(blocks) > 0 {
			pagesWithText++
		}
		doc.Pages = append(doc.Pages, port.ParsedPage{
			Index:  pi,
			Width:  p.Width,
			Height: p.Height,
			Blocks: blocks,
		})
	}

	// 置信度 = 有文字的页 / 总页数。
	//
	// port 的注释写的是"提取字符数 / 内容流估算字符数"，但 poppler 不暴露内容流规模，
	// 那个公式在本实现里算不出来。用"有文本的页占比"是同一意图的近似：
	// 扫描件的每一页都取不到字，整份文档的占比会直接掉到 0。
	//
	// 零页单独返回：0/0 在 Go 里是 NaN，而 NaN < minConfidence 恒为 false，
	// 会让一份一页都没解析出来的 PDF 以 confidence=NaN 成功返回。
	if len(pages) == 0 {
		return doc, port.New(port.CodeNoTextLayer, "未检测到可提取的文本层（扫描件），请改用 OCR")
	}
	doc.Confidence = float64(pagesWithText) / float64(len(pages))

	// 首页无字才算扫描件，阈值在这里不参与判断：2 页里第 2 页是扫描签字页时
	// 占比是 0.5，低于 0.6，但那份合同的第一页完全可用，拒掉整份等于把
	// 可审查的正文一起丢掉。反过来，首页就没字，说明这份 PDF 是图片拼的。
	if len(doc.Pages[0].Blocks) == 0 {
		return doc, port.New(port.CodeNoTextLayer, "未检测到可提取的文本层（扫描件），请改用 OCR")
	}

	if doc.Confidence < e.minConfidence {
		// 只告警不拒绝。pages_with_text 一起打出来，运维才能分清是"附件扫描"
		// 还是"字体编码坏了"——后者 poppler 会提取出私用区乱码，块数正常但内容是垃圾。
		e.logger().Warn("PDF 文本层覆盖率偏低，按部分可解析继续",
			"page_count", doc.PageCount,
			"pages_with_text", pagesWithText,
			"confidence", doc.Confidence,
		)
	}
	return doc, nil
}

// mapRunError 把子进程失败翻译成业务码。
//
// 关键区分：超时是可重试的（CodeTaskTimeout），而加密/损坏是不可重试的
// （CodeUnsupportedMedia）。混成一个码会让前端对同一份坏文件反复重试。
func (e *Extractor) mapRunError(err error) error {
	if errors.Is(err, context.DeadlineExceeded) {
		return port.Wrap(port.CodeTaskTimeout, err, "PDF 解析超时（%s）", e.timeout)
	}
	if errors.Is(err, exec.ErrNotFound) {
		return port.Wrap(port.CodeInternal, err, "未找到 pdftotext，请检查 FILE_PDFTOTEXT_PATH")
	}

	detail := detailOf(err)
	switch {
	case strings.Contains(detail, "password"), strings.Contains(detail, "encrypted"):
		return port.Wrap(port.CodeUnsupportedMedia, err, "PDF 已加密，请先解除密码")
	case strings.Contains(detail, "executable file not found"), strings.Contains(detail, "no such file"):
		return port.Wrap(port.CodeInternal, err, "无法执行 pdftotext")
	default:
		return port.Wrap(port.CodeUnsupportedMedia, err, "PDF 解析失败")
	}
}
