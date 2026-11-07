// statuteseed 把权威来源的法律原文 HTML 解析成种子数据，可选地灌进 MySQL。
//
// 为什么不手敲条文进 JSON：这个项目的第一铁律是"AI 不许直写法条原文"。
// 法条只能来自权威原文，解析器切分 + 条号连续性硬校验，比人敲键盘可靠，
// 而且以后收录新法律只要再下载一份原文。
//
// 用法：
//
//	go run ./cmd/statuteseed                 # 只生成 deploy/seed/statutes.json
//	go run ./cmd/statuteseed -import         # 生成后全量重建数据库法条表
//	go run ./cmd/statuteseed -import -query 经济补偿
//
// 数据库连接读 .env 的 MYSQL_DSN，与 API 服务同源，避免两套连接配置漂移。
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"laborlaw/internal/config"
	"laborlaw/internal/port"
	"laborlaw/internal/store"
)

func main() {
	var (
		sourceDir = flag.String("source", filepath.Join("deploy", "seed", "source"), "权威原文 HTML 所在目录")
		outPath   = flag.String("out", filepath.Join("deploy", "seed", "statutes.json"), "种子 JSON 输出路径")
		importDB  = flag.Bool("import", false, "生成后全量重建 MySQL 法条表")
		query     = flag.String("query", "", "导入完成后跑一次检索冒烟，如 经济补偿")
	)
	flag.Parse()

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()

	items, err := build(*sourceDir, *outPath)
	if err != nil {
		fail(err)
	}
	if !*importDB {
		return
	}
	if err := importAndVerify(ctx, items, *query); err != nil {
		fail(err)
	}
}

func fail(err error) {
	fmt.Fprintln(os.Stderr, "statuteseed:", err)
	os.Exit(1)
}

// build 解析全部法律原文并写出种子 JSON。
func build(sourceDir, outPath string) ([]port.Statute, error) {
	var (
		all  []port.Statute
		rows []statuteJSON
	)
	for _, spec := range sources {
		raw, err := os.ReadFile(filepath.Join(sourceDir, spec.File))
		if err != nil {
			return nil, fmt.Errorf("读取 %s 原文失败（先按 deploy/seed/README.md 下载权威原文）: %w", spec.Law, err)
		}
		statutes, err := parseStatutes(string(raw), spec)
		if err != nil {
			return nil, err
		}
		all = append(all, statutes...)
		for _, s := range statutes {
			rows = append(rows, toJSON(s))
		}
		fmt.Printf("%-6s %3d 条  %s … %s\n", spec.IDPrefix, len(statutes), statutes[0].ID, statutes[len(statutes)-1].ID)
	}

	data, err := json.MarshalIndent(rows, "", "  ")
	if err != nil {
		return nil, fmt.Errorf("序列化种子数据: %w", err)
	}
	if err := os.MkdirAll(filepath.Dir(outPath), 0o755); err != nil {
		return nil, fmt.Errorf("创建输出目录: %w", err)
	}
	if err := os.WriteFile(outPath, append(data, '\n'), 0o644); err != nil {
		return nil, fmt.Errorf("写入种子文件: %w", err)
	}
	fmt.Printf("共 %d 条法条 → %s\n", len(all), outPath)
	return all, nil
}

// spotCheckIDs 是导入后必须逐字复核的条文。
//
// 挑这几条不是因为它们随机，而是因为它们是咨询里最容易被模型背错的几条：
// 二倍工资的适用期间、赔偿金的二倍基数、加班费的三档比例。
// 行数对得上不代表内容对，必须有一次逐字比对。
var spotCheckIDs = []string{"LCL-38", "LCL-82", "LCL-87", "LL-44"}

// importAndVerify 全量重建法条表，并做行数、逐字、检索三层验收。
func importAndVerify(ctx context.Context, items []port.Statute, query string) error {
	// 与 cmd/api 同源加载 .env，避免工具与服务的连接配置两套漂移。
	if _, err := config.LoadDotEnv(".env"); err != nil {
		return fmt.Errorf("读取 .env: %w", err)
	}
	cfg, err := config.Load()
	if err != nil {
		return fmt.Errorf("读取配置: %w", err)
	}
	if cfg.MySQL.DSN == "" {
		return fmt.Errorf("MYSQL_DSN 为空，无法导入")
	}
	db, err := store.OpenMySQL(ctx, cfg.MySQL)
	if err != nil {
		return err
	}
	defer db.Close()

	st := store.NewStatutes(db)
	before, err := st.Count(ctx)
	if err != nil {
		return err
	}
	if err := st.ReplaceAll(ctx, items); err != nil {
		return err
	}
	after, err := st.Count(ctx)
	if err != nil {
		return err
	}
	fmt.Printf("法条表重建完成：%d → %d 行\n", before, after)
	if after != len(items) {
		return fmt.Errorf("导入后行数 %d 与种子条数 %d 不一致", after, len(items))
	}

	want := make(map[string]string, len(items))
	for _, item := range items {
		want[item.ID] = item.Text
	}
	for _, statuteID := range spotCheckIDs {
		got, err := st.Get(ctx, statuteID)
		if err != nil {
			return fmt.Errorf("复核 %s: %w", statuteID, err)
		}
		if got.Text != want[statuteID] {
			return fmt.Errorf("%s 入库原文与种子不一致", statuteID)
		}
	}
	fmt.Printf("逐字复核通过：%v\n", spotCheckIDs)

	if query != "" {
		hits, err := st.SearchText(ctx, query, "", 5)
		if err != nil {
			return fmt.Errorf("检索冒烟: %w", err)
		}
		fmt.Printf("检索 %q 命中 %d 条：\n", query, len(hits))
		for _, h := range hits {
			fmt.Printf("  %-8s %s %s（命中 %.0f 次）\n", h.Statute.ID, h.Statute.Article, h.Statute.Title, h.Score)
		}
	}
	return nil
}
