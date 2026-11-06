package store

import (
	"strings"
	"testing"
)

func TestLoadMigrations(t *testing.T) {
	ms, err := loadMigrations()
	if err != nil {
		t.Fatalf("loadMigrations() 返回错误: %v", err)
	}
	if len(ms) == 0 {
		t.Fatal("没有任何迁移")
	}

	for i, m := range ms {
		if m.version <= 0 {
			t.Fatalf("第 %d 条迁移版本号非法: %d", i, m.version)
		}
		if i > 0 && ms[i-1].version >= m.version {
			t.Fatalf("迁移未按版本升序: %d 在 %d 之前", ms[i-1].version, ms[i].version)
		}
		if strings.TrimSpace(m.upSQL) == "" {
			t.Fatalf("迁移 %04d_%s 的 up 脚本为空", m.version, m.name)
		}
		if !strings.Contains(m.upSQL, "CREATE TABLE") {
			t.Fatalf("迁移 %04d_%s 看起来不是建表脚本", m.version, m.name)
		}
	}
}

func TestParseMigrationName(t *testing.T) {
	tests := []struct {
		file      string
		wantVer   int
		wantName  string
		wantDir   string
		wantError bool
	}{
		{file: "0001_init.up.sql", wantVer: 1, wantName: "init", wantDir: "up"},
		{file: "0012_add_cache.down.sql", wantVer: 12, wantName: "add_cache", wantDir: "down"},
		{file: "1_init.up.sql", wantVer: 1, wantName: "init", wantDir: "up"},
		{file: "0001_init.sideways.sql", wantError: true},
		{file: "init.up.sql", wantError: true},
		{file: "0000_init.up.sql", wantError: true},
		{file: "0001.up.sql", wantError: true},
		{file: "0001_init.up.txt", wantError: true},
	}

	for _, tc := range tests {
		t.Run(tc.file, func(t *testing.T) {
			ver, name, dir, err := parseMigrationName(tc.file)

			if tc.wantError {
				if err == nil {
					t.Fatalf("期望报错，实际通过: ver=%d name=%q dir=%q", ver, name, dir)
				}
				return
			}
			if err != nil {
				t.Fatalf("parseMigrationName(%q) 返回错误: %v", tc.file, err)
			}
			if ver != tc.wantVer || name != tc.wantName || dir != tc.wantDir {
				t.Fatalf("got (%d, %q, %q), want (%d, %q, %q)",
					ver, name, dir, tc.wantVer, tc.wantName, tc.wantDir)
			}
		})
	}
}

func TestSplitStatements(t *testing.T) {
	tests := []struct {
		name string
		src  string
		want []string
	}{
		{
			name: "跳过注释与空行",
			src:  "-- 注释\n\nCREATE TABLE a (id INT);\n-- 又一行注释\nCREATE TABLE b (id INT);\n",
			want: []string{"CREATE TABLE a (id INT)", "CREATE TABLE b (id INT)"},
		},
		{
			name: "尾部分号不产生空语句",
			src:  "SELECT 1;",
			want: []string{"SELECT 1"},
		},
		{
			name: "空文件返回空切片",
			src:  "\n-- 只有注释\n",
			want: nil,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := splitStatements(tc.src)
			if len(got) != len(tc.want) {
				t.Fatalf("语句数 = %d, want %d（got=%v）", len(got), len(tc.want), got)
			}
			for i := range got {
				if got[i] != tc.want[i] {
					t.Fatalf("第 %d 条 = %q, want %q", i, got[i], tc.want[i])
				}
			}
		})
	}
}
