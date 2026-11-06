package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func writeEnvFile(t *testing.T, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), ".env")
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatalf("写临时 .env 失败: %v", err)
	}
	return path
}

func TestLoadDotEnv(t *testing.T) {
	tests := []struct {
		name    string
		body    string
		want    map[string]string
		wantErr string
	}{
		{
			name: "普通键值与注释",
			body: "# 注释\nHTTP_ADDR=:9090\n\nLOG_LEVEL = info \n",
			want: map[string]string{"HTTP_ADDR": ":9090", "LOG_LEVEL": "info"},
		},
		{
			name: "带引号的值允许空格",
			body: "APP_NAME=\"labor law api\"\n",
			want: map[string]string{"APP_NAME": "labor law api"},
		},
		{
			name: "export 前缀被剥掉",
			body: "export REDIS_ADDR=127.0.0.1:6380\n",
			want: map[string]string{"REDIS_ADDR": "127.0.0.1:6380"},
		},
		{
			name:    "缺等号直接报错",
			body:    "BROKEN_LINE\n",
			wantErr: "缺少 '='",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			path := writeEnvFile(t, tc.body)

			// 用例之间互不影响：只断言本用例新引入的键。
			for k := range tc.want {
				t.Cleanup(func() { os.Unsetenv(k) })
			}

			n, err := LoadDotEnv(path)
			if tc.wantErr != "" {
				if err == nil {
					t.Fatalf("期望报错 %q，实际 nil", tc.wantErr)
				}
				if !strings.Contains(err.Error(), tc.wantErr) {
					t.Fatalf("错误 = %q，期望包含 %q", err.Error(), tc.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("LoadDotEnv() 返回错误: %v", err)
			}
			if n != len(tc.want) {
				t.Fatalf("写入变量数 = %d, want %d", n, len(tc.want))
			}
			for k, want := range tc.want {
				if got := os.Getenv(k); got != want {
					t.Fatalf("%s = %q, want %q", k, got, want)
				}
			}
		})
	}
}

func TestLoadDotEnvDoesNotOverride(t *testing.T) {
	const key = "DSH_TEST_OVERRIDE"
	t.Setenv(key, "from-process")
	t.Cleanup(func() { os.Unsetenv(key) })

	path := writeEnvFile(t, key+"=from-file\n")
	if _, err := LoadDotEnv(path); err != nil {
		t.Fatalf("LoadDotEnv() 返回错误: %v", err)
	}

	if got := os.Getenv(key); got != "from-process" {
		t.Fatalf("%s = %q, want from-process（已存在的环境变量不许被文件覆盖）", key, got)
	}
}

func TestLoadDotEnvMissingFileIsOK(t *testing.T) {
	path := filepath.Join(t.TempDir(), "no-such.env")

	n, err := LoadDotEnv(path)
	if err != nil {
		t.Fatalf("缺文件不该报错，实际: %v", err)
	}
	if n != 0 {
		t.Fatalf("写入变量数 = %d, want 0", n)
	}
}
