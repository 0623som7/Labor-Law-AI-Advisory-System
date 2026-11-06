package file

import (
	"context"
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
	var b strings.Builder
	b.WriteString(`<?xml version="1.0" encoding="UTF-8"?><html><body><doc>`)
	for i := 0; i < n; i++ {
		b.WriteString(`<page width="600" height="800">`)
		if !(lastEmpty && i == n-1) {
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

func TestParseLowConfidence(t *testing.T) {
	r := &fakeRunner{out: []byte(pagesXML(2, true))} // 第二页空白
	e := testExtractor(r, 50)

	doc, err := e.Parse(context.Background(), "f", "/tmp/a.pdf")
	if err == nil {
		t.Fatal("期望 CodeNoTextLayer，实际成功")
	}
	if code := port.CodeOf(err); code != port.CodeNoTextLayer {
		t.Fatalf("错误码 = %v, want 42201", code)
	}
	// 关键：错误时仍要返回 doc，调用方才能把 confidence 落库。
	if doc == nil {
		t.Fatal("低置信度时 doc 不应为 nil")
	}
	if doc.Confidence != 0.5 {
		t.Fatalf("Confidence = %v, want 0.5", doc.Confidence)
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
