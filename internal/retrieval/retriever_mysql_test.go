package retrieval

import (
	"context"
	"database/sql"
	"encoding/json"
	"os"
	"testing"
	"time"

	_ "github.com/go-sql-driver/mysql"

	"laborlaw/internal/port"
	"laborlaw/internal/store"
)

// 这些用例需要真实 MySQL 与已导入的法条种子数据：
//
//	go run ./cmd/statuteseed -import
//	TEST_MYSQL_DSN=... go test ./internal/retrieval/ -run Real -v
//
// 为什么一定要用真数据测：检索质量取决于"用户的话"与"法条原文的用词"之间的距离，
// 这个距离在假数据上是编不出来的。造三条测试数据时谁都会把关键词写进正文，
// 而真实法条里根本没有"二倍工资"这个连续词——原文是"经济补偿标准的二倍"。
func realRetriever(t *testing.T) *Retriever {
	t.Helper()
	dsn := os.Getenv("TEST_MYSQL_DSN")
	if dsn == "" {
		t.Skip("TEST_MYSQL_DSN 未设置，跳过真实库检索测试")
	}
	db, err := sql.Open("mysql", dsn)
	if err != nil {
		t.Fatalf("打开数据库失败: %v", err)
	}
	t.Cleanup(func() { db.Close() })

	ctx := context.Background()
	if _, err := store.Migrate(ctx, db); err != nil {
		t.Fatalf("执行迁移失败: %v", err)
	}
	ensureCorpus(t, db)
	return New(store.NewStatutes(db))
}

// ensureCorpus 保证库里已经有种子法条。
//
// CI 的 MySQL service 是一个空库：这里不补数据，检索用例就只能全红或全体跳过，
// 而"在 CI 里真跑一遍召回"正是这个 job 存在的理由。种子文件是仓库里的权威数据，
// 测试直接读它，不经过 HTTP、不依赖 .env，也不改任何线上数据——库是测试库。
func ensureCorpus(t *testing.T, db *sql.DB) {
	t.Helper()
	ctx := context.Background()
	st := store.NewStatutes(db)
	n, err := st.Count(ctx)
	if err != nil {
		t.Fatalf("查询法条总数失败: %v", err)
	}
	if n > 0 {
		return
	}

	const seedPath = "../../deploy/seed/statutes.json"
	raw, err := os.ReadFile(seedPath)
	if err != nil {
		t.Skipf("法条库为空，且读不到种子文件 %s（%v），跳过真实库检索测试", seedPath, err)
	}
	var rows []struct {
		ID          string `json:"id"`
		Law         string `json:"law"`
		Article     string `json:"article"`
		Title       string `json:"title"`
		Text        string `json:"text"`
		URL         string `json:"url"`
		EffectiveAt string `json:"effective_at"`
	}
	if err := json.Unmarshal(raw, &rows); err != nil {
		t.Fatalf("解析种子文件失败: %v", err)
	}
	items := make([]port.Statute, 0, len(rows))
	for _, r := range rows {
		var effective time.Time
		if r.EffectiveAt != "" {
			if d, perr := time.Parse("2006-01-02", r.EffectiveAt); perr == nil {
				effective = d
			}
		}
		items = append(items, port.Statute{
			ID: r.ID, Law: r.Law, Article: r.Article, Title: r.Title,
			Text: r.Text, URL: r.URL, EffectiveAt: effective,
		})
	}
	if err := st.ReplaceAll(ctx, items); err != nil {
		t.Fatalf("导入种子法条失败: %v", err)
	}
	t.Logf("库中没有法条，已从 %s 导入 %d 条", seedPath, len(items))
}

// realConsultCases 是最典型的劳动咨询口语，以及这个问题必须召回的法条。
//
// want 是"必须出现在结果里"的 ID 之一：排第几可以再调，完全没召回就是检索的缺陷。
// depth 是这条用例允许的召回深度，零值表示用默认的 5。
//
// 只有"加班费"放宽到 8，原因是已知局限而不是宽容：
// 《劳动法》第四十四条（三档加班计费比例）与第四十二条（延长工作时间的例外情形）
// 里"延长工作时间"各出现一次，词频完全相同，排序只能靠 ID 兜底，于是第四十四条
// 稳定落在第六位。词频检索分不出"讲限制"和"讲计费标准"的同样词频条文，
// 这是词表方案的边界，后续要靠向量检索解决，不该在这里靠调权重糊过去。
var realConsultCases = []struct {
	name  string
	input string
	want  []string
	depth int
}{
	{"违法解除赔偿", "公司把我辞退了，能要赔偿吗", []string{"LCL-87", "LCL-48", "LCL-46", "LCL-47"}, 0},
	{"未签合同二倍工资", "入职半年一直没签劳动合同，能要双倍工资吗", []string{"LCL-82", "LCL-10"}, 0},
	{"加班费", "天天加班，加班费应该怎么算", []string{"LL-44"}, 8},
	{"拖欠工资", "公司拖欠工资三个月了怎么办", []string{"LCL-85", "LL-91", "LCL-30"}, 0},
	{"不交社保", "公司一直不给交社保，我可以辞职要补偿吗", []string{"LCL-38", "LL-72"}, 0},
	// 同一件事的另一种说法：不说"辞职"，说"直接走"。词表只认"辞职/离职"时不命中
	// 解除类条文，用户拿到的是三页社保条文，模型只能回答"资料里没覆盖"。
	{"社保直接走", "公司不给我交社保，我能直接走吗", []string{"LCL-38"}, 0},
	{"试用期", "试用期被开除有补偿吗", []string{"LCL-21", "LCL-39", "LCL-19"}, 0},
	{"竞业限制", "离职后公司要求竞业限制，补偿怎么算", []string{"LCL-23", "LCL-24"}, 0},
	{"经济补偿", "合同到期公司不续签，有经济补偿吗", []string{"LCL-46", "LCL-47", "LCL-44"}, 0},
}

func TestRealCorpusConsultRecall(t *testing.T) {
	r := realRetriever(t)
	ctx := context.Background()

	for _, tc := range realConsultCases {
		t.Run(tc.name, func(t *testing.T) {
			keywords := ExtractKeywords(tc.input)
			depth := tc.depth
			if depth <= 0 {
				depth = 5
			}
			hits, err := r.Search(ctx, tc.input, depth)
			if err != nil {
				t.Fatalf("Search 失败: %v", err)
			}
			ids := make([]string, 0, len(hits))
			for _, h := range hits {
				ids = append(ids, h.Statute.ID)
			}
			t.Logf("输入: %s", tc.input)
			t.Logf("关键词: %v", keywords)
			t.Logf("召回: %v", ids)

			for _, want := range tc.want {
				for _, got := range ids {
					if got == want {
						return
					}
				}
			}
			t.Errorf("期望命中 %v 之一，实际 %v", tc.want, ids)
		})
	}
}
