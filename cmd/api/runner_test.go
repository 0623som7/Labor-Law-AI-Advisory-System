package main

import (
	"io"
	"log/slog"
	"testing"

	"laborlaw/internal/agent"
	"laborlaw/internal/config"
)

// 装配降级的风险不是编译不过，而是"演示时以为接了真模型，实际还是桩"。
// 所以把两条分支都钉死。
func TestNewRunnerFallsBackToStubWithoutKey(t *testing.T) {
	cfg := config.Config{}
	log := slog.New(slog.NewTextHandler(io.Discard, nil))

	if _, ok := newRunner(cfg, log, nil, nil).(*stubRunner); !ok {
		t.Error("API key 为空时必须退回演示桩")
	}
}

func TestNewRunnerUsesAgentWithKey(t *testing.T) {
	cfg := config.Config{}
	cfg.LLM.APIKey = "test-key"
	cfg.LLM.BaseURL = "http://127.0.0.1:0"
	log := slog.New(slog.NewTextHandler(io.Discard, nil))

	if _, ok := newRunner(cfg, log, nil, nil).(*agent.Runner); !ok {
		t.Error("配置了 API key 后必须启用真实编排")
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
