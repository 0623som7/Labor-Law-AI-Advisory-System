package api

import (
	"context"
	"net/http"
	"regexp"
	"testing"
	"time"

	"laborlaw/internal/port"
	"laborlaw/internal/stream"
)

// chatFixture 把"问答 + 落库"这条链路装起来。
//
// 不用真实 MySQL：这里要验的是"什么时候写、写了什么、ID 对不对"，
// 全是 handler 的决策；SQL 行为由 internal/store 的集成用例负责。
type chatFixture struct {
	h        http.Handler
	sessions *fakeSessionStore
	messages *fakeMessageStore
}

// chatStore 是带事务的聚合体：Tx 直接跑回调，让 handler 的
// "两条消息必须一起写"在测试里也能被观察到。
type chatStore struct {
	port.Store
	sessions *fakeSessionStore
	messages *fakeMessageStore
	files    port.FileStore
}

func (s chatStore) Session() port.SessionStore { return s.sessions }
func (s chatStore) Message() port.MessageStore { return s.messages }
func (s chatStore) File() port.FileStore       { return s.files }

func (s chatStore) Tx(_ context.Context, fn func(port.Store) error) error {
	return fn(s)
}

// fakeAttachFiles 只实现 FileStore.Get：附件校验只走这一条路。
//
// 归属过滤照真实现的语义写在假实现里（userID 不匹配就当查不到），
// 这样"文件属于别人"那条分支测的是 handler 挑的错误码，不是 SQL。
type fakeAttachFiles struct {
	port.FileStore
	rec    *port.FileRecord
	getErr error
}

func (f *fakeAttachFiles) Get(_ context.Context, userID, fileID string) (*port.FileRecord, error) {
	if f.getErr != nil {
		return nil, f.getErr
	}
	if f.rec == nil || f.rec.ID != fileID || f.rec.UserID != userID {
		return nil, port.New(port.CodeNotFound, "文件不存在")
	}
	return f.rec, nil
}

func newChatFixture(t *testing.T, r port.Runner) *chatFixture {
	t.Helper()
	return newChatFixtureWithFiles(t, r, &fakeAttachFiles{})
}

// newChatFixtureWithFiles 让用例能指定假 FileStore（附件用例要控制"这份文件属于谁"）。
func newChatFixtureWithFiles(t *testing.T, r port.Runner, files port.FileStore) *chatFixture {
	t.Helper()
	sessions := newFakeSessions()
	messages := &fakeMessageStore{}
	h := New(testConfig("dev-user-0001"), discardLogger(),
		WithRunner(r),
		WithHub(stream.NewHub(stream.Options{HeartbeatInterval: time.Hour}, time.Minute, discardLogger())),
		WithStore(chatStore{sessions: sessions, messages: messages, files: files}),
	).Handler()
	return &chatFixture{h: h, sessions: sessions, messages: messages}
}

var metaMessageIDRe = regexp.MustCompile(`"message_id":"([^"]+)"`)

func TestChatPersistsTurn(t *testing.T) {
	f := newChatFixture(t, &fakeRunner{chunks: []string{"可以据此解除", "劳动合同。"}})

	rec := postChat(t, f.h, `{"input":"公司不给我交社保，我能直接走吗"}`, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body=%s", rec.Code, rec.Body.String())
	}

	if f.sessions.created == nil {
		t.Fatal("没有 session_id 时必须新建会话")
	}
	if f.sessions.created.Scene != chatScene {
		t.Fatalf("scene = %q, want %q", f.sessions.created.Scene, chatScene)
	}
	if got := f.sessions.created.Title; got != "公司不给我交社保，我能直接走吗" {
		t.Fatalf("标题 = %q", got)
	}

	if len(f.messages.appended) != 2 {
		t.Fatalf("落库消息数 = %d, want 2（user + assistant）", len(f.messages.appended))
	}
	user, assistant := f.messages.appended[0], f.messages.appended[1]
	if user.Role != port.RoleUser || user.Content != "公司不给我交社保，我能直接走吗" {
		t.Fatalf("user 消息 = %+v", user)
	}
	if user.SessionID != f.sessions.created.ID {
		t.Fatalf("user 消息的 session_id = %q, want %q", user.SessionID, f.sessions.created.ID)
	}
	if assistant.Role != port.RoleAssistant || assistant.Content != "可以据此解除劳动合同。" {
		t.Fatalf("assistant 消息 = %+v", assistant)
	}
	if assistant.PromptTokens != 10 || assistant.CompletionTokens != 20 {
		t.Fatalf("token 计量没落库: %+v", assistant)
	}

	// assistant 消息的 ID 必须与 meta 帧里的 message_id 一致，
	// 否则前端刷新历史后无法把占位气泡替换成真正那条。
	m := metaMessageIDRe.FindStringSubmatch(rec.Body.String())
	if m == nil {
		t.Fatalf("响应里没有 message_id: %s", rec.Body.String())
	}
	if assistant.ID != m[1] {
		t.Fatalf("assistant 消息 ID = %q, 与 meta 的 message_id %q 不一致", assistant.ID, m[1])
	}
	if f.sessions.touched != "" && f.sessions.touched != f.sessions.created.ID {
		t.Fatalf("Touch 打到 %q", f.sessions.touched)
	}
}

func TestChatSessionTitleTruncated(t *testing.T) {
	f := newChatFixture(t, &fakeRunner{chunks: []string{"好"}})

	long := "一二三四五六七八九十一二三四五六七八九十二十一"
	postChat(t, f.h, `{"input":"`+long+`"}`, nil)

	if f.sessions.created == nil {
		t.Fatal("会话没有创建")
	}
	// 20 个 rune + 省略号。
	if n := len([]rune(f.sessions.created.Title)); n != chatSessionTitleRunes+1 {
		t.Fatalf("标题长度 = %d 个字符, want %d", n, chatSessionTitleRunes+1)
	}
}

// TestChatRejectsUnknownSession 守住"会话不属于你时必须 404，而不是开流"。
//
// 缺陷形态：拿到 session_id 就 Attach 开始推流，等 Run 内部才发现没有这个会话。
// 此时响应头已经是 200 + text/event-stream，前端只能看到一个空回答，
// 而越权访问也就从"被拒绝"变成了"静默成功"。
func TestChatRejectsUnknownSession(t *testing.T) {
	f := newChatFixture(t, &fakeRunner{chunks: []string{"不该出现"}})

	rec := postChat(t, f.h, `{"session_id":"someone-elses","input":"继续"}`, nil)

	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404, body=%s", rec.Code, rec.Body.String())
	}
	env := decodeEnvelope(t, rec.Body.Bytes())
	if env.Code != int(port.CodeNotFound) {
		t.Fatalf("code = %d, want 40401", env.Code)
	}
	if len(f.messages.appended) != 0 {
		t.Fatalf("越权请求不该落库，写入了 %d 条", len(f.messages.appended))
	}
}

// TestChatRunErrorDoesNotPersistAnswer 守住"失败的一轮不写答案"。
//
// 写进一条空的 assistant 消息会让前端历史里出现一个永远空白的回答，
// 用户以为系统答过、只是一片空白。
func TestChatRunErrorDoesNotPersistAnswer(t *testing.T) {
	f := newChatFixture(t, &fakeRunner{runErr: port.New(port.CodeUpstream, "上游失败")})

	rec := postChat(t, f.h, `{"input":"试用期能随便辞退吗"}`, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("SSE 已开流，状态码应为 200，得到 %d", rec.Code)
	}
	for _, m := range f.messages.appended {
		if m.Role == port.RoleAssistant {
			t.Fatalf("失败的一轮不该写 assistant 消息，写入了 %+v", m)
		}
	}
}

// TestChatErrorPersistsUserQuestion 守住"失败的一轮仍要留下提问"。
//
// 缺陷形态：模型超时后 runErr 分支直接 return，user 消息没落库。
// 会话标题却已经由这句话生成，于是侧边栏有一条标题、打开却是空的，
// 刷新一次就把"我问过什么"抹掉了。
func TestChatErrorPersistsUserQuestion(t *testing.T) {
	const input = "拖欠工资三个月能要经济补偿吗"
	f := newChatFixture(t, &fakeRunner{runErr: port.New(port.CodeUpstream, "上游失败")})

	postChat(t, f.h, `{"input":"`+input+`"}`, nil)

	if len(f.messages.appended) != 1 {
		t.Fatalf("落库消息数 = %d, want 1（只有 user）", len(f.messages.appended))
	}
	got := f.messages.appended[0]
	if got.Role != port.RoleUser {
		t.Fatalf("落库的 role = %q, want user", got.Role)
	}
	if got.Content != input {
		t.Fatalf("落库的 content = %q, want %q", got.Content, input)
	}
	if f.sessions.created == nil || got.SessionID != f.sessions.created.ID {
		t.Fatalf("user 消息的 session_id = %q，与新建会话 %+v 不一致", got.SessionID, f.sessions.created)
	}
	// Touch 也要走：用户刚问过，会话应按最近活跃排到前面。
	if f.sessions.touched != got.SessionID {
		t.Fatalf("Touch 打到 %q, want %q", f.sessions.touched, got.SessionID)
	}
}

// TestChatPersistsCitations 守住"引用法条随 assistant 消息一起落库"。
//
// 缺陷形态：citation 帧只活在 SSE 流里，库里只存正文。
// 于是当前轮有 3 张法条卡片，一刷新就全没了——答辩演示"关掉页面再打开"时无法解释。
func TestChatPersistsCitations(t *testing.T) {
	citations := []port.Citation{
		{StatuteID: "LCL-38", Law: "中华人民共和国劳动合同法", Article: "第三十八条", Text: "用人单位有下列情形之一的，劳动者可以解除劳动合同。", URL: "https://flk.npc.gov.cn/"},
		{StatuteID: "LCL-46", Law: "中华人民共和国劳动合同法", Article: "第四十六条", Text: "下列情形之一的，用人单位应当向劳动者支付经济补偿。", URL: ""},
	}
	f := newChatFixture(t, &fakeRunner{chunks: []string{"可以解除。"}, citations: citations})

	rec := postChat(t, f.h, `{"input":"公司不给我交社保，我能直接走吗"}`, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
	if len(f.messages.appended) != 2 {
		t.Fatalf("落库消息数 = %d, want 2", len(f.messages.appended))
	}
	assistant := f.messages.appended[1]
	if assistant.Role != port.RoleAssistant {
		t.Fatalf("第二条应为 assistant，实际 %+v", assistant)
	}
	if len(assistant.Citations) != len(citations) {
		t.Fatalf("引用了 %d 条，want %d（%+v）", len(assistant.Citations), len(citations), assistant.Citations)
	}
	for i := range citations {
		if assistant.Citations[i] != citations[i] {
			t.Fatalf("第 %d 条引用 = %+v, want %+v", i, assistant.Citations[i], citations[i])
		}
	}
	// user 消息不该被塞引用：卡片只属于回答。
	if len(f.messages.appended[0].Citations) != 0 {
		t.Fatalf("user 消息带了引用: %+v", f.messages.appended[0].Citations)
	}
}

// TestChatPersistsAttachment 守住"这轮问题带了合同，附件随 user 消息落库"。
//
// 缺陷形态：附件只活在前端内存里，刷新后卡片消失，用户点不回审查页；
// 而 assistant 消息带上附件更糟——历史里会出现"回答也带一份合同"的假象。
func TestChatPersistsAttachment(t *testing.T) {
	file := &port.FileRecord{ID: "01J9Z0K7Q2X3M4N5P6R7S8T9V0", UserID: "dev-user-0001", Filename: "劳动合同.pdf", PageCount: 4}
	f := newChatFixtureWithFiles(t, &fakeRunner{chunks: []string{"第三条有问题。"}}, &fakeAttachFiles{rec: file})

	rec := postChat(t, f.h, `{"input":"帮我看看这份合同有没有风险","file_id":"`+file.ID+`"}`, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body=%s", rec.Code, rec.Body.String())
	}
	if len(f.messages.appended) != 2 {
		t.Fatalf("落库消息数 = %d, want 2（user + assistant）", len(f.messages.appended))
	}

	user, assistant := f.messages.appended[0], f.messages.appended[1]
	if len(user.Attachments) != 1 {
		t.Fatalf("user 消息附件数 = %d, want 1（%+v）", len(user.Attachments), user.Attachments)
	}
	want := port.Attachment{FileID: file.ID, Filename: file.Filename, PageCount: file.PageCount}
	if user.Attachments[0] != want {
		t.Fatalf("附件 = %+v, want %+v", user.Attachments[0], want)
	}
	if len(assistant.Attachments) != 0 {
		t.Fatalf("assistant 消息不该带附件: %+v", assistant.Attachments)
	}
}

// TestChatRejectsOthersAttachment 守住"文件不属于你"与"会话不属于你"同一个错误码。
//
// 两种错误必须不可区分：能分辨的话，别人的 file_id 是否有效就成了可探测信息。
// 同时一条消息都不许落库——否则越权请求会在会话里留下一条带别人文件的卡片。
func TestChatRejectsOthersAttachment(t *testing.T) {
	file := &port.FileRecord{ID: "01J9Z0K7Q2X3M4N5P6R7S8T9V1", UserID: "someone-else", Filename: "别人的合同.pdf", PageCount: 2}
	f := newChatFixtureWithFiles(t, &fakeRunner{chunks: []string{"不该出现"}}, &fakeAttachFiles{rec: file})

	rec := postChat(t, f.h, `{"input":"看看这份","file_id":"`+file.ID+`"}`, nil)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404, body=%s", rec.Code, rec.Body.String())
	}
	env := decodeEnvelope(t, rec.Body.Bytes())
	if env.Code != int(port.CodeNotFound) {
		t.Fatalf("code = %d, want 40401", env.Code)
	}
	if len(f.messages.appended) != 0 {
		t.Fatalf("越权附件不该落库，写入了 %d 条", len(f.messages.appended))
	}
	// 会话也不许建：resolveSession 在没有 session_id 时会 Create，
	// 附件校验排在它后面的话，侧栏就会多一条点进去什么都没有的空会话。
	if f.sessions.created != nil {
		t.Fatal("附件校验失败时不应创建会话")
	}
}
