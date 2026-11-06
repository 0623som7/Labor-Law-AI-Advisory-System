-- 0002_block_id_width.down.sql —— 把 text_blocks.id 收回 CHAR(26)。
--
-- 警告：down 只在"表里没有任何超过 26 字符的 id"时才会成功。
-- 一旦库里已经写入了 "fileID-页-行" 形式的块 ID（0002 上线后正常产生的数据），
-- 这条 ALTER 会报 1406 Data too long，回滚失败。此时必须先清理这些数据
-- （例如 DELETE 掉对应文件的 text_blocks 并重新解析），再重跑 down。
-- TEXT 列的收缩在严格模式下不会静默截断，这一点是有意依赖的。

ALTER TABLE text_blocks MODIFY COLUMN id CHAR(26) NOT NULL;
