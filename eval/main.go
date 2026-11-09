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
	"log/slog"
	"os"
	"strings"
	"time"

	"laborlaw/internal/config"
	"laborlaw/internal/llm"
	"laborlaw/internal/port"
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
	semanticFlag := flag.Bool("semantic", false, "等价于 -mode hybrid，保留旧写法")
	modeFlag := flag.String("mode", "", "keyword | semantic | hybrid | rerank；不传时看 -semantic 与环境变量")
	flag.Parse()

	// 命令行优先，环境变量兜底：开关在 .env 里，跑评测时不想每改一次都带上 flag。
	mode := strings.TrimSpace(*modeFlag)
	if mode == "" {
		if *semanticFlag || envTruthy(os.Getenv("RETRIEVAL_SEMANTIC")) {
			mode = "hybrid"
		} else {
			mode = "keyword"
		}
	}
	switch mode {
	case "keyword", "semantic", "hybrid", "rerank":
	default:
		fmt.Fprintf(os.Stderr, "未知 -mode %q，可选 keyword / semantic / hybrid / rerank\n", mode)
		os.Exit(2)
	}

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

	statutes := store.NewStatutes(db)
	keyword := retrieval.New(statutes)

	// search 是一个函数值而不是接口：几种检索器的共同方法只有一个，
	// 为此多定义一个接口只是让调用点多绕一层。
	search := func(q string) ([]port.StatuteHit, error) {
		return keyword.Search(ctx, q, topK)
	}
	label := "关键词"
	if mode != "keyword" {
		embedder, model, err := buildEmbedder()
		if err != nil {
			fmt.Fprintf(os.Stderr, "开启向量检索失败: %v\n", err)
			os.Exit(1)
		}
		index, err := retrieval.LoadSemanticIndex(ctx, statutes, "statute", model)
		if err != nil {
			fmt.Fprintf(os.Stderr, "语义索引构建失败: %v\n", err)
			os.Exit(1)
		}
		if index.Len() == 0 {
			fmt.Fprintln(os.Stderr, "embeddings 里没有可用的法条向量：先跑 go run ./cmd/embeddingseed")
			os.Exit(1)
		}

		switch mode {
		case "semantic":
			// 纯向量：不融合、不重排。单列一档是为了看清向量单独有多强，
			// 否则 RRF 的增益会跟向量的功劳记成一笔。
			search = func(q string) ([]port.StatuteHit, error) {
				vecs, err := embedder.Embed(ctx, model, []string{q})
				if err != nil {
					return nil, err
				}
				if len(vecs) == 0 || len(vecs[0]) == 0 {
					return []port.StatuteHit{}, nil
				}
				return index.Search(vecs[0], topK, 0)
			}
			label = fmt.Sprintf("纯向量(%s, %d 条)", model, index.Len())
		default:
			opts := []retrieval.HybridOption{}
			label = fmt.Sprintf("关键词+向量(%s, %d 条)", model, index.Len())
			if mode == "rerank" {
				reranker, err := buildReranker()
				if err != nil {
					fmt.Fprintf(os.Stderr, "开启重排失败: %v\n", err)
					os.Exit(1)
				}
				opts = append(opts, retrieval.WithReranker(reranker))
				label += " + 重排(" + rerankerModel() + ")"
			}
			hybrid := retrieval.NewHybrid(keyword, index, embedder, statutes, model, slog.Default(), opts...)
			search = func(q string) ([]port.StatuteHit, error) {
				return hybrid.Search(ctx, q, topK)
			}
		}
		// 语义那路失败会静默降级成关键词结果，所以这里要明说这次跑的是哪一路，
		// 否则"融合没提升"和"融合根本没生效"在输出上长得一模一样。
	}

	fmt.Printf("%-4s %-34s %-16s %-14s %-4s %-4s\n", "ID", "问题", "期望", "Top1", "@3", "@5")
	fmt.Println(strings.Repeat("-", 92))

	var hit3, hit5, empty int
	for _, c := range cases {
		hits, err := search(c.Question)
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

	// 失败不算崩：这是评测，不是断言。低于目标要在评审时被看见，
	// 但不该让"跑评测"这件事本身变成 CI 里的红灯——检索质量是渐进的。
	//
	// 叫 goal3 而不是 baseline3：baseline 是本轮实测下来的现状（见 README），
	// 随词表改动上下浮动；这个常量是"做到什么程度算合格"的目标线，不随运行变化。
	// 两个词混用，输出看起来就像"回归了"，其实只是离目标还有距离。
	const goal3 = 0.75
	if total3 < goal3*100 {
		fmt.Printf("提示：hit@3 %.0f%% 低于目标 %.0f%%，先看词表是不是漏了口语说法\n", total3, goal3*100)
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

// envTruthy 只认 on/true/1，与 internal/config 的布尔口径保持一致。
func envTruthy(raw string) bool {
	switch strings.ToLower(strings.TrimSpace(raw)) {
	case "on", "true", "1":
		return true
	}
	return false
}

// buildEmbedder 从环境变量拼出向量化客户端。
//
// 不调 config.Load()：那会把 JWT_SECRET、APP_ENV 一整套服务端校验也拉进来，
// 而评测是离线工具，只关心模型名、网关地址与密钥。
func buildEmbedder() (*llm.EmbeddingClient, string, error) {
	// .env 存在就吃进来，不存在不报错：CI 里是直接导环境变量。
	_, _ = config.LoadDotEnv(".env")

	cfg := config.LLMConfig{
		BaseURL:        strings.TrimSpace(os.Getenv("LLM_BASE_URL")),
		APIKey:         strings.TrimSpace(os.Getenv("LLM_API_KEY")),
		ModelEmbedding: strings.TrimSpace(os.Getenv("LLM_EMBEDDING_MODEL")),
	}
	if cfg.BaseURL == "" {
		cfg.BaseURL = "https://dashscope.aliyuncs.com/compatible-mode/v1"
	}
	if cfg.ModelEmbedding == "" {
		cfg.ModelEmbedding = "text-embedding-v3"
	}
	// Timeout 必须显式给：向量化客户端用 context.WithTimeout(ctx, Timeout) 包每次调用，
	// 零值等于立刻到期，所有请求瞬间失败并静默降级成关键词——评测看起来"没提升"，
	// 实际融合压根没跑过。
	cfg.Timeout = envDuration("LLM_TIMEOUT", 60*time.Second)
	if cfg.APIKey == "" {
		return nil, "", fmt.Errorf("缺少 LLM_API_KEY：向量化要调上游 %s", cfg.BaseURL)
	}
	return llm.NewEmbeddingClient(cfg), cfg.ModelEmbedding, nil
}

// envDuration 读时长，缺省或解析失败都退回 fallback：
// 评测工具不该因为 .env 里少一个键就跑不起来。
func rerankerModel() string {
	if m := strings.TrimSpace(os.Getenv("LLM_RERANK_MODEL")); m != "" {
		return m
	}
	return "qwen3-rerank"
}

func buildReranker() (*llm.RerankClient, error) {
	_, _ = config.LoadDotEnv(".env")

	key := strings.TrimSpace(os.Getenv("LLM_API_KEY"))
	if key == "" {
		return nil, fmt.Errorf("缺少 LLM_API_KEY：重排要调上游")
	}
	endpoint := strings.TrimSpace(os.Getenv("LLM_RERANK_URL"))
	if endpoint == "" {
		endpoint = "https://dashscope.aliyuncs.com/api/v1/services/rerank/text-rerank/text-rerank"
	}
	cfg := config.LLMConfig{
		APIKey:      key,
		RerankModel: rerankerModel(),
		// 与 buildEmbedder 同样的坑：零值 Timeout 立刻到期，重排会静默降级，
		// 评测表看着像"重排没效果"，实际一次都没发出去。
		Timeout: envDuration("LLM_TIMEOUT", 60*time.Second),
	}
	return llm.NewRerankClient(cfg, endpoint), nil
}

func envDuration(key string, fallback time.Duration) time.Duration {
	raw := strings.TrimSpace(os.Getenv(key))
	if raw == "" {
		return fallback
	}
	d, err := time.ParseDuration(raw)
	if err != nil || d <= 0 {
		return fallback
	}
	return d
}
