# 架构图说明（docs/assets/architecture.png）

一句话：三层数据流从浏览器经 nginx 进 Go 服务，落到 MySQL/Redis 两个存储，另挂两条外部模型依赖，备用通道是 8443 自签证书入口。

- **第一层 接入**：浏览器（Vue 3 + TS，SSE 流式、PDF.js 渲染）走 HTTPS 443，证书是 Let's Encrypt；nginx 负责 80→443 跳转、静态资源、`/api` 反向代理，并把 `/metrics` 对外屏蔽。
- **第二层 应用**：Go 服务（Gin）拆四条链路——会话与鉴权（JWT + 双桶限流）、对话链路（检索 → 提示词 → 流式输出）、审查链路（PDF 解析 → 21 条规则 → 坐标回填）、worker（异步审查任务）。
- **第三层 存储**：MySQL 8 存法条 596 条 / 会话 / 审查 / 计量；Redis 存限流桶、向量缓存、账户桶。
- **两条外部依赖**：DashScope 的 qwen-plus 负责对话生成，text-embedding-v3 负责 596×1024 维向量；qwen3-rerank 已实现并保留为对照档，线上未启用。
- **一处兜底通道**：8443 端口挂自签证书，主域证书/链路出问题时答辩现场仍可访问。

## 产物与源的关系
- architecture.png 由 docs/assets/tools/make_arch.py 逐像素生成，是**权威版本**，PPT 用的是它
- architecture.mmd 保留为可读备份，**不是生成源**
- 重跑 make_arch.py 后需确认 PNG 尺寸仍为 2400×1350（16:9）

## 为什么不用 mermaid 直出
mermaid-cli 12.2.0 有三处限制，无法表达这张图：
1. subgraph 内的 direction 被忽略 —— 三段横排做不出来（TB 版渲染成 3210×4800，比例 0.669）
2. cluster 之间的连线会塌陷（横排版本 cluster 级连线只有 410×900，比例 0.46）
3. block-beta 会把 `-->|443|` 当成 block 名解析；HTML <table> 节点不渲染
因此 mmd 保持 TB 形态作为"内容一致的可解析备份"，实际出图走 Python 绘制。
如需重新出图，改 make_arch.py 里的坐标常量，不要改 mmd。
