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
	probe            prober
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
		probe:            execRunner{},
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
	// 0 页必须当场失败：后面所有判定都建立在"至少有一页"之上。
	// 现在 parsePoppler 遇到空 doc 自己会报错，这里是第二道闸——
	// 一旦哪天换了更宽松的解析器（比如允许空 doc 返回空切片），
	// 0 页会一路走到 doc.Confidence = 0/0 = NaN，而 NaN < minConfidence 恒为 false，
	// 于是一份空文件以 page_count=0、confidence=NaN "成功"返回。
	if len(pages) == 0 {
		return nil, port.New(port.CodeUnsupportedMedia,
			"PDF 未解析出任何页面（pdftotext 输出为空或格式不认识）")
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
	// 分母非零由上面的 len(pages)==0 保证。这行注释是给将来改代码的人看的：
	// 0/0 在 Go 里是 NaN，而 NaN 与任何数比较都是 false，`NaN < minConfidence`
	// 也会是 false——"置信度太低"的兜底会静默失效，文件反而一路成功。
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
// 判定顺序是有原因的，不能随手调换：
//
//	1. ctx 超时 → 50401（可重试）
//	2. 找不到可执行文件 → 50001（配置错）
//	3. stderr 是 Usage / unknown option → 50001（装错工具）
//	4. password / encrypted → 41501（文件加密）
//	5. 其余 → 41501（文件损坏）
//
// 第 3 条必须排在第 4 条前面。实测过的坑：本机 PATH 里的 pdftotext 是 xpdf 4.06，
// 它不认 -bbox-layout，于是打印 Usage 文本并以 99 退出；而那段 Usage 里恰好有
// "owner password (for encrypted files)" 一行——顺序反了就会把"工具装错了"
// 报成"PDF 已加密，请先解除密码"，用户拿着一个完全正常的 PDF 去解密码。
func (e *Extractor) mapRunError(err error) error {
	if errors.Is(err, context.DeadlineExceeded) {
		return port.Wrap(port.CodeTaskTimeout, err, "PDF 解析超时（%s）", e.timeout)
	}
	if errors.Is(err, exec.ErrNotFound) {
		return port.Wrap(port.CodeInternal, err, "未找到 pdftotext，请检查 FILE_PDFTOTEXT_PATH")
	}

	detail := detailOf(err)

	// 工具装错优先于文件有问题：这两类错误都提示"换一个二进制"，而不是"换一个文件"。
	if strings.Contains(detail, "usage:") ||
		strings.Contains(detail, "unknown option") ||
		strings.Contains(detail, "invalid option") {
		return port.Wrap(port.CodeInternal, err,
			"pdftotext 版本不兼容：需要 poppler 版（支持 -bbox-layout），"+
				"当前可执行文件看起来是 xpdf 或其他实现。"+
				"请设置 FILE_PDFTOTEXT_PATH 指向 poppler 的 pdftotext")
	}
	if strings.Contains(detail, "password") || strings.Contains(detail, "encrypted") {
		return port.Wrap(port.CodeUnsupportedMedia, err, "PDF 已加密，请先解除密码")
	}
	if strings.Contains(detail, "executable file not found") || strings.Contains(detail, "no such file") {
		return port.Wrap(port.CodeInternal, err, "无法执行 pdftotext")
	}
	return port.Wrap(port.CodeUnsupportedMedia, err, "PDF 解析失败")
}
