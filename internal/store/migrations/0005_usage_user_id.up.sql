-- usage_records 补 user_id：成本核算要能回答"钱花在谁身上"。
--
-- 默认空串而不是 NOT NULL 无默认值：这一列上线前的历史记录本来就不属于任何人，
-- 强行回填成某个用户等于编数据。空串单独归组即可（见文档里的报表口径）。
ALTER TABLE usage_records
    ADD COLUMN user_id CHAR(26) NOT NULL DEFAULT '' AFTER id,
    ADD KEY idx_usage_user_created (user_id, created_at);
