-- 回滚 0005：索引要先删，否则 MySQL 会因"列仍被索引引用"拒绝 DROP COLUMN。
ALTER TABLE usage_records
    DROP KEY idx_usage_user_created,
    DROP COLUMN user_id;
