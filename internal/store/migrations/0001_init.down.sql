-- 0001_init.down.sql —— 回滚初始 schema。
-- 顺序与建表相反：先删有外键语义依赖的表，再删被依赖的表。
-- 本库没有建物理外键（ULID 主键 + 应用层保证归属），所以这里的顺序只是为了可读性，
-- 但保持"后建先删"仍然是对的，将来加外键时不用改这份脚本。

DROP TABLE IF EXISTS usage_records;
DROP TABLE IF EXISTS embeddings;
DROP TABLE IF EXISTS cases;
DROP TABLE IF EXISTS statutes;
DROP TABLE IF EXISTS tasks;
DROP TABLE IF EXISTS findings;
DROP TABLE IF EXISTS reviews;
DROP TABLE IF EXISTS contract_extracts;
DROP TABLE IF EXISTS text_blocks;
DROP TABLE IF EXISTS file_pages;
DROP TABLE IF EXISTS files;
DROP TABLE IF EXISTS messages;
DROP TABLE IF EXISTS sessions;
DROP TABLE IF EXISTS users;
