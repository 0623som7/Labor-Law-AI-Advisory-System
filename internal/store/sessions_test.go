package store

import (
	"context"
	"database/sql"
	"reflect"
	"strings"
	"testing"

	"laborlaw/internal/id"
	"laborlaw/internal/port"
)

// 会话与消息的 DAL 走 TEST_MYSQL_DSN 开关（见 files_test.go 的说明）。
// 这里刻意覆盖四类容易写错的语义：归属过滤必须在 SQL 里、空列表不是 nil、
// 软删之后查不到但行还在、消息列表要的是"最近 N 条的正序"而不是"最早 N 条"。

func cleanupSessions(t *testing.T, db *sql.DB, sessionIDs ...string) {
	t.Helper()
	t.Cleanup(func() {
		ctx := context.Background()
		for _, sid := range sessionIDs {
			for _, q := range []string{`DELETE FROM messages WHERE session_id = ?`, `DELETE FROM sessions WHERE id = ?`} {
				if _, err := db.ExecContext(ctx, q, sid); err != nil {
					t.Logf("清理失败: %v", err)
				}
			}
		}
	})
}

func newSession(userID string) *port.Session {
	return &port.Session{UserID: userID, Title: "试用期辞退", Scene: "consult"}
}

func TestSessionCreateGet(t *testing.T) {
	db := testDB(t)
	s := newSession(id.New())
	cleanupSessions(t, db, s.ID)

	ctx := context.Background()
	d := NewSessions(db)

	if err := d.Create(ctx, s); err != nil {
		t.Fatalf("Create: %v", err)
	}
	if s.ID == "" {
		t.Fatal("Create 应当回填 ID")
	}

	got, err := d.Get(ctx, s.UserID, s.ID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.Title != s.Title || got.Scene != s.Scene {
		t.Fatalf("标题/场景不符: %+v", got)
	}
	if got.CreatedAt.IsZero() || got.UpdatedAt.IsZero() {
		t.Fatalf("时间字段未填充: %+v", got)
	}
}

func TestSessionGetRejectsOtherUser(t *testing.T) {
	db := testDB(t)
	s := newSession(id.New())
	cleanupSessions(t, db, s.ID)

	ctx := context.Background()
	d := NewSessions(db)
	if err := d.Create(ctx, s); err != nil {
		t.Fatalf("Create: %v", err)
	}

	// 归属必须是查询条件：换一个 userID 查同一条必须 40401，而不是返回数据。
	_, err := d.Get(ctx, id.New(), s.ID)
	if !hasCode(err, port.CodeNotFound) {
		t.Fatalf("越权查询应返回 40401，实际 %v", err)
	}
}

func TestSessionCreateDefaultsTitle(t *testing.T) {
	db := testDB(t)
	s := &port.Session{UserID: id.New(), Scene: ""}
	ctx := context.Background()
	d := NewSessions(db)

	if err := d.Create(ctx, s); err != nil {
		t.Fatalf("Create: %v", err)
	}
	cleanupSessions(t, db, s.ID)

	if s.Title != defaultSessionTitle {
		t.Fatalf("空标题应兜底为 %q，实际 %q", defaultSessionTitle, s.Title)
	}
	if s.Scene != "consult" {
		t.Fatalf("空场景应兜底为 consult，实际 %q", s.Scene)
	}
}

func TestSessionCreateTruncatesLongTitle(t *testing.T) {
	db := testDB(t)
	// 200 个汉字是 600 字节，按字节截会把最后一个字符切坏；按 rune 截才是 100 个字。
	s := &port.Session{UserID: id.New(), Title: strings.Repeat("劳", 200), Scene: "consult"}
	ctx := context.Background()
	d := NewSessions(db)

	if err := d.Create(ctx, s); err != nil {
		t.Fatalf("Create: %v", err)
	}
	cleanupSessions(t, db, s.ID)

	if n := len([]rune(s.Title)); n != 100 {
		t.Fatalf("标题应截到 100 个字符，实际 %d", n)
	}
}

func TestSessionListByUser(t *testing.T) {
	db := testDB(t)
	userID := id.New()
	ctx := context.Background()
	d := NewSessions(db)

	empty, err := d.ListByUser(ctx, userID, 10, 0)
	if err != nil {
		t.Fatalf("ListByUser 空结果: %v", err)
	}
	if empty == nil {
		t.Fatal("空结果必须是长度为 0 的切片，不能是 nil")
	}
	if len(empty) != 0 {
		t.Fatalf("新用户不该有会话，实际 %d 条", len(empty))
	}

	first := newSession(userID)
	second := newSession(userID)
	if err := d.Create(ctx, first); err != nil {
		t.Fatalf("Create first: %v", err)
	}
	cleanupSessions(t, db, first.ID, second.ID)
	if err := d.Create(ctx, second); err != nil {
		t.Fatalf("Create second: %v", err)
	}
	// second 比 first 新，列表应当把 second 排在前面。
	if err := d.Touch(ctx, userID, second.ID); err != nil {
		t.Fatalf("Touch: %v", err)
	}

	list, err := d.ListByUser(ctx, userID, 10, 0)
	if err != nil {
		t.Fatalf("ListByUser: %v", err)
	}
	if len(list) != 2 {
		t.Fatalf("应有 2 条会话，实际 %d", len(list))
	}
	if list[0].ID != second.ID {
		t.Fatalf("列表应按 updated_at 倒序，首条应为 %s，实际 %s", second.ID, list[0].ID)
	}
}

func TestSessionTouchMissing(t *testing.T) {
	db := testDB(t)
	d := NewSessions(db)
	if err := d.Touch(context.Background(), id.New(), id.New()); !hasCode(err, port.CodeNotFound) {
		t.Fatalf("Touch 不存在的会话应 40401，实际 %v", err)
	}
}

func TestSessionDeleteIsSoft(t *testing.T) {
	db := testDB(t)
	s := newSession(id.New())
	cleanupSessions(t, db, s.ID)

	ctx := context.Background()
	d := NewSessions(db)
	if err := d.Create(ctx, s); err != nil {
		t.Fatalf("Create: %v", err)
	}
	if err := d.Delete(ctx, s.UserID, s.ID); err != nil {
		t.Fatalf("Delete: %v", err)
	}

	if _, err := d.Get(ctx, s.UserID, s.ID); !hasCode(err, port.CodeNotFound) {
		t.Fatalf("软删后 Get 应 40401，实际 %v", err)
	}
	list, err := d.ListByUser(ctx, s.UserID, 10, 0)
	if err != nil {
		t.Fatalf("ListByUser: %v", err)
	}
	for _, item := range list {
		if item.ID == s.ID {
			t.Fatal("软删的会话不应出现在列表里")
		}
	}

	// 硬删只由 cleanupSessions 做；这里确认行还在，说明是软删。
	var deleted sql.NullTime
	if err := db.QueryRowContext(ctx, `SELECT deleted_at FROM sessions WHERE id = ?`, s.ID).Scan(&deleted); err != nil {
		t.Fatalf("行应仍然存在: %v", err)
	}
	if !deleted.Valid {
		t.Fatal("deleted_at 应已置位")
	}

	// 重复删除返回 40401 而不是静默成功。
	if err := d.Delete(ctx, s.UserID, s.ID); !hasCode(err, port.CodeNotFound) {
		t.Fatalf("重复删除应 40401，实际 %v", err)
	}
}

func TestMessageAppendAndList(t *testing.T) {
	db := testDB(t)
	s := newSession(id.New())
	cleanupSessions(t, db, s.ID)

	ctx := context.Background()
	if err := NewSessions(db).Create(ctx, s); err != nil {
		t.Fatalf("Create: %v", err)
	}

	m := NewMessages(db)
	msgs := []*port.ChatMessage{
		{SessionID: s.ID, Role: port.RoleUser, Content: "公司辞退我"},
		{SessionID: s.ID, Role: port.RoleAssistant, Content: "请先确认解除理由", PromptTokens: 120, CompletionTokens: 60, TraceID: "trace-1"},
	}
	for _, msg := range msgs {
		if err := m.Append(ctx, msg); err != nil {
			t.Fatalf("Append: %v", err)
		}
		if msg.ID == "" {
			t.Fatal("Append 应当回填 ID")
		}
	}

	got, err := m.ListBySession(ctx, s.ID, 10)
	if err != nil {
		t.Fatalf("ListBySession: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("应有 2 条消息，实际 %d", len(got))
	}
	if got[0].Role != port.RoleUser || got[1].Role != port.RoleAssistant {
		t.Fatalf("顺序应为时间正序，实际 %v / %v", got[0].Role, got[1].Role)
	}
	if got[1].PromptTokens != 120 || got[1].TraceID != "trace-1" {
		t.Fatalf("计量字段未落库: %+v", got[1])
	}
}

func TestMessageListReturnsLatest(t *testing.T) {
	db := testDB(t)
	s := newSession(id.New())
	cleanupSessions(t, db, s.ID)

	ctx := context.Background()
	if err := NewSessions(db).Create(ctx, s); err != nil {
		t.Fatalf("Create: %v", err)
	}

	m := NewMessages(db)
	for i := 0; i < 5; i++ {
		if err := m.Append(ctx, &port.ChatMessage{SessionID: s.ID, Role: port.RoleUser, Content: string(rune('a' + i))}); err != nil {
			t.Fatalf("Append %d: %v", i, err)
		}
	}

	// limit=2 要的是最后两条，且仍是正序：d、e。
	got, err := m.ListBySession(ctx, s.ID, 2)
	if err != nil {
		t.Fatalf("ListBySession: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("应返回 2 条，实际 %d", len(got))
	}
	if got[0].Content != "d" || got[1].Content != "e" {
		t.Fatalf("应返回最近两条且正序，实际 %q %q", got[0].Content, got[1].Content)
	}
}

func TestMessageListEmptyNotNil(t *testing.T) {
	db := testDB(t)
	got, err := NewMessages(db).ListBySession(context.Background(), id.New(), 10)
	if err != nil {
		t.Fatalf("ListBySession: %v", err)
	}
	if got == nil {
		t.Fatal("空结果必须是长度为 0 的切片，不能是 nil")
	}
}

// TestMessageCitationsRoundTrip 守住"引用法条能存进去、能读回来"。
//
// 缺陷形态：messages 表里只有正文，前端刷新后只能承认"库里没存引用"，
// 当前轮那 3 张法条卡片一刷新就消失。
func TestMessageCitationsRoundTrip(t *testing.T) {
	db := testDB(t)
	s := newSession(id.New())
	cleanupSessions(t, db, s.ID)

	ctx := context.Background()
	if err := NewSessions(db).Create(ctx, s); err != nil {
		t.Fatalf("Create: %v", err)
	}

	citations := []port.Citation{
		{StatuteID: "LCL-38", Law: "中华人民共和国劳动合同法", Article: "第三十八条", Text: "用人单位有下列情形之一的，劳动者可以解除劳动合同。", URL: "https://flk.npc.gov.cn/"},
		{StatuteID: "LL-91", Law: "中华人民共和国劳动法", Article: "第九十一条", Text: "用人单位有下列侵害劳动者合法权益情形之一的，由劳动行政部门责令支付劳动者的工资报酬、经济补偿，并可以责令支付赔偿金。", URL: ""},
	}
	m := NewMessages(db)
	if err := m.Append(ctx, &port.ChatMessage{
		SessionID:    s.ID,
		Role:         port.RoleAssistant,
		Content:      "可以解除并要求经济补偿。",
		PromptTokens: 120,
		Citations:    citations,
	}); err != nil {
		t.Fatalf("Append: %v", err)
	}
	// 再写一条没有引用的 user 消息：它读回来必须是 nil，而不是空切片里的两条空引用。
	if err := m.Append(ctx, &port.ChatMessage{SessionID: s.ID, Role: port.RoleUser, Content: "那我能要补偿吗"}); err != nil {
		t.Fatalf("Append user: %v", err)
	}

	got, err := m.ListBySession(ctx, s.ID, 10)
	if err != nil {
		t.Fatalf("ListBySession: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("应返回 2 条，实际 %d", len(got))
	}
	// 按 role 定位而不是按下标：两条消息可能落在同一毫秒，正序只保证 id 递增。
	var assistant, user *port.ChatMessage
	for i := range got {
		if got[i].Role == port.RoleAssistant {
			assistant = &got[i]
		} else {
			user = &got[i]
		}
	}
	if assistant == nil || user == nil {
		t.Fatalf("role 不符合预期: %+v", got)
	}
	if len(user.Citations) != 0 {
		t.Fatalf("user 消息不该有引用，实际 %+v", user.Citations)
	}
	if !reflect.DeepEqual(assistant.Citations, citations) {
		t.Fatalf("引用往返不一致:\n got %+v\nwant %+v", assistant.Citations, citations)
	}
	if assistant.PromptTokens != 120 {
		t.Fatalf("token 计量丢了: %+v", assistant)
	}
}
