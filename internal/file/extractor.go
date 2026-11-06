package file

import (
	"context"
	"errors"
	"fmt"
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
	}
}

// Parse 解析一份 PDF，返回可直接落库的 ParsedDoc。
//
// 返回值约定：**error 非 nil 时 doc 仍可能非 nil**（置信度过低、页数超限的情形），
// 调用方可以据此把 confidence / page_count 落库，让用户在"解析过但没文本层"
// 和"从没解析过"之间能区分开。
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

	// 置信度 = 有文字的行所属页数 / 总页数。
	//
	// port 的注释写的是"提取字符数 / 内容流估算字符数"，但 poppler 不暴露内容流规模，
	// 那个公式在本实现里算不出来。用"有文本的页占比"是同一意图的近似：
	// 扫描件的每一页都取不到字，整份文档的占比会直接掉到 0。
	doc.Confidence = float64(pagesWithText) / float64(len(pages))

	if doc.Confidence < e.minConfidence {
		return doc, port.New(port.CodeNoTextLayer,
			"未检测到可提取的文本层（%.0f%% 的页面没有文字），可能是扫描件", doc.Confidence*100)
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
