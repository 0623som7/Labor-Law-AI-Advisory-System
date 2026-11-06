package config

import (
	"strings"
	"testing"
	"time"
)

// baseEnv 返回一份能通过校验的最小环境。
//
// 每个用例只改自己关心的那一项：这样断言失败时能确定是被改的那项引起的，
// 而不是"某个默认值碰巧也错了"。
func baseEnv() map[string]string {
	return map[string]string{
		"APP_ENV":    "dev",
		"JWT_SECRET": strings.Repeat("k", minSecretLen),
		"MYSQL_DSN":  "root:p@tcp(127.0.0.1:3306)/laborlaw",
		"REDIS_ADDR": "127.0.0.1:6379",
	}
}

func envGetter(env map[string]string) func(string) string {
	return func(key string) string { return env[key] }
}

func TestLoadFromDefaults(t *testing.T) {
	cfg, err := LoadFrom(envGetter(baseEnv()))
	if err != nil {
		t.Fatalf("LoadFrom() 返回错误: %v", err)
	}

	cases := []struct {
		name string
		got  any
		want any
	}{
		{"HTTP.Addr", cfg.HTTP.Addr, ":8080"},
		{"HTTP.WriteTimeout", cfg.HTTP.WriteTimeout, time.Duration(0)},
		{"HTTP.ReadHeaderTimeout", cfg.HTTP.ReadHeaderTimeout, 5 * time.Second},
		{"Auth.Issuer", cfg.Auth.Issuer, "laborlaw"},
		{"Auth.AccessTTL", cfg.Auth.AccessTTL, 2 * time.Hour},
		{"Worker.PoolSize", cfg.Worker.PoolSize, 2},
		{"Storage.MaxUploadMB", cfg.Storage.MaxUploadMB, 5},
		{"Storage.MaxPages", cfg.Storage.MaxPages, 50},
		{"File.MinConfidence", cfg.File.MinConfidence, 0.6},
		{"LLM.MaxRetries", cfg.LLM.MaxRetries, 2},
		{"Log.Level", cfg.Log.Level, "info"},
		{"App.Env", cfg.App.Env, EnvDev},
	}
	for _, tc := range cases {
		if tc.got != tc.want {
			t.Errorf("%s = %v, want %v", tc.name, tc.got, tc.want)
		}
	}
}

func TestLoadFromOverrides(t *testing.T) {
	env := baseEnv()
	env["APP_ENV"] = "staging"
	env["HTTP_ADDR"] = ":9090"
	env["HTTP_WRITE_TIMEOUT"] = "0s"
	env["JWT_ACCESS_TTL"] = "30m"
	env["WORKER_POOL_SIZE"] = "1"
	env["LOG_LEVEL"] = "DEBUG"
	env["LOG_FORMAT"] = "json"

	cfg, err := LoadFrom(envGetter(env))
	if err != nil {
		t.Fatalf("LoadFrom() 返回错误: %v", err)
	}
	if cfg.App.Env != EnvStaging {
		t.Errorf("App.Env = %q, want %q", cfg.App.Env, EnvStaging)
	}
	if cfg.HTTP.Addr != ":9090" {
		t.Errorf("HTTP.Addr = %q, want :9090", cfg.HTTP.Addr)
	}
	if cfg.Auth.AccessTTL != 30*time.Minute {
		t.Errorf("Auth.AccessTTL = %v, want 30m", cfg.Auth.AccessTTL)
	}
	if cfg.Worker.PoolSize != 1 {
		t.Errorf("Worker.PoolSize = %d, want 1", cfg.Worker.PoolSize)
	}
	// 日志级别统一转小写，避免 "DEBUG" 这种写法让 switch 落到 default。
	if cfg.Log.Level != "debug" {
		t.Errorf("Log.Level = %q, want debug", cfg.Log.Level)
	}
}

func TestLoadFromErrors(t *testing.T) {
	tests := []struct {
		name    string
		mutate  func(map[string]string)
		wantErr string
	}{
		{
			name:    "缺少 JWT_SECRET",
			mutate:  func(e map[string]string) { delete(e, "JWT_SECRET") },
			wantErr: "JWT_SECRET 必填",
		},
		{
			name:    "缺少 MYSQL_DSN",
			mutate:  func(e map[string]string) { delete(e, "MYSQL_DSN") },
			wantErr: "MYSQL_DSN 必填",
		},
		{
			name:    "缺少 REDIS_ADDR",
			mutate:  func(e map[string]string) { delete(e, "REDIS_ADDR") },
			wantErr: "REDIS_ADDR 必填",
		},
		{
			name:    "密钥过短",
			mutate:  func(e map[string]string) { e["JWT_SECRET"] = "short" },
			wantErr: "JWT_SECRET 长度至少 32 字节",
		},
		{
			name:    "时长非法",
			mutate:  func(e map[string]string) { e["HTTP_READ_TIMEOUT"] = "abc" },
			wantErr: "HTTP_READ_TIMEOUT 不是合法的正时长",
		},
		{
			name:    "时长为零",
			mutate:  func(e map[string]string) { e["HTTP_IDLE_TIMEOUT"] = "0s" },
			wantErr: "HTTP_IDLE_TIMEOUT 不是合法的正时长",
		},
		{
			name:    "整数非法",
			mutate:  func(e map[string]string) { e["WORKER_POOL_SIZE"] = "two" },
			wantErr: "WORKER_POOL_SIZE 必须是整数",
		},
		{
			name:    "池大小越界",
			mutate:  func(e map[string]string) { e["WORKER_POOL_SIZE"] = "0" },
			wantErr: "WORKER_POOL_SIZE 必须在 1..64 之间",
		},
		{
			name:    "QPS 越界",
			mutate:  func(e map[string]string) { e["LLM_QPS"] = "0" },
			wantErr: "LLM_QPS 必须在",
		},
		{
			name:    "环境非法",
			mutate:  func(e map[string]string) { e["APP_ENV"] = "prod" },
			wantErr: "APP_ENV 必须是 dev / staging / production 之一",
		},
		{
			name: "生产禁止调试用户",
			mutate: func(e map[string]string) {
				e["APP_ENV"] = "production"
				e["LOG_FORMAT"] = "json"
				e["AUTH_DEV_USER"] = "u1"
			},
			wantErr: "生产环境禁止 AUTH_DEV_USER",
		},
		{
			name: "生产要求 json 日志",
			mutate: func(e map[string]string) {
				e["APP_ENV"] = "production"
				e["LOG_FORMAT"] = "text"
			},
			wantErr: "生产环境 LOG_FORMAT 必须是 json",
		},
		{
			name: "生产禁止自动迁移",
			mutate: func(e map[string]string) {
				e["APP_ENV"] = "production"
				e["LOG_FORMAT"] = "json"
				e["AUTO_MIGRATE"] = "true"
			},
			wantErr: "生产环境禁止 AUTO_MIGRATE",
		},
		{
			name:    "布尔值非法",
			mutate:  func(e map[string]string) { e["AUTO_MIGRATE"] = "yes" },
			wantErr: "AUTO_MIGRATE 必须是 true / false",
		},
		{
			name:    "access 不短于 refresh",
			mutate:  func(e map[string]string) { e["JWT_REFRESH_TTL"] = "1h" },
			wantErr: "JWT_ACCESS_TTL 必须小于 JWT_REFRESH_TTL",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			env := baseEnv()
			tc.mutate(env)

			_, err := LoadFrom(envGetter(env))
			if err == nil {
				t.Fatalf("LoadFrom() 期望报错，实际返回 nil")
			}
			if !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("错误信息 = %q，期望包含 %q", err.Error(), tc.wantErr)
			}
		})
	}
}

// TestErrorsDoNotEchoSecrets 守住"报错只报变量名"这条底线。
//
// 一旦有人为了好排查把原始值拼进错误，DSN 里的密码就会顺着日志落盘。
func TestErrorsDoNotEchoSecrets(t *testing.T) {
	env := baseEnv()
	env["JWT_SECRET"] = "leak-me-please"

	_, err := LoadFrom(envGetter(env))
	if err == nil {
		t.Fatal("期望 JWT_SECRET 过短报错，实际返回 nil")
	}
	if strings.Contains(err.Error(), "leak-me-please") {
		t.Fatalf("错误信息泄漏了密钥原文: %q", err.Error())
	}
}

func TestValidateCrossField(t *testing.T) {
	valid := func() Config {
		return Config{
			App:     AppConfig{Env: EnvDev},
			HTTP:    HTTPConfig{MaxBodyBytes: 12 << 20},
			Auth:    AuthConfig{Secret: strings.Repeat("k", 32), AccessTTL: time.Hour, RefreshTTL: 24 * time.Hour},
			MySQL:   MySQLConfig{MaxOpenConns: 10, MaxIdleConns: 5},
			Storage: StorageConfig{MaxUploadMB: 5},
			File:    FileConfig{MinConfidence: 0.6},
			Log:     LogConfig{Level: "info", Format: "text"},
		}
	}

	tests := []struct {
		name    string
		mutate  func(*Config)
		wantErr string
	}{
		{"基线合法", func(c *Config) {}, ""},
		{"idle 大于 open", func(c *Config) { c.MySQL.MaxIdleConns = 20 }, "MYSQL_MAX_IDLE_CONNS 不能大于"},
		{"请求体上限小于上传上限", func(c *Config) { c.HTTP.MaxBodyBytes = 1 << 20 }, "HTTP_MAX_BODY_BYTES 必须不小于"},
		{"置信度越界", func(c *Config) { c.File.MinConfidence = 1.5 }, "FILE_MIN_CONFIDENCE"},
		{"日志级别非法", func(c *Config) { c.Log.Level = "trace" }, "LOG_LEVEL 必须是"},
		{"日志格式非法", func(c *Config) { c.Log.Format = "xml" }, "LOG_FORMAT 必须是"},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			cfg := valid()
			tc.mutate(&cfg)

			err := cfg.Validate()
			if tc.wantErr == "" {
				if err != nil {
					t.Fatalf("Validate() 期望通过，实际: %v", err)
				}
				return
			}
			if err == nil {
				t.Fatalf("Validate() 期望报错 %q，实际 nil", tc.wantErr)
			}
			if !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("错误信息 = %q，期望包含 %q", err.Error(), tc.wantErr)
			}
		})
	}
}
