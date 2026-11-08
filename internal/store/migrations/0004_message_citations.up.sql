-- 引用法条存 JSON 而非单独建表：引用是消息的附属属性，没有独立查询需求，
-- 单独建表会让"取一页消息"变成 N+1 次查询。
--
-- 背景：当前这一轮咨询有 3 张法条卡片，一刷新就全没了——messages 表里只有正文，
-- 前端只能承认"库里没有存引用，硬补就是编造"。答辩演示"关掉页面再打开"时，
-- 引用卡片消失无法解释。
--
-- NULL 表示没有引用（user 消息恒为 NULL）：用 NULL 而不是空数组 '[]'，
-- 是为了让 SUM(citations IS NULL) 能直接答"有多少条消息没有引用"，
-- 也让老数据在迁移后天然落在正确语义上。
ALTER TABLE messages
  ADD COLUMN citations MEDIUMTEXT NULL
    COMMENT '引用法条 JSON 数组，仅 assistant 消息，无引用时为 NULL';
