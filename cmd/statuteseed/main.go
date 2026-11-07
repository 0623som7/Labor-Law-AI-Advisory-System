// statuteseed 把权威来源的法律原文 HTML 解析成种子数据。
//
// 为什么不手敲条文进 JSON：这个项目的第一铁律是"AI 不许直写法条原文"。
// 法条只能来自权威原文，解析器切分 + 条号连续性硬校验，比人敲键盘可靠，
// 而且以后收录新法律只要再下载一份原文。
//
// 用法：
//
//	go run ./cmd/statuteseed
//	go run ./cmd/statuteseed -source deploy/seed/source -out deploy/seed/statutes.json
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"path/filepath"
)

func main() {
	var (
		sourceDir = flag.String("source", filepath.Join("deploy", "seed", "source"), "权威原文 HTML 所在目录")
		outPath   = flag.String("out", filepath.Join("deploy", "seed", "statutes.json"), "种子 JSON 输出路径")
	)
	flag.Parse()

	if err := run(*sourceDir, *outPath); err != nil {
		fmt.Fprintln(os.Stderr, "statuteseed:", err)
		os.Exit(1)
	}
}

func run(sourceDir, outPath string) error {
	var all []statuteJSON
	for _, spec := range sources {
		raw, err := os.ReadFile(filepath.Join(sourceDir, spec.File))
		if err != nil {
			return fmt.Errorf("读取 %s 原文失败（先按 README 下载权威原文）: %w", spec.Law, err)
		}
		statutes, err := parseStatutes(string(raw), spec)
		if err != nil {
			return err
		}
		for _, s := range statutes {
			all = append(all, toJSON(s))
		}
		fmt.Printf("%-14s %3d 条  %s … %s\n", spec.IDPrefix, len(statutes), statutes[0].ID, statutes[len(statutes)-1].ID)
	}

	data, err := json.MarshalIndent(all, "", "  ")
	if err != nil {
		return fmt.Errorf("序列化种子数据: %w", err)
	}
	if err := os.MkdirAll(filepath.Dir(outPath), 0o755); err != nil {
		return fmt.Errorf("创建输出目录: %w", err)
	}
	if err := os.WriteFile(outPath, append(data, '\n'), 0o644); err != nil {
		return fmt.Errorf("写入种子文件: %w", err)
	}
	fmt.Printf("共 %d 条法条 → %s\n", len(all), outPath)
	return nil
}
