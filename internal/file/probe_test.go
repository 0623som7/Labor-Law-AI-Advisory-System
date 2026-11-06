package file

import (
	"context"
	"errors"
	"strings"
	"testing"

	"laborlaw/internal/port"
)

// fakeProber 让自检用例不依赖本机装的是 poppler 还是 xpdf。
type fakeProber struct {
	out string
	err error
}

func (f fakeProber) Version(context.Context, string) (string, error) { return f.out, f.err }

func TestCheckBinaryPoppler(t *testing.T) {
	e := testExtractor(&fakeRunner{}, 50)
	e.probe = fakeProber{out: "pdftotext version 24.08.0\nCopyright 2005-2024 The Poppler Developers\n"}

	if err := e.CheckBinary(context.Background()); err != nil {
		t.Fatalf("poppler 应当通过自检: %v", err)
	}
}

// TestCheckBinaryXpdf 复现本机实况：xpdf 的 -v 打到 stdout 且退出码 99。
// 自检报错文案必须带出"不是 poppler 版"和实际版本首行。
func TestCheckBinaryXpdf(t *testing.T) {
	e := testExtractor(&fakeRunner{}, 50)
	e.probe = fakeProber{
		out: "pdftotext version 4.06 [www.xpdfreader.com]\nCopyright 1996-2025 Glyph & Cog, LLC\n",
		err: errors.New("exit status 99"),
	}

	err := e.CheckBinary(context.Background())
	if code := port.CodeOf(err); code != port.CodeInternal {
		t.Fatalf("错误码 = %v, want 50001", code)
	}
	msg := port.AsAppError(err).Message
	for _, want := range []string{"poppler", "4.06"} {
		if !strings.Contains(msg, want) {
			t.Fatalf("文案缺少 %q：%s", want, msg)
		}
	}
}

// TestCheckBinaryMissing 覆盖二进制根本不存在。
func TestCheckBinaryMissing(t *testing.T) {
	e := testExtractor(&fakeRunner{}, 50)
	e.probe = fakeProber{err: errors.New("exec: file not found")}

	err := e.CheckBinary(context.Background())
	if code := port.CodeOf(err); code != port.CodeInternal {
		t.Fatalf("错误码 = %v, want 50001", code)
	}
	if !strings.Contains(port.AsAppError(err).Message, "不可执行") {
		t.Fatalf("文案应提示不可执行，实际 %q", port.AsAppError(err).Message)
	}
}

// TestCheckBinaryEmptyOutput 覆盖"退出码 0 但版本输出为空"这种实现。
func TestCheckBinaryEmptyOutput(t *testing.T) {
	e := testExtractor(&fakeRunner{}, 50)
	e.probe = fakeProber{}

	err := e.CheckBinary(context.Background())
	if code := port.CodeOf(err); code != port.CodeInternal {
		t.Fatalf("错误码 = %v, want 50001", code)
	}
	if !strings.Contains(port.AsAppError(err).Message, "不是 poppler") {
		t.Fatalf("文案应说明不是 poppler，实际 %q", port.AsAppError(err).Message)
	}
}
