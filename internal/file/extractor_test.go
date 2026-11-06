package file

import (
	"bytes"
	"context"
	"log/slog"
	"strings"
	"testing"
	"time"

	"laborlaw/internal/config"
	"laborlaw/internal/port"
)

// fakeRunner 让单元测试不依赖 poppler 安装。
type fakeRunner struct {
	out  []byte
	err  error
	args []string
}

func (f *fakeRunner) Run(_ context.Context, bin string, args ...string) ([]byte, error) {
	f.args = append([]string{bin}, args...)
	if f.err != nil {
		return nil, f.err
	}
	return f.out, nil
}

func testExtractor(r *fakeRunner, maxPages int) *Extractor {
	cfg := config.Config{
		File: config.FileConfig{
			MinConfidence:    0.6,
			MaxBlocksPerPage: 100,
			PdfToTextPath:    "pdftotext",
			ExtractTimeout:   time.Second,
		},
		Storage: config.StorageConfig{MaxPages: maxPages},
	}
	e := NewExtractor(cfg)
	e.run = r
	return e
}

func pagesXML(n int, lastEmpty bool) string {
	emptyAt := make([]int, 0, 1)
	if lastEmpty {
		emptyAt = append(emptyAt, n-1)
	}
	return pagesXMLEmptyAt(n, emptyAt...)
}

// pagesXMLEmptyAt 生成 n 页 poppler 输出，emptyAt 指定的页号不含任何文字块。
//
// 为什么要能精确指定页号：首页空白与末页空白是两种完全不同的判定结果
// （整份扫描件 vs 带扫描签字的正常合同），只靠 lastEmpty 表达不了首页空白。
func pagesXMLEmptyAt(n int, emptyAt ...int) string {
	empty := make(map[int]bool, len(emptyAt))
	for _, i := range emptyAt {
		empty[i] = true
	}

	var b strings.Builder
	b.WriteString(`<?xml version="1.0" encoding="UTF-8"?><html><body><doc>`)
	for i := 0; i < n; i++ {
		b.WriteString(`<page width="600" height="800">`)
		if !empty[i] {
			b.WriteString(`<flow><block><line xMin="10" yMin="10" xMax="100" yMax="30">` +
				`<word xMin="10" yMin="10" xMax="100" yMax="30">正文</word></line></block></flow>`)
		}
		b.WriteString(`</page>`)
	}
	b.WriteString(`</doc></body></html>`)
	return b.String()
}

func TestParseSuccess(t *testing.T) {
	r := &fakeRunner{out: []byte(sampleXML)}
	e := testExtractor(r, 50)

	doc, err := e.Parse(context.Background(), "file-1", "/tmp/a.pdf")
	if err != nil {
		t.Fatalf("Parse() 返回错误: %v", err)
	}

	if doc.FileID != "file-1" {
		t.Fatalf("FileID = %q", doc.FileID)
	}
	if doc.PageCount != 2 || len(doc.Pages) != 2 {
		t.Fatalf("PageCount = %d, len(Pages) = %d, want 2", doc.PageCount, len(doc.Pages))
	}
	if doc.Confidence != 1.0 {
		t.Fatalf("Confidence = %v, want 1", doc.Confidence)
	}
	if doc.OCRUsed {
		t.Fatal("OCRUsed 应当为 false：OCR 在前端做")
	}

	// 字符偏移必须跨页连续。
	var wantStarts = []int{0, 6, 29, 35}
	var got []int
	for _, p := range doc.Pages {
		for _, b := range p.Blocks {
			got = append(got, b.CharStart)
		}
	}
	if len(got) != len(wantStarts) {
		t.Fatalf("block 数 = %d, want %d（got starts=%v）", len(got), len(wantStarts), got)
	}
	for i := range wantStarts {
		if got[i] != wantStarts[i] {
			t.Fatalf("第 %d 个 block 的 CharStart = %d, want %d（全部=%v）", i, got[i], wantStarts[i], got)
		}
	}

	first := doc.Pages[0].Blocks[0]
	if first.ID != "file-1-0-0" {
		t.Fatalf("BlockID = %q, want file-1-0-0", first.ID)
	}
	if first.Page != 0 {
		t.Fatalf("Page = %d, want 0", first.Page)
	}
}

// TestParseCommandArgs 守住 -f/-l：少一个参数就可能让一份超大 PDF 把内存打爆。
func TestParseCommandArgs(t *testing.T) {
	r := &fakeRunner{out: []byte(pagesXML(1, false))}
	e := testExtractor(r, 50)

	if _, err := e.Parse(context.Background(), "f", "/tmp/a.pdf"); err != nil {
		t.Fatalf("Parse() 返回错误: %v", err)
	}

	joined := strings.Join(r.args, " ")
	for _, want := range []string{"-bbox-layout", "-enc UTF-8", "-f 1", "-l 51", "/tmp/a.pdf"} {
		if !strings.Contains(joined, want) {
			t.Fatalf("命令参数缺少 %q：%s", want, joined)
		}
	}
}

// TestParseLowConfidence 原断言是"2 页里第 2 页空白 → 42201"。
// 那个断言把"覆盖率低"和"没有文本层"当成同一件事，而这正是本次要修的缺陷：
// 真实合同常带扫描的签字页/附件，首页有字的文档必须照常返回。
// 现在守的是新语义：仍返回 doc、err == nil、confidence = 0.5、OCRUsed 保持 false，
// 并且恰好打一条 Warn，带上 page_count / pages_with_text / confidence。
func TestParseLowConfidence(t *testing.T) {
	r := &fakeRunner{out: []byte(pagesXMLEmptyAt(2, 1))} // 第二页空白
	e := testExtractor(r, 50)

	var logs bytes.Buffer
	e.log = slog.New(slog.NewTextHandler(&logs, &slog.HandlerOptions{Level: slog.LevelDebug}))

	doc, err := e.Parse(context.Background(), "f", "/tmp/a.pdf")
	if err != nil {
		t.Fatalf("低置信度不应拒绝：%v", err)
	}
	if doc == nil {
		t.Fatal("doc 不应为 nil")
	}
	if doc.Confidence != 0.5 {
		t.Fatalf("Confidence = %v, want 0.5", doc.Confidence)
	}
	if doc.OCRUsed {
		t.Fatal("OCRUsed 必须为 false：本实现没跑 OCR，置 true 就是撒谎")
	}

	out := logs.String()
	if n := strings.Count(out, "level=WARN"); n != 1 {
		t.Fatalf("Warn 次数 = %d, want 1（got=%q）", n, out)
	}
	for _, want := range []string{"page_count=2", "pages_with_text=1", "confidence=0.5"} {
		if !strings.Contains(out, want) {
			t.Fatalf("Warn 缺少 %q：%s", want, out)
		}
	}
}

// TestParseFirstPageEmptyIsScan 首页无字 = 整份是图片拼的，必须 42201。
func TestParseFirstPageEmptyIsScan(t *testing.T) {
	r := &fakeRunner{out: []byte(pagesXMLEmptyAt(2, 0))}
	e := testExtractor(r, 50)

	doc, err := e.Parse(context.Background(), "f", "/tmp/a.pdf")
	if code := port.CodeOf(err); code != port.CodeNoTextLayer {
		t.Fatalf("错误码 = %v, want 42201", code)
	}
	if doc == nil {
		t.Fatal("扫描件错误也要返回 doc，调用方才能落库 confidence")
	}
	if doc.Confidence != 0.5 {
		t.Fatalf("Confidence = %v, want 0.5", doc.Confidence)
	}
	if !strings.Contains(port.AsAppError(err).Message, "扫描件") {
		t.Fatalf("文案应提示扫描件，实际 %q", port.AsAppError(err).Message)
	}
}

// TestParseLastPageEmptyStillOK 末页是扫描附件：整份照常可用。
func TestParseLastPageEmptyStillOK(t *testing.T) {
	r := &fakeRunner{out: []byte(pagesXMLEmptyAt(3, 2))}
	e := testExtractor(r, 50)

	doc, err := e.Parse(context.Background(), "f", "/tmp/a.pdf")
	if err != nil {
		t.Fatalf("末页扫描不应让整份失败：%v", err)
	}
	if len(doc.Pages) != 3 {
		t.Fatalf("页数 = %d, want 3", len(doc.Pages))
	}
	if doc.Pages[2].Blocks == nil {
		t.Fatal("空页的 Blocks 应当是空切片而不是 nil")
	}
	if len(doc.Pages[2].Blocks) != 0 {
		t.Fatalf("第 3 页应当是空的，实际 %d 个块", len(doc.Pages[2].Blocks))
	}
	const want = 2.0 / 3.0
	if diff := doc.Confidence - want; diff > 1e-9 || diff < -1e-9 {
		t.Fatalf("Confidence = %v, want ≈%v", doc.Confidence, want)
	}
	if doc.OCRUsed {
		t.Fatal("OCRUsed 必须为 false")
	}
}

// TestParseAllPagesEmptyIsScan 全空是第一页无字的特例，同样 42201。
func TestParseAllPagesEmptyIsScan(t *testing.T) {
	r := &fakeRunner{out: []byte(pagesXMLEmptyAt(3, 0, 1, 2))}
	e := testExtractor(r, 50)

	doc, err := e.Parse(context.Background(), "f", "/tmp/a.pdf")
	if code := port.CodeOf(err); code != port.CodeNoTextLayer {
		t.Fatalf("错误码 = %v, want 42201", code)
	}
	if doc == nil {
		t.Fatal("doc 不应为 nil")
	}
	if doc.Confidence != 0 {
		t.Fatalf("Confidence = %v, want 0", doc.Confidence)
	}
}

func TestParseTooManyPages(t *testing.T) {
	r := &fakeRunner{out: []byte(pagesXML(3, false))}
	e := testExtractor(r, 2)

	_, err := e.Parse(context.Background(), "f", "/tmp/a.pdf")
	if code := port.CodeOf(err); code != port.CodeFileTooLarge {
		t.Fatalf("错误码 = %v, want 41301", code)
	}
}

func TestParseEncrypted(t *testing.T) {
	r := &fakeRunner{err: &execError{detail: "Syntax Error: Incorrect password"}}
	e := testExtractor(r, 50)

	_, err := e.Parse(context.Background(), "f", "/tmp/a.pdf")
	if code := port.CodeOf(err); code != port.CodeUnsupportedMedia {
		t.Fatalf("错误码 = %v, want 41501", code)
	}
	if !strings.Contains(port.AsAppError(err).Message, "加密") {
		t.Fatalf("文案应提示加密，实际 %q", port.AsAppError(err).Message)
	}
}

func TestParseTimeout(t *testing.T) {
	r := &fakeRunner{err: context.DeadlineExceeded}
	e := testExtractor(r, 50)

	_, err := e.Parse(context.Background(), "f", "/tmp/a.pdf")
	if code := port.CodeOf(err); code != port.CodeTaskTimeout {
		t.Fatalf("错误码 = %v, want 50401", code)
	}
}

func TestParseInvalidOutput(t *testing.T) {
	r := &fakeRunner{out: []byte("这不是 XML")}
	e := testExtractor(r, 50)

	_, err := e.Parse(context.Background(), "f", "/tmp/a.pdf")
	if code := port.CodeOf(err); code != port.CodeUnsupportedMedia {
		t.Fatalf("错误码 = %v, want 41501", code)
	}
}

func TestParseEmptyPath(t *testing.T) {
	e := testExtractor(&fakeRunner{}, 50)

	_, err := e.Parse(context.Background(), "f", "   ")
	if code := port.CodeOf(err); code != port.CodeInvalidArgument {
		t.Fatalf("错误码 = %v, want 40001", code)
	}
}
