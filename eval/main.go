// Command eval 跑最小检索评测集：把"口语提问 → 期望命中的法条"跑一遍，
// 输出 hit@3 / hit@5 / 零召回。
//
// 为什么要这个命令：检索质量是整套咨询的可信度上限——召回不到原文，模型就只能
// 说"资料里没覆盖"（或者更糟，凭记忆编一条）。改一次词表、换一次 SQL，到底变好
// 还是变坏，靠单条手感判断不了，得有同一批用例的同一把尺子。
package main

import (
	"context"
	_ "embed"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"strings"
	"time"

	"laborlaw/internal/config"
	"laborlaw/internal/retrieval"
	"laborlaw/internal/store"
)

// cases.json 用 embed 而不是运行时读文件：跑评测时工作目录可能是仓库根，
// 也可能是 eval/，内嵌之后两份都能跑，且用例版本与代码版本永远一致。
//
//go:embed cases.json
var casesFile []byte

type evalCase struct {
	ID       string   `json:"id"`
	Question string   `json:"question"`
	Expected []string `json:"expected"`
}

// topK 固定 5：与线上 consult 的检索深度一致（agent 默认取 5 条）。
// hit@3 是"模型真正会优先看到的"，hit@5 是"进了候选池的"。
const topK = 5

func main() {
	dsn := flag.String("dsn", "", "MySQL DSN；不传则读 TEST_MYSQL_DSN")
	verbose := flag.Bool("v", false, "逐条打印召回的条号")
	flag.Parse()

	if *dsn == "" {
		*dsn = os.Getenv("TEST_MYSQL_DSN")
	}
	if *dsn == "" {
		fmt.Fprintln(os.Stderr, "缺少 DSN：用 -dsn 传入，或先设置 TEST_MYSQL_DSN")
		fmt.Fprintln(os.Stderr, "示例：TEST_MYSQL_DSN='laborlaw:laborlaw_dev@tcp(127.0.0.1:3307)/laborlaw?parseTime=true&charset=utf8mb4&loc=UTC' go run ./eval")
		os.Exit(2)
	}

	var cases []evalCase
	if err := json.Unmarshal(casesFile, &cases); err != nil {
		fmt.Fprintf(os.Stderr, "用例文件解析失败: %v\n", err)
		os.Exit(1)
	}
	if len(cases) == 0 {
		fmt.Fprintln(os.Stderr, "用例为空")
		os.Exit(1)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()

	db, err := store.OpenMySQL(ctx, config.MySQLConfig{DSN: *dsn, MaxOpenConns: 4, MaxIdleConns: 4})
	if err != nil {
		fmt.Fprintf(os.Stderr, "连接 MySQL 失败: %v\n", err)
		os.Exit(1)
	}
	defer db.Close()

	var total int
	if err := db.QueryRowContext(ctx, `SELECT COUNT(*) FROM statutes`).Scan(&total); err != nil {
		fmt.Fprintf(os.Stderr, "读取法条数失败: %v\n", err)
		os.Exit(1)
	}
	if total == 0 {
		fmt.Fprintln(os.Stderr, "statutes 表是空的：先跑 go run ./cmd/statuteseed -import")
		os.Exit(1)
	}

	retriever := retrieval.New(store.NewStatutes(db))

	fmt.Printf("法条库 %d 条，用例 %d 条，topK=%d\n\n", total, len(cases), topK)
	fmt.Printf("%-4s %-34s %-16s %-14s %-4s %-4s\n", "ID", "问题", "期望", "Top1", "@3", "@5")
	fmt.Println(strings.Repeat("-", 92))

	var hit3, hit5, empty int
	for _, c := range cases {
		hits, err := retriever.Search(ctx, c.Question, topK)
		if err != nil {
			fmt.Fprintf(os.Stderr, "%s 检索失败: %v\n", c.ID, err)
			os.Exit(1)
		}

		got := make([]string, 0, len(hits))
		for _, h := range hits {
			got = append(got, h.Statute.ID)
		}
		if len(got) == 0 {
			empty++
		}

		in3 := containsAny(got, c.Expected, 3)
		in5 := containsAny(got, c.Expected, topK)
		if in3 {
			hit3++
		}
		if in5 {
			hit5++
		}

		top := "-"
		if len(got) > 0 {
			top = got[0]
		}
		fmt.Printf("%-4s %-34s %-16s %-14s %-4s %-4s\n",
			c.ID, truncate(c.Question, 32), truncate(strings.Join(c.Expected, ","), 14),
			top, mark(in3), mark(in5))
		if *verbose {
			fmt.Printf("     召回: %s\n", strings.Join(got, " "))
		}
	}

	total3 := percent(hit3, len(cases))
	total5 := percent(hit5, len(cases))
	fmt.Println(strings.Repeat("-", 92))
	fmt.Printf("总结：%d 条用例，hit@3 %d/%d = %.1f%%，hit@5 %d/%d = %.1f%%，零召回 %d 条\n",
		len(cases), hit3, len(cases), total3, hit5, len(cases), total5, empty)

	// 失败不算崩：这是评测，不是断言。低于基线要在评审时被看见，
	// 但不该让"跑评测"这件事本身变成 CI 里的红灯——检索质量是渐进的。
	const baseline3 = 0.75
	if total3 < baseline3*100 {
		fmt.Printf("提示：hit@3 低于基线 %.0f%%，先看词表是不是漏了口语说法\n", baseline3*100)
	}
}

// containsAny 判断期望条号是否出现在前 n 条内。一条提问命中任意一个期望条号即算命中：
// 同一情形常有多条可选依据（例如"不交社保"同时对应解除权与参保义务），
// 要求全部命中会把"答对了但多引了一条"也判成失败。
func containsAny(got, expected []string, n int) bool {
	if n > len(got) {
		n = len(got)
	}
	for _, g := range got[:n] {
		for _, e := range expected {
			if g == e {
				return true
			}
		}
	}
	return false
}

func mark(ok bool) string {
	if ok {
		return "✓"
	}
	return "✗"
}

func percent(n, total int) float64 {
	if total == 0 {
		return 0
	}
	return float64(n) * 100 / float64(total)
}

// truncate 按 rune 截断：中文按字节截会切出半个字，表格会乱码。
func truncate(s string, maxRunes int) string {
	r := []rune(s)
	if len(r) <= maxRunes {
		return s
	}
	return string(r[:maxRunes-1]) + "…"
}
