package file

import (
	"context"
	"os/exec"
	"strings"
	"time"

	"laborlaw/internal/port"
)

// probeTimeout 是自检的预算。`pdftotext -v` 只打印几行，1 秒都用不到；
// 给 5 秒是为了网络盘 / 杀毒软件扫描下的冷启动留余量。
const probeTimeout = 5 * time.Second

// prober 只负责"问一下这个二进制是谁"。
//
// 为什么不复用 runner：两种实现的"正常"长得不一样——poppler 的 -v 把版本打到
// stderr 且退出码 0，xpdf 的 -v 把版本打到 stdout 且退出码 99。用只收 stdout
// 的 runner 会把 poppler 的版本号丢光，用只收 stderr 的会把 xpdf 的丢光，
// 所以自检需要合并输出的通道，单独抽出来。
type prober interface {
	Version(ctx context.Context, bin string) (string, error)
}

// Version 执行 `<bin> -v` 并返回合并后的输出。
//
// 出错时也把已读到的输出返回：xpdf 会带着版本号以 99 退出，
// 上层要靠这段文字才能告诉用户"你现在装的是 xpdf 4.06"。
func (execRunner) Version(ctx context.Context, bin string) (string, error) {
	cmd := exec.CommandContext(ctx, bin, "-v")
	cmd.WaitDelay = waitDelay
	out, err := cmd.CombinedOutput()
	return string(out), err
}

// CheckBinary 自检 pdftotext 是不是能支持 -bbox-layout 的 poppler 版。
//
// 为什么需要它：xpdf 也提供同名的 pdftotext，但它没有 -bbox-layout，
// 装上之后上传接口会把每个 PDF 都判成"解析失败"，而错误信息只在真上传时才出现。
// 启动时打一条 Warn，能让"工具装错了"在第一次上传之前就被看到。
func (e *Extractor) CheckBinary(ctx context.Context) error {
	probeCtx, cancel := context.WithTimeout(ctx, probeTimeout)
	defer cancel()

	raw, err := e.prober().Version(probeCtx, e.bin)
	version := firstLine(raw)
	if err != nil && strings.TrimSpace(raw) == "" {
		return port.Wrap(port.CodeInternal, err, "pdftotext 不可执行（%s）", e.bin)
	}
	// 大小写不敏感：poppler 打的是 "The Poppler Developers"。
	if strings.Contains(strings.ToLower(raw), "poppler") {
		return nil
	}
	if version == "" {
		version = "无版本输出"
	}
	return port.New(port.CodeInternal,
		"pdftotext 不是 poppler 版（当前: %s），需要支持 -bbox-layout，请设置 FILE_PDFTOTEXT_PATH", version)
}

// prober 返回可用的自检实现，零值 Extractor 也能工作。
func (e *Extractor) prober() prober {
	if e.probe == nil {
		return execRunner{}
	}
	return e.probe
}

// firstLine 取输出的第一行非空内容，用于把版本号放进错误文案。
func firstLine(s string) string {
	for _, line := range strings.Split(s, "\n") {
		if trimmed := strings.TrimSpace(line); trimmed != "" {
			return trimmed
		}
	}
	return ""
}
