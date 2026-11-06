package file

import (
	"context"
	"errors"
	"fmt"
	"os/exec"
	"strings"
	"time"
)

// maxOutputBytes 是子进程 stdout 的硬上限。
//
// 页数已经用 -f/-l 限制过，正常合同不会超过几 MB；这个上限是防"畸形 PDF 让
// poppler 输出几百 MB XML"把 2G 机器打爆，属于兜底而不是常规路径。
const maxOutputBytes = 32 << 20

// waitDelay 是进程被 kill 后等待其退出的时间。
//
// 不设的话，子进程如果打开了管道又没关，Wait 会一直挂住——表现为"解析卡死"，
// 而 ctx 明明已经超时了。
const waitDelay = 5 * time.Second

// runner 抽象"执行外部命令并返回 stdout"。
//
// 抽出接口只为测试：真机上跑 poppler 需要装二进制，CI 与单元测试都不该依赖它。
type runner interface {
	Run(ctx context.Context, bin string, args ...string) ([]byte, error)
}

// execRunner 是 runner 的真实实现。
type execRunner struct{}

// Run 执行命令并返回 stdout，stderr 只在失败时作为错误信息的一部分。
//
// 用 exec.CommandContext 而不是自己管进程：ctx 到点会 kill 子进程，
// 配合 WaitDelay 保证不会因为管道未关而永久阻塞。
func (execRunner) Run(ctx context.Context, bin string, args ...string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, bin, args...)
	cmd.WaitDelay = waitDelay

	var stdout, stderr strings.Builder
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	err := cmd.Run()
	if err != nil {
		// ctx 超时与"命令本身失败"必须分开：前者是任务超时（可重试），
		// 后者是文件有问题（重试一万次也一样）。
		if ctxErr := ctx.Err(); ctxErr != nil {
			return nil, fmt.Errorf("执行 %s 超时: %w", bin, ctxErr)
		}
		detail := strings.TrimSpace(stderr.String())
		if detail == "" {
			detail = err.Error()
		}
		return nil, &execError{detail: detail, err: err}
	}

	if stdout.Len() > maxOutputBytes {
		return nil, fmt.Errorf("解析输出超过 %d 字节，疑似畸形 PDF", maxOutputBytes)
	}
	return []byte(stdout.String()), nil
}

// execError 携带子进程的 stderr，便于上层区分"加密 PDF"与"文件损坏"。
type execError struct {
	detail string
	err    error
}

func (e *execError) Error() string {
	return fmt.Sprintf("pdftotext 失败: %s", e.detail)
}

func (e *execError) Unwrap() error {
	return e.err
}

// detailOf 取出子进程的错误输出，供上层做关键字判断。
func detailOf(err error) string {
	var ee *execError
	if errors.As(err, &ee) {
		return strings.ToLower(ee.detail)
	}
	return strings.ToLower(err.Error())
}
