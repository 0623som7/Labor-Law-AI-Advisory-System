-- 0002_block_id_width.up.sql —— 放宽 text_blocks.id 的宽度。
--
-- 背景：解析层给文本块生成的 ID 是 "fileID-页-行"（见 internal/file/extractor.go），
-- fileID 是 26 字符 ULID，拼上 "-0-0" 之后是 28~34 字符，装不进 0001 里定的 CHAR(26)；
-- MySQL 在 STRICT_TRANS_TABLES 下直接报 1406 Data too long，导致第一次上传合同必定 500，
-- 而 files 行已经写进去了（SavePages 的事务只回滚了页面与文本块）。
--
-- 为什么是 VARCHAR(40) 而不是把 ID 换成 ULID：B 靠 "fileID-页-行" 反推位置，
-- 换 ID 格式等于把这条链路剪断。40 给页号与行号各留了足够的十进制位数。
-- 为什么新增迁移而不是改 0001：已上线的迁移是历史，改它会让已建库与新建库分叉。

ALTER TABLE text_blocks MODIFY COLUMN id VARCHAR(40) NOT NULL;
