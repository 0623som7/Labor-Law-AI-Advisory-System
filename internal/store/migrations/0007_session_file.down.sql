-- 回滚 0007：文件信息只是标题用的快照，删掉不影响会话与消息本身。
ALTER TABLE sessions
  DROP KEY idx_sessions_file,
  DROP COLUMN file_name,
  DROP COLUMN file_id;
