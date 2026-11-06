package store

import (
	"context"
	"database/sql"
	"errors"
	"testing"

	"laborlaw/internal/auth"
	"laborlaw/internal/port"
)

// usersTestSecret 是集成测试用的手机号哈希密钥，长度与 minSecretLen 一致。
const usersTestSecret = "0123456789abcdef0123456789abcdef"

// newTestUsers 构造 Dal 并注册清理：按手机号哈希删掉测试写入的行。
func newTestUsers(t *testing.T, db *sql.DB, phone string) *Users {
	t.Helper()
	users := NewUsers(db, usersTestSecret)

	hash, err := auth.Hash(usersTestSecret, phone)
	if err != nil {
		t.Fatalf("计算手机号哈希失败: %v", err)
	}
	t.Cleanup(func() {
		if _, err := db.Exec(`DELETE FROM users WHERE phone_hash = ?`, hash); err != nil {
			t.Logf("清理用户失败: %v", err)
		}
	})
	return users
}

func TestUsersCreateAndGet(t *testing.T) {
	db := testDB(t)
	users := newTestUsers(t, db, "13900139000")

	hash, err := auth.HashPassword("password123")
	if err != nil {
		t.Fatalf("哈希密码失败: %v", err)
	}

	u := &port.User{Phone: "13900139000", Nickname: "小码", PasswordHash: hash}
	if err := users.Create(context.Background(), u); err != nil {
		t.Fatalf("创建用户失败: %v", err)
	}
	if len(u.ID) != 26 {
		t.Fatalf("ID 应当是 26 位 ULID，实得 %q", u.ID)
	}
	if u.Phone != "139****9000" {
		t.Fatalf("Create 后 Phone 应当已被就地脱敏，实得 %q", u.Phone)
	}
	if u.CreatedAt.IsZero() {
		t.Fatal("Create 应当回填 created_at")
	}

	got, err := users.GetByPhone(context.Background(), "13900139000")
	if err != nil {
		t.Fatalf("按手机号查询失败: %v", err)
	}
	if got.ID != u.ID || got.Nickname != "小码" {
		t.Fatalf("查回的用户不符: %+v", got)
	}
	if got.Phone != "139****9000" {
		t.Fatalf("出库手机号必须是脱敏形态，实得 %q", got.Phone)
	}
	if got.PasswordHash != hash {
		t.Fatal("密码哈希不一致")
	}

	byID, err := users.Get(context.Background(), u.ID)
	if err != nil {
		t.Fatalf("按 ID 查询失败: %v", err)
	}
	if byID.ID != u.ID {
		t.Fatalf("按 ID 查回的不是同一用户: %+v", byID)
	}
}

func TestUsersDuplicatePhoneConflict(t *testing.T) {
	db := testDB(t)
	users := newTestUsers(t, db, "13900139001")

	hash, err := auth.HashPassword("password123")
	if err != nil {
		t.Fatalf("哈希密码失败: %v", err)
	}
	first := &port.User{Phone: "13900139001", PasswordHash: hash}
	if err := users.Create(context.Background(), first); err != nil {
		t.Fatalf("首次创建失败: %v", err)
	}

	second := &port.User{Phone: "13900139001", PasswordHash: hash}
	err = users.Create(context.Background(), second)
	if !errors.Is(err, &port.AppError{Code: port.CodeConflict}) {
		t.Fatalf("重复手机号应当报 40901，实得 %v", err)
	}
}

func TestUsersSoftDeleteHidesUser(t *testing.T) {
	db := testDB(t)
	users := newTestUsers(t, db, "13900139002")

	hash, err := auth.HashPassword("password123")
	if err != nil {
		t.Fatalf("哈希密码失败: %v", err)
	}
	u := &port.User{Phone: "13900139002", PasswordHash: hash}
	if err := users.Create(context.Background(), u); err != nil {
		t.Fatalf("创建用户失败: %v", err)
	}

	if _, err := db.Exec(`UPDATE users SET deleted_at = CURRENT_TIMESTAMP(3) WHERE id = ?`, u.ID); err != nil {
		t.Fatalf("软删除失败: %v", err)
	}

	if _, err := users.GetByPhone(context.Background(), "13900139002"); !errors.Is(err, &port.AppError{Code: port.CodeNotFound}) {
		t.Fatalf("软删后按手机号应当查不到，实得 %v", err)
	}
	if _, err := users.Get(context.Background(), u.ID); !errors.Is(err, &port.AppError{Code: port.CodeNotFound}) {
		t.Fatalf("软删后按 ID 应当查不到，实得 %v", err)
	}
}

func TestUsersUnknownPhoneNotFound(t *testing.T) {
	db := testDB(t)
	users := NewUsers(db, usersTestSecret)

	if _, err := users.GetByPhone(context.Background(), "13911112222"); !errors.Is(err, &port.AppError{Code: port.CodeNotFound}) {
		t.Fatalf("未注册号码应当返回 40401，实得 %v", err)
	}
}

func TestUsersInvalidPhoneRejected(t *testing.T) {
	db := testDB(t)
	users := NewUsers(db, usersTestSecret)

	if _, err := users.GetByPhone(context.Background(), "123"); !errors.Is(err, &port.AppError{Code: port.CodeInvalidArgument}) {
		t.Fatalf("非法手机号应当返回 40001，实得 %v", err)
	}
	hash, _ := auth.HashPassword("password123")
	err := users.Create(context.Background(), &port.User{Phone: "abc", PasswordHash: hash})
	if !errors.Is(err, &port.AppError{Code: port.CodeInvalidArgument}) {
		t.Fatalf("非法手机号建号应当返回 40001，实得 %v", err)
	}
}

func TestDBUsersWithoutSecretIsError(t *testing.T) {
	// 没给密钥时不许返回 nil 接口：调用方第一次调用就 panic 比明确的 50001 难查得多。
	agg := New(nil)
	if _, err := agg.Users().GetByPhone(context.Background(), "13900139000"); err == nil {
		t.Fatal("未配置时应当返回错误而不是 nil")
	}
}
