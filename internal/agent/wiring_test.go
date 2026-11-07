package agent

import (
	"laborlaw/internal/retrieval"
)

// 装配契约：检索器必须能直接插进 Runner。
//
// 断言放在测试文件而不是 runner.go，是为了让 agent 包本身不依赖任何具体实现——
// 生产代码里 agent 只认 port 与本文件定义的窄接口。
var _ StatuteSearcher = (*retrieval.Retriever)(nil)
