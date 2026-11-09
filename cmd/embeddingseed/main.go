// embeddingseed 把法条表向量化，写进 embeddings 表；可选跑检索自检。
//
// 为什么不走 port.StatuteStore：那个接口没有 List，而 port 是两人共用的冻结
// 契约。为一个离线运维工具往契约上加方法，等于让另一侧为它的便利签字——
// 这个工具直接持 *sql.DB 读 statutes 表是更小的代价。
//
// 用法：
//
//	go run ./cmd/embeddingseed                      # 全量向量化
//	go run ./cmd/embeddingseed -limit 10 -verbose   # 先拿 10 条试水
//	go run ./cmd/embeddingseed -query '加班费|二倍工资|拖欠工资'
package main

import (
	"context"
	"database/sql"
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

// ownerTypeStatute 与期 2 的 SaveEmbeddings 约定一致，混合检索按它筛选。
const ownerTypeStatute = "statute"

// defaultMinScore 是自检时的相似度下限。
//
// 只给这一个显式调用点：语义相似度的绝对值没有跨模型可比性，客户端里不给
// 默认值，否则换模型就失效。
const defaultMinScore = 0.5

// seedRetries 是一次批次失败后的尝试次数（含首次）。
const seedRetries = 3

func main() {
	var (
		dsn      = flag.String("dsn", "", "MySQL DSN，缺省读 .env 的 MYSQL_DSN")
		model    = flag.String("model", "", "向量模型名，缺省读 LLM_EMBEDDING_MODEL")
		batch    = flag.Int("batch", 10, "每批条数，上限 10（上游单请求限制）")
		limit    = flag.Int("limit", 0, "只处理前 N 条，0 表示全部")
		query    = flag.String("query", "", "自检查询，多个用 | 分隔")
		minScore = flag.Float64("min-score", defaultMinScore, "自检相似度下限，0 表示不过滤")
		verbose  = flag.Bool("verbose", false, "打印每条法条的处理进度")
	)
	flag.Parse()

	if err := run(*dsn, *model, *batch, *limit, *query, float32(*minScore), *verbose); err != nil {
		fmt.Fprintln(os.Stderr, "embeddingseed:", err)
		os.Exit(1)
	}
}

func run(dsn, model string, batch, limit int, query string, minScore float32, verbose bool) error {
	ctx := context.Background()

	// 与 cmd/api、cmd/statuteseed 同源加载 .env：工具与服务的连接配置只能有一套。
	if _, err := config.LoadDotEnv(".env"); err != nil {
		return fmt.Errorf("读取 .env: %w", err)
	}
	cfg, err := config.Load()
	if err != nil {
		return fmt.Errorf("读取配置: %w", err)
	}
	if dsn != "" {
		cfg.MySQL.DSN = dsn
	}
	if cfg.MySQL.DSN == "" {
		return fmt.Errorf("MYSQL_DSN 为空，无法连接数据库")
	}
	if model == "" {
		model = cfg.LLM.ModelEmbedding
	}
	if model == "" {
		return fmt.Errorf("向量模型名为空，请用 -model 指定或设置 LLM_EMBEDDING_MODEL")
	}

	db, err := store.OpenMySQL(ctx, cfg.MySQL)
	if err != nil {
		return err
	}
	defer db.Close()

	total, err := countStatutes(ctx, db)
	if err != nil {
		return err
	}
	if total == 0 {
		return fmt.Errorf("statutes 表是空的：先跑 go run ./cmd/statuteseed -import")
	}

	statutes, err := loadStatutes(ctx, db, limit)
	if err != nil {
		return err
	}
	batchSize := clampBatch(batch)
	fmt.Printf("法条 %d 条，模型 %s，批次大小 %d\n", len(statutes), model, batchSize)
	if batch != batchSize {
		slog.Warn("batch 超过上游上限，已截断", "want", batch, "got", batchSize)
	}

	client := llm.NewEmbeddingClient(cfg.LLM, llm.WithEmbeddingLogger(slog.Default()))
	log := slog.Default()
	st := store.NewStatutes(db)

	started := time.Now()
	var (
		dim     int
		batches int
		written int
	)
	for begin := 0; begin < len(statutes); begin += batchSize {
		end := begin + batchSize
		if end > len(statutes) {
			end = len(statutes)
		}
		chunk := statutes[begin:end]
		batches++

		texts := make([]string, len(chunk))
		for i, s := range chunk {
			texts[i] = embedText(s)
		}
		if verbose {
			fmt.Printf("[%d/%d] 批次 %d 起：%s\n", begin, len(statutes), batches, chunk[0].ID)
		}

		batchStart := time.Now()
		vecs, err := seedWithRetry(ctx, client, model, texts, batches, log)
		if err != nil {
			return err
		}
		if len(vecs) != len(chunk) {
			return fmt.Errorf("第 %d 批返回 %d 条向量，与提交的 %d 条不一致", batches, len(vecs), len(chunk))
		}
		if dim == 0 {
			dim = len(vecs[0])
		} else if len(vecs[0]) != dim {
			// 维度不一致只能停下：混进库后余弦点积要么越界要么静默截断，
			// 分数看起来"像那么回事"，只能等召回异常时才发现。
			return fmt.Errorf("第 %d 批维度 %d 与首批 %d 不一致，已停止（前 %d 条已写入）",
				batches, len(vecs[0]), dim, written)
		}

		items := make([]port.Embedding, len(chunk))
		for i, s := range chunk {
			items[i] = port.Embedding{
				OwnerType: ownerTypeStatute,
				OwnerID:   s.ID,
				Model:     model,
				Vector:    vecs[i],
			}
		}
		// 逐批落库，不在内存攒 205 条：中途失败时已写入的不必重跑，
		// 重跑也是幂等 upsert。
		if err := st.SaveEmbeddings(ctx, items); err != nil {
			return fmt.Errorf("第 %d 批写入失败: %w", batches, err)
		}
		written += len(items)

		if verbose || batches == 1 || end == len(statutes) {
			fmt.Printf("[%d/%d] 维度 %d，耗时 %.1fs\n", end, len(statutes), dim, time.Since(batchStart).Seconds())
		}
	}

	fmt.Printf("完成：%d 条，维度 %d，%d 批，耗时 %.1fs\n",
		written, dim, batches, time.Since(started).Seconds())
	fmt.Printf("写入 embeddings：%d 行（owner_type=%s，model=%s）\n", written, ownerTypeStatute, model)

	if query == "" {
		return nil
	}
	return selfCheck(ctx, db, client, model, query, minScore)
}

// countStatutes 先确认库里真有法条：空库跑向量化只会得到一张空表，
// 而"完成：0 条"看起来像成功。
func countStatutes(ctx context.Context, db *sql.DB) (int, error) {
	var total int
	if err := db.QueryRowContext(ctx, "SELECT COUNT(*) FROM statutes").Scan(&total); err != nil {
		return 0, fmt.Errorf("统计法条条数: %w", err)
	}
	return total, nil
}

// loadStatutes 直接读表，不走 port.StatuteStore（它没有 List，而 port 已冻结）。
func loadStatutes(ctx context.Context, db *sql.DB, limit int) ([]port.Statute, error) {
	query := "SELECT id, law, article, title, text FROM statutes ORDER BY id"
	args := []any{}
	if limit > 0 {
		query += " LIMIT ?"
		args = append(args, limit)
	}
	rows, err := db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("读取法条: %w", err)
	}
	defer rows.Close()

	items := make([]port.Statute, 0, 256)
	for rows.Next() {
		var s port.Statute
		if err := rows.Scan(&s.ID, &s.Law, &s.Article, &s.Title, &s.Text); err != nil {
			return nil, fmt.Errorf("扫描法条: %w", err)
		}
		items = append(items, s)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("遍历法条: %w", err)
	}
	return items, nil
}

// embedText 拼嵌入文本。
//
// 实现搬到了 retrieval.StatuteDocument：重排时要喂给交叉编码器的文本必须与
// 这里建向量时喂给嵌入模型的文本逐字一致，否则"索引里的相似度"和"重排读到的
// 内容"是两个东西，精度对不上时无从排查。留着这一层薄封装是因为调用点与测试
// 都按这个名字找，改名的收益为零。
func embedText(s port.Statute) string {
	return retrieval.StatuteDocument(s)
}

// clampBatch 把批次大小夹到 1..10：上游单请求最多 10 条，多了就是静默丢批。
func clampBatch(n int) int {
	if n <= 0 || n > 10 {
		return 10
	}
	return n
}

// seedWithRetry 调一次向量化，失败按 1s/2s/4s 退避重试，只重试可重试的码。
func seedWithRetry(ctx context.Context, client *llm.EmbeddingClient, model string,
	texts []string, batchNo int, log *slog.Logger) ([][]float32, error) {
	var lastErr error
	for attempt := 1; attempt <= seedRetries; attempt++ {
		vecs, err := client.Embed(ctx, model, texts)
		if err == nil {
			return vecs, nil
		}
		lastErr = err
		if !retryableCode(port.CodeOf(err)) {
			return nil, fmt.Errorf("第 %d 批向量化失败（不可重试）: %w", batchNo, err)
		}
		if attempt == seedRetries {
			break
		}
		backoff := time.Duration(1<<(attempt-1)) * time.Second
		log.Warn("向量化批次失败，退避后重试",
			"batch", batchNo, "attempt", attempt, "code", port.CodeOf(err), "sleep", backoff.String())
		select {
		case <-ctx.Done():
			return nil, fmt.Errorf("第 %d 批向量化被取消: %w", batchNo, ctx.Err())
		case <-time.After(backoff):
		}
	}
	return nil, fmt.Errorf("第 %d 批向量化失败，已重试 %d 次: %w", batchNo, seedRetries, lastErr)
}

// retryableCode 只认限流与上游故障：参数错误重试一百次也是同一个错。
func retryableCode(code port.ErrorCode) bool {
	switch code {
	case port.CodeRateLimited, port.CodeUpstream, port.CodeUpstreamTimeout:
		return true
	default:
		return false
	}
}

// selfCheck 把库里已有的向量载入内存索引，对每条查询打 top5。
func selfCheck(ctx context.Context, db *sql.DB, client *llm.EmbeddingClient,
	model, query string, minScore float32) error {
	st := store.NewStatutes(db)
	items, err := st.LoadEmbeddings(ctx, ownerTypeStatute, model)
	if err != nil {
		return fmt.Errorf("读取向量: %w", err)
	}
	index, err := retrieval.NewSemantic(items)
	if err != nil {
		return fmt.Errorf("建索引: %w", err)
	}

	queries := strings.Split(query, "|")
	for _, raw := range queries {
		q := strings.TrimSpace(raw)
		if q == "" {
			continue
		}
		vecs, err := seedWithRetry(ctx, client, model, []string{q}, 0, slog.Default())
		if err != nil {
			return err
		}
		hits, err := index.Search(vecs[0], 5, minScore)
		if err != nil {
			return fmt.Errorf("检索 %q: %w", q, err)
		}
		fmt.Printf("检索 %q 命中 %d 条：\n", q, len(hits))
		for _, h := range hits {
			s, err := st.Get(ctx, h.Statute.ID)
			if err != nil {
				fmt.Printf("  %-8s %.4f（回表失败: %v）\n", h.Statute.ID, h.Score, err)
				continue
			}
			fmt.Printf("  %-8s %.4f %s %s\n", s.ID, h.Score, s.Article, s.Title)
		}
	}
	return nil
}
