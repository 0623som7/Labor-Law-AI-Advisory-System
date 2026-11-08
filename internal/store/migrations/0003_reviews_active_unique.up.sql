-- 同一份合同同一时间只允许一条在跑的审查。
--
-- 背景：api 层的"先 GetByFile 查重、再 Create"不在同一个事务里（中间还要建 task、
-- 入池），双击按钮或前端超时重试时两个请求都查不到已有记录，于是各建一条 review
-- + 一条 task：一次审查变成两次 50–110 秒的模型计费，两条记录还可能一条 succeeded
-- 一条卡 pending。
--
-- 为什么不用 (user_id, file_id) 唯一键：同一份合同**允许**历史多条审查（规则更新后
-- 重跑），要拦的只是"同时有两条在跑"。
--
-- 生成列 + NULL 的技巧：MySQL 的唯一索引不约束 NULL，所以
--   status ∈ (pending, running) → active = 1，唯一键生效，同 (user, file) 最多一条；
--   status 落到终态            → active = NULL，不参与唯一约束，历史记录随便多条。
-- 状态改变时生成列由 MySQL 自动重算，不需要应用层维护。
ALTER TABLE reviews
  ADD COLUMN active TINYINT
    AS (IF(status IN ('pending', 'running'), 1, NULL)) STORED,
  ADD UNIQUE KEY uk_reviews_active_file (user_id, file_id, active);
