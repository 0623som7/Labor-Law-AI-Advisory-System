-- 会话记住"建它的时候问的是哪份合同"。
--
-- 侧栏标题要显示文件名，但 sessions 里只有一句截断的原话，认不出是哪份合同：
-- 同一个条款反复追问，几条会话长得一模一样。这里把建会话那一刻的 file_id 与
-- 文件名一起存下来。
--
-- file_name 存快照而不是每次 join files：文件可能被改名或删除，而历史会话
-- 必须留着原文（删文件不该让一条咨询记录变成"未命名"）。
--
-- 不建外键：files 行可以被删，会话不能跟着被删。
ALTER TABLE sessions
  ADD COLUMN file_id CHAR(26) NULL AFTER scene,
  ADD COLUMN file_name VARCHAR(255) NOT NULL DEFAULT '' AFTER file_id,
  ADD KEY idx_sessions_file (file_id);
