package store

import (
	"context"

	"laborlaw/internal/port"
)

// 本文件存放 M3 阶段尚未落地的域。
//
// 为什么返回"始终报错"的实现而不是 nil：port.Store 的六个 accessor 返回值是接口，
// 一旦有人写了 `st.Session().Create(...)` 而 accessor 返回 nil 接口，得到的是
// runtime panic（nil pointer dereference），recovery 之后只剩一条 500 日志；
// 返回一个明确报 50001 的实现，调用方拿到的是可以打日志、可以断言错误码的错误。
// 每落地一个域就删掉对应的类型，这里同样应当随里程碑变空。

// unimplementedFile 是 File() 在 MySQL 未配置时的兜底。
// 正常的 db == nil（dev 无存储模式）下也不该走到这里——main 不会注入 Store。
type unimplementedFile struct{}

var _ port.FileStore = unimplementedFile{}

func (unimplementedFile) Create(context.Context, *port.FileRecord) error {
	return errNotImplemented("文件")
}

func (unimplementedFile) Get(context.Context, string, string) (*port.FileRecord, error) {
	return nil, errNotImplemented("文件")
}

func (unimplementedFile) GetByDigest(context.Context, string, string) (*port.FileRecord, error) {
	return nil, errNotImplemented("文件")
}

func (unimplementedFile) Update(context.Context, *port.FileRecord) error {
	return errNotImplemented("文件")
}

func (unimplementedFile) SavePages(context.Context, string, []port.ParsedPage) error {
	return errNotImplemented("文件")
}

func (unimplementedFile) LoadPages(context.Context, string) ([]port.ParsedPage, error) {
	return nil, errNotImplemented("文件")
}

// unimplementedSession 是会话域占位。
type unimplementedSession struct{}

var _ port.SessionStore = unimplementedSession{}

func (unimplementedSession) Create(context.Context, *port.Session) error {
	return errNotImplemented("会话")
}

func (unimplementedSession) Get(context.Context, string, string) (*port.Session, error) {
	return nil, errNotImplemented("会话")
}

func (unimplementedSession) ListByUser(context.Context, string, int, int) ([]port.Session, error) {
	return nil, errNotImplemented("会话")
}

func (unimplementedSession) Touch(context.Context, string, string) error {
	return errNotImplemented("会话")
}

func (unimplementedSession) Delete(context.Context, string, string) error {
	return errNotImplemented("会话")
}

// unimplementedMessage 是消息域占位。
type unimplementedMessage struct{}

var _ port.MessageStore = unimplementedMessage{}

func (unimplementedMessage) Append(context.Context, *port.ChatMessage) error {
	return errNotImplemented("消息")
}

func (unimplementedMessage) ListBySession(context.Context, string, int) ([]port.ChatMessage, error) {
	return nil, errNotImplemented("消息")
}

// unimplementedReview 是审查域占位。
type unimplementedReview struct{}

var _ port.ReviewStore = unimplementedReview{}

func (unimplementedReview) Create(context.Context, *port.Review) error {
	return errNotImplemented("审查")
}

func (unimplementedReview) Get(context.Context, string, string) (*port.Review, error) {
	return nil, errNotImplemented("审查")
}

func (unimplementedReview) GetByFile(context.Context, string, string) (*port.Review, error) {
	return nil, errNotImplemented("审查")
}

func (unimplementedReview) Update(context.Context, *port.Review) error {
	return errNotImplemented("审查")
}

func (unimplementedReview) SaveFindings(context.Context, string, []port.Finding) error {
	return errNotImplemented("审查")
}

func (unimplementedReview) ListFindings(context.Context, string) ([]port.Finding, error) {
	return nil, errNotImplemented("审查")
}

func (unimplementedReview) ListByUser(context.Context, string, int, int) ([]port.Review, error) {
	return nil, errNotImplemented("审查")
}

// unimplementedStatute 是法条域占位。
type unimplementedStatute struct{}

var _ port.StatuteStore = unimplementedStatute{}

func (unimplementedStatute) Get(context.Context, string) (*port.Statute, error) {
	return nil, errNotImplemented("法条")
}

func (unimplementedStatute) ReplaceAll(context.Context, []port.Statute) error {
	return errNotImplemented("法条")
}

func (unimplementedStatute) Count(context.Context) (int, error) {
	return 0, errNotImplemented("法条")
}

func (unimplementedStatute) SearchText(context.Context, string, string, int) ([]port.StatuteHit, error) {
	return nil, errNotImplemented("法条")
}

func (unimplementedStatute) ReplaceCases(context.Context, []port.LegalCase) error {
	return errNotImplemented("法条")
}

func (unimplementedStatute) SearchCases(context.Context, string, string, int) ([]port.CaseHit, error) {
	return nil, errNotImplemented("法条")
}

func (unimplementedStatute) SaveEmbeddings(context.Context, []port.Embedding) error {
	return errNotImplemented("法条")
}

func (unimplementedStatute) LoadEmbeddings(context.Context, string, string) ([]port.Embedding, error) {
	return nil, errNotImplemented("法条")
}

// unimplementedTask 是任务域占位。
type unimplementedTask struct{}

var _ port.TaskStore = unimplementedTask{}

func (unimplementedTask) Create(context.Context, *port.Task) error {
	return errNotImplemented("任务")
}

func (unimplementedTask) Get(context.Context, string) (*port.Task, error) {
	return nil, errNotImplemented("任务")
}

func (unimplementedTask) UpdateProgress(context.Context, string, int, string) error {
	return errNotImplemented("任务")
}

func (unimplementedTask) Finish(context.Context, string, port.TaskStatus, string) error {
	return errNotImplemented("任务")
}

func (unimplementedTask) ListByStatus(context.Context, port.TaskStatus, int) ([]port.Task, error) {
	return nil, errNotImplemented("任务")
}

func (unimplementedTask) FailRunning(context.Context, string) (int64, error) {
	return 0, errNotImplemented("任务")
}

// errNotImplemented 统一构造"域尚未实现"的 50001。
//
// 刻意用 50001 而不是新增错误码：这是一个"代码还没写完"的状态，不是业务分支；
// 用户在演示时看到它，说明有人接了这条链路却没接数据。
func errNotImplemented(domain string) error {
	return port.New(port.CodeInternal, "%s尚未实现", domain)
}
