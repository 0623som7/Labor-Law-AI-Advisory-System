// Package port 定义 A（后端）与 B（Agent）之间唯一的编译期契约。
//
// 本包只放 interface、跨域 DTO 与错误码，不含任何实现，也不允许 import 任何具体实现包
// （gin / sqlx / redis / eino 一律不许出现在本包的 import 里）。原因是它是两个人并行开发的
// 唯一接缝：任何一侧往里塞实现细节，另一侧的编译速度就会被对方的依赖树拖住，
// 而且接口签名会跟着实现漂移。
//
// 定稿时间：W1（2026-11-06）。之后只加不改：要改签名，提供方先出变更单，消费方 24h 内回话。
//
// # 接口 × 提供方 × 消费方 × 变化频率
//
//	接口              文件        提供方  消费方  变化频率
//	llm.Client        llm.go      A       B       低：签名已冻结，只加能力不加参数
//	agent.Runner      agent.go    B       A       中：B 调 graph 时可能想加 Options 语义
//	store.Store       store.go    A       B       高：每加一张表就加一个子 store 方法
//	worker.Handler    worker.go   A（定义） B（注册） 低：函数签名是三方法里最稳的
//	file.ParsedDoc    file.go     A       B       低：坐标协议冻结，只加字段不加语义
//
// # 命名与边界约定
//
//  1. 本包的类型是"跨域 DTO"：它同时是 DAL 的投影和 Agent 的输入，因此不许带 driver 类型
//     （sql.NullString、redis.Cmdable 之类）。
//  2. 领域实体（Session / FileRecord / Review / Task ...）放 model.go，interface 只引用它们，
//     不定义它们——这样 B 加字段评审时只看一个文件。
//  3. internal/file、internal/agent 等实现包用类型别名（type ParsedDoc = port.ParsedDoc）
//     复脚本里写的 file.ParsedDoc / agent.Event，保证方案文档里的字面写法能编译。
//  4. 所有跨包错误都是 *AppError（errors.go）。非 AppError 的 error 视为内部缺陷，
//     由 API 层统一折成 50001，不允许原样返回给前端。
//
// # 待消费方（B）确认的增项
//
// 以下增项是 A 在实现 A5/A10 时发现的缺口，按"只加不改"流程在此登记，未获 B 回话前不落地：
//
//	提议                              原因                                 状态
//	Store.User() UserStore            A10 需要 users 表读写（注册/登录/软删）  待 B 签字
//	Store.Usage() UsageStore          A3 的 token 计量落库需要写 usage 表      待 B 签字
//	Result 结构体字段                 §3.2 只给了 Runner 签名，没给 Result    待 B 签字
package port
