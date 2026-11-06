// Package store 是持久层：连接、迁移与各域 DAL 的实现。
//
// 它不 import 任何 HTTP 或 Agent 相关的包：port.Store 的实现在这里，
// B 通过接口拿到它，A 通过这里写 SQL。方向永远是 store → port，反过来就是循环依赖。
package store

import (
	"context"
	"database/sql"
	"embed"
	"fmt"
	"io/fs"
	"sort"
	"strconv"
	"strings"
)

//go:embed migrations/*.sql
var migrationFS embed.FS

// migrateLockName 是 MySQL 命名锁的名字。
//
// 为什么需要锁：api 与 worker 是两个进程，同时启动时会同时执行迁移。
// 幂等的 CREATE TABLE IF NOT EXISTS 也扛不住两条 ALTER 并发跑同一条版本。
const migrateLockName = "laborlaw_schema_migrate"

// migration 是一条已加载的迁移。
type migration struct {
	version int
	name    string
	upSQL   string
}

// Migrate 按版本号顺序应用尚未执行的迁移，返回本次应用的数量。
//
// 不用 golang-migrate 的原因：它带进一整棵依赖树，而本项目只需要"顺序执行 up、
// 记录版本、并发时加锁"这三件事。代价是本实现不支持触发器与存储过程
// （splitStatements 只按分号切分），本项目也用不到。
func Migrate(ctx context.Context, db *sql.DB) (applied int, err error) {
	migrations, err := loadMigrations()
	if err != nil {
		return 0, err
	}

	// 命名锁绑定在单条连接上，必须用 Conn 而不是 Pool：GET_LOCK 与 RELEASE_LOCK
	// 落在不同连接上等于没加锁。
	conn, err := db.Conn(ctx)
	if err != nil {
		return 0, fmt.Errorf("获取迁移连接: %w", err)
	}
	defer conn.Close()

	var locked sql.NullInt64
	if err := conn.QueryRowContext(ctx, "SELECT GET_LOCK(?, 10)", migrateLockName).Scan(&locked); err != nil {
		return 0, fmt.Errorf("申请迁移锁: %w", err)
	}
	if !locked.Valid || locked.Int64 != 1 {
		return 0, fmt.Errorf("申请迁移锁: 10s 内未拿到锁，可能有另一个进程正在迁移")
	}
	defer func() {
		// 释放失败不覆盖主错误：连接关闭时 MySQL 会自动释放命名锁，
		// 这里只是让等待方早点继续。
		if _, relErr := conn.ExecContext(context.Background(), "SELECT RELEASE_LOCK(?)", migrateLockName); relErr != nil && err == nil {
			err = fmt.Errorf("释放迁移锁: %w", relErr)
		}
	}()

	// 版本表由迁移器自己建：如果把建表语句写进 0001，第一次执行时表还不存在。
	if _, err := conn.ExecContext(ctx, `CREATE TABLE IF NOT EXISTS schema_migrations (
		version    INT          NOT NULL,
		name       VARCHAR(128) NOT NULL,
		applied_at DATETIME(3)  NOT NULL DEFAULT CURRENT_TIMESTAMP(3),
		PRIMARY KEY (version)
	) ENGINE = InnoDB DEFAULT CHARSET = utf8mb4 COLLATE = utf8mb4_0900_ai_ci`); err != nil {
		return 0, fmt.Errorf("创建 schema_migrations: %w", err)
	}

	done, err := appliedVersions(ctx, conn)
	if err != nil {
		return 0, err
	}

	for _, m := range migrations {
		if done[m.version] {
			continue
		}
		for i, stmt := range splitStatements(m.upSQL) {
			if _, err := conn.ExecContext(ctx, stmt); err != nil {
				return applied, fmt.Errorf("应用迁移 %04d_%s 第 %d 条语句: %w", m.version, m.name, i+1, err)
			}
		}
		if _, err := conn.ExecContext(ctx,
			"INSERT INTO schema_migrations (version, name) VALUES (?, ?)", m.version, m.name); err != nil {
			return applied, fmt.Errorf("记录迁移 %04d_%s: %w", m.version, m.name, err)
		}
		applied++
	}
	return applied, nil
}

func appliedVersions(ctx context.Context, conn *sql.Conn) (map[int]bool, error) {
	rows, err := conn.QueryContext(ctx, "SELECT version FROM schema_migrations")
	if err != nil {
		return nil, fmt.Errorf("读取已应用版本: %w", err)
	}
	defer rows.Close()

	done := make(map[int]bool)
	for rows.Next() {
		var v int
		if err := rows.Scan(&v); err != nil {
			return nil, fmt.Errorf("扫描已应用版本: %w", err)
		}
		done[v] = true
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("遍历已应用版本: %w", err)
	}
	return done, nil
}

// loadMigrations 读取内嵌的迁移文件，校验 up/down 成对且版本号唯一。
//
// 在加载阶段就校验而不是等到执行：文件名写错（少一位数字、方向写成 update）
// 如果拖到运行期才发现，往往已经是部署当天。
func loadMigrations() ([]migration, error) {
	entries, err := fs.ReadDir(migrationFS, "migrations")
	if err != nil {
		return nil, fmt.Errorf("读取迁移目录: %w", err)
	}

	ups := make(map[int]migration)
	downs := make(map[int]bool)

	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".sql") {
			continue
		}
		version, name, direction, err := parseMigrationName(e.Name())
		if err != nil {
			return nil, err
		}
		content, err := fs.ReadFile(migrationFS, "migrations/"+e.Name())
		if err != nil {
			return nil, fmt.Errorf("读取迁移文件 %s: %w", e.Name(), err)
		}

		switch direction {
		case "up":
			if _, dup := ups[version]; dup {
				return nil, fmt.Errorf("迁移版本 %04d 重复定义", version)
			}
			ups[version] = migration{version: version, name: name, upSQL: string(content)}
		case "down":
			downs[version] = true
		}
	}

	if len(ups) == 0 {
		return nil, fmt.Errorf("没有任何迁移文件")
	}

	out := make([]migration, 0, len(ups))
	for v, m := range ups {
		if !downs[v] {
			return nil, fmt.Errorf("迁移 %04d_%s 缺少 down 脚本", v, m.name)
		}
		out = append(out, m)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].version < out[j].version })
	return out, nil
}

// parseMigrationName 解析 0001_init.up.sql 这一种命名。
func parseMigrationName(file string) (version int, name string, direction string, err error) {
	parts := strings.Split(file, ".")
	if len(parts) != 3 || parts[2] != "sql" {
		return 0, "", "", fmt.Errorf("迁移文件名 %q 不符合 NNNN_name.up.sql", file)
	}

	direction = parts[1]
	if direction != "up" && direction != "down" {
		return 0, "", "", fmt.Errorf("迁移文件名 %q 的方向必须是 up 或 down", file)
	}

	versionPart, name, found := strings.Cut(parts[0], "_")
	if !found || name == "" {
		return 0, "", "", fmt.Errorf("迁移文件名 %q 缺少名称", file)
	}

	v, convErr := strconv.Atoi(versionPart)
	if convErr != nil || v <= 0 {
		return 0, "", "", fmt.Errorf("迁移文件名 %q 的版本号不是正整数", file)
	}
	return v, name, direction, nil
}

// splitStatements 按分号切分 SQL，跳过整行注释与空行。
//
// 限制：不支持存储过程、触发器与 DELIMITER——那类语句内部也有分号，按行切会碎。
// 本项目只有 DDL 与简单 DML，用不到；真需要时应改用 golang-migrate。
func splitStatements(src string) []string {
	var b strings.Builder
	for _, line := range strings.Split(src, "\n") {
		trimmed := strings.TrimSpace(line)
		if trimmed == "" || strings.HasPrefix(trimmed, "--") {
			continue
		}
		b.WriteString(line)
		b.WriteString("\n")
	}

	var out []string
	for _, stmt := range strings.Split(b.String(), ";") {
		if s := strings.TrimSpace(stmt); s != "" {
			out = append(out, s)
		}
	}
	return out
}
