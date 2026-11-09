package main

import (
	"context"
	"database/sql"
	"io"
	"log/slog"
	"testing"

	_ "github.com/go-sql-driver/mysql"

	"laborlaw/internal/agent"
	"laborlaw/internal/config"
	"laborlaw/internal/retrieval"
)

func testLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// 装配降级的风险不是编译不过，而是"演示时以为接了真模型，实际还是桩"。
// 所以两条分支都钉死。
func TestNewRunnerWithoutAPIKey(t *testing.T) {
	// 没有密钥时调用方不构造客户端（见 main.go），这里传 nil。
	// 断言的是"不 panic 且明确退回桩"，而不是某个字段。
	if _, ok := newRunner(testLogger(), nil, nil, nil, 8000).(*stubRunner); !ok {
		t.Error("API key 为空时必须退回演示桩")
	}
}

func TestNewRunnerUsesAgentWithKey(t *testing.T) {
	cfg := config.Config{}
	cfg.LLM.APIKey = "test-key"
	cfg.LLM.BaseURL = "http://127.0.0.1:0"

	client := newLLM(context.Background(), cfg, testLogger(), nil, nil)
	if _, ok := newRunner(testLogger(), client, nil, nil, 8000).(*agent.Runner); !ok {
		t.Error("配置了 API key 后必须启用真实编排")
	}
}

// 上下文预算只在 Runner 内部生效，外面看不到；这里钉住的是"传进去不炸、
// 传 0 也不炸"以及构造点没有把 Config 漏进 agent 包。
func TestNewRunnerPassesContextTokens(t *testing.T) {
	cfg := config.Config{}
	cfg.LLM.APIKey = "test-key"
	cfg.LLM.BaseURL = "http://127.0.0.1:0"
	client := newLLM(context.Background(), cfg, testLogger(), nil, nil)

	for _, n := range []int{0, 8000, 64000} {
		if _, ok := newRunner(testLogger(), client, nil, nil, n).(*agent.Runner); !ok {
			t.Fatalf("ctxTokens=%d 时应当仍是真实编排", n)
		}
	}
}

// 全进程只能有一个客户端：QPS 与日预算闸都长在它身上。
// 用例直接钉住"两次调用是不同的实例、同一个构造点"这件事没有意义，
// 真正要守的是 newLLM 不校验密钥（否则上面的桩分支根本走不到）。
func TestNewLLMDoesNotRequireKey(t *testing.T) {
	client := newLLM(context.Background(), config.Config{}, testLogger(), nil, nil)
	if client == nil {
		t.Fatal("密钥为空时 newLLM 也必须能安全构造")
	}
}

// 没有 Redis 时预算只能留在进程内。要求：构造不 panic、预热不报错、
// 也不构造一个"每次调用都失败"的半残 store（那会让每条请求刷一条告警）。
func TestNewLLMWithoutRedisBudget(t *testing.T) {
	if s := newBudgetStore(nil, testLogger()); s != nil {
		t.Fatal("没有 Redis 时 newBudgetStore 必须返回 nil")
	}

	cfg := config.Config{}
	cfg.LLM.APIKey = "test-key"
	cfg.LLM.BaseURL = "http://127.0.0.1:0"
	cfg.LLM.DailyBudgetYuan = 20

	client := newLLM(context.Background(), cfg, testLogger(), nil, nil)
	if client == nil {
		t.Fatal("没有 Redis 也必须能构造客户端")
	}
	if err := client.WarmBudget(context.Background()); err != nil {
		t.Fatalf("没有 store 时 WarmBudget 应当返回 nil，实际 %v", err)
	}
	if spent, limit := client.Budget(); spent != 0 || limit != 20 {
		t.Fatalf("Budget() = (%v, %v), want (0, 20)", spent, limit)
	}
}

// 无 MySQL 时必须返回 nil 检索器，让 Runner 走"跳过检索"的降级路径。
func TestNewSearcherWithoutDB(t *testing.T) {
	on := config.Config{}
	on.Retrieval.Semantic = true
	for _, cfg := range []config.Config{{}, on} {
		if got := newSearcher(context.Background(), nil, nil, cfg, testLogger()); got != nil {
			t.Errorf("没有数据库连接时不应构造检索器（semantic=%v）", cfg.Retrieval.Semantic)
		}
	}
}

// 历史读取同理：没有库就该返回 nil，让 Runner 走无状态问答。
func TestNewHistoryWithoutDB(t *testing.T) {
	if got := newHistory(nil); got != nil {
		t.Error("没有数据库连接时不应构造历史读取器")
	}
}

// 灰度关闭时必须与引入向量之前完全一致：同一个 *retrieval.Retriever，
// 不是"包了一层但行为相同"。类型相同才有资格说零回归。
func TestNewSearcherKeywordOnlyWhenSemanticOff(t *testing.T) {
	db := openDeadDB(t)
	got := newSearcher(context.Background(), db, nil, config.Config{}, testLogger())
	if _, ok := got.(*retrieval.Retriever); !ok {
		t.Fatalf("开关关闭时应返回 *retrieval.Retriever，实际 %T", got)
	}
}

// 开关打开但索引建不起来（库连不上/向量没灌）时必须降级，而不是返回坏检索器，
// 更不能让启动失败：向量召回是增强项，不是启动前提。
func TestNewSearcherDegradesWhenIndexFails(t *testing.T) {
	db := openDeadDB(t)
	cfg := config.Config{}
	cfg.Retrieval.Semantic = true
	cfg.LLM.APIKey = "test-key"
	cfg.LLM.BaseURL = "http://127.0.0.1:0"
	cfg.LLM.ModelEmbedding = "text-embedding-v3"

	got := newSearcher(context.Background(), db, nil, cfg, testLogger())
	if _, ok := got.(*retrieval.Retriever); !ok {
		t.Fatalf("索引建不起来时应退回关键词检索，实际 %T", got)
	}
}

// openDeadDB 拿一个不连也不 ping 的连接池：sql.Open 只校验 DSN，
// 真正发查询才会失败，正好用来测"库不可用"这条降级路径。
func openDeadDB(t *testing.T) *sql.DB {
	t.Helper()
	db, err := sql.Open("mysql", "root@tcp(127.0.0.1:1)/laborlaw")
	if err != nil {
		t.Fatalf("sql.Open 失败：%v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return db
}

// TestEmbedScope 钉住缓存键的命名空间：带 host，但去掉路径。
//
// 路径（/compatible-mode/v1 之类）只是同一家服务商的不同入口，
// 跟着它变会让整库缓存白扔一次；host 变了才是真的换了服务商。
func TestEmbedScope(t *testing.T) {
	cases := []struct {
		base string
		want string
	}{
		{"https://dashscope.aliyuncs.com/compatible-mode/v1", "text-embedding-v3@dashscope.aliyuncs.com"},
		{"https://dashscope.aliyuncs.com", "text-embedding-v3@dashscope.aliyuncs.com"},
		{"http://127.0.0.1:9999/v1", "text-embedding-v3@127.0.0.1:9999"},
		{"", "text-embedding-v3@"},
		{"::", "text-embedding-v3@::"},
	}
	for _, c := range cases {
		got := embedScope(config.LLMConfig{BaseURL: c.base, ModelEmbedding: "text-embedding-v3"})
		if got != c.want {
			t.Errorf("embedScope(%q) = %q, want %q", c.base, got, c.want)
		}
	}
}
