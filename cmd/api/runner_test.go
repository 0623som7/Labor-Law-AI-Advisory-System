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
	if _, ok := newRunner(testLogger(), nil, nil, nil).(*stubRunner); !ok {
		t.Error("API key 为空时必须退回演示桩")
	}
}

func TestNewRunnerUsesAgentWithKey(t *testing.T) {
	cfg := config.Config{}
	cfg.LLM.APIKey = "test-key"
	cfg.LLM.BaseURL = "http://127.0.0.1:0"

	client := newLLM(cfg, testLogger(), nil)
	if _, ok := newRunner(testLogger(), client, nil, nil).(*agent.Runner); !ok {
		t.Error("配置了 API key 后必须启用真实编排")
	}
}

// 全进程只能有一个客户端：QPS 与日预算闸都长在它身上。
// 用例直接钉住"两次调用是不同的实例、同一个构造点"这件事没有意义，
// 真正要守的是 newLLM 不校验密钥（否则上面的桩分支根本走不到）。
func TestNewLLMDoesNotRequireKey(t *testing.T) {
	client := newLLM(config.Config{}, testLogger(), nil)
	if client == nil {
		t.Fatal("密钥为空时 newLLM 也必须能安全构造")
	}
}

// 无 MySQL 时必须返回 nil 检索器，让 Runner 走"跳过检索"的降级路径。
func TestNewSearcherWithoutDB(t *testing.T) {
	on := config.Config{}
	on.Retrieval.Semantic = true
	for _, cfg := range []config.Config{{}, on} {
		if got := newSearcher(context.Background(), nil, cfg, testLogger()); got != nil {
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
	got := newSearcher(context.Background(), db, config.Config{}, testLogger())
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

	got := newSearcher(context.Background(), db, cfg, testLogger())
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
