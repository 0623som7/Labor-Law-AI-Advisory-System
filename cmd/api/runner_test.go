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

	if _, ok := newRunner(cfg, log).(*stubRunner); !ok {
		t.Error("API key 为空时必须退回演示桩")
	}
}

func TestNewRunnerUsesAgentWithKey(t *testing.T) {
	cfg := config.Config{}
	cfg.LLM.APIKey = "test-key"
	cfg.LLM.BaseURL = "http://127.0.0.1:0"
	log := slog.New(slog.NewTextHandler(io.Discard, nil))

	if _, ok := newRunner(cfg, log).(*agent.Runner); !ok {
		t.Error("配置了 API key 后必须启用真实编排")
	}
}
