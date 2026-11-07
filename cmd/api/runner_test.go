package main

import (
	"io"
	"log/slog"
	"testing"

	"laborlaw/internal/agent"
	"laborlaw/internal/config"
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
	if got := newSearcher(nil); got != nil {
		t.Error("没有数据库连接时不应构造检索器")
	}
}

// 历史读取同理：没有库就该返回 nil，让 Runner 走无状态问答。
func TestNewHistoryWithoutDB(t *testing.T) {
	if got := newHistory(nil); got != nil {
		t.Error("没有数据库连接时不应构造历史读取器")
	}
}
