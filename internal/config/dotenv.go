package config

import (
	"bufio"
	"errors"
	"fmt"
	"os"
	"strings"
)

// LoadDotEnv 读取 .env 并写进进程环境变量，**已存在的变量不覆盖**。
//
// 不覆盖是刻意的：容器里注入的真环境变量优先级必须高于仓库里的示例文件，
// 否则一次误提交的 .env 就能把线上配置改掉。
//
// 为什么不用 godotenv：本项目只需要 `KEY=VALUE` 与 `#` 注释两种语法，
// 四十行能写完；多一个依赖就多一份供应链风险，而这个文件要进生产镜像。
func LoadDotEnv(path string) (int, error) {
	f, err := os.Open(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			// 没有 .env 是正常情况：生产只走真实环境变量。
			return 0, nil
		}
		return 0, fmt.Errorf("打开 %s: %w", path, err)
	}
	defer f.Close()

	applied := 0
	scanner := bufio.NewScanner(f)
	lineNo := 0
	for scanner.Scan() {
		lineNo++
		line := strings.TrimSpace(scanner.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}

		key, value, ok := strings.Cut(line, "=")
		if !ok {
			return applied, fmt.Errorf("%s 第 %d 行缺少 '='", path, lineNo)
		}
		key = strings.TrimSpace(strings.TrimPrefix(key, "export "))
		if key == "" {
			return applied, fmt.Errorf("%s 第 %d 行变量名为空", path, lineNo)
		}
		value = unquote(strings.TrimSpace(value))

		if _, exists := os.LookupEnv(key); exists {
			continue
		}
		if err := os.Setenv(key, value); err != nil {
			return applied, fmt.Errorf("%s 第 %d 行写入环境变量 %s: %w", path, lineNo, key, err)
		}
		applied++
	}
	if err := scanner.Err(); err != nil {
		return applied, fmt.Errorf("读取 %s: %w", path, err)
	}
	return applied, nil
}

// unquote 去掉成对的单/双引号。值里允许出现空格与中文，因此不能 TrimSpace 了事。
func unquote(s string) string {
	if len(s) >= 2 {
		if (s[0] == '"' && s[len(s)-1] == '"') || (s[0] == '\'' && s[len(s)-1] == '\'') {
			return s[1 : len(s)-1]
		}
	}
	return s
}
