-- 0001_init.up.sql —— 初始 schema。
--
-- 约定（全库一致）：
--   * 主键 CHAR(26) ULID：带时间前缀，ORDER BY id 即时间序，不暴露自增数量。
--   * 时间列 DATETIME(3) 且一律存 UTC：地方政策有生效日期，毫秒精度够用。
--   * 正文与长文本用 MEDIUMTEXT，绝不放在主表以外的冗余列里做 SELECT *。
--   * 每张表只建有查询支撑的索引；没有查询的索引只带来写放大。

CREATE TABLE IF NOT EXISTS users (
    id            CHAR(26)     NOT NULL,
    phone_masked  VARCHAR(32)  NOT NULL COMMENT '展示用，形如 138****8000',
    phone_hash    CHAR(64)     NOT NULL COMMENT 'SHA256+盐，登录检索用，唯一',
    password_hash VARCHAR(100) NOT NULL COMMENT 'bcrypt cost 12',
    nickname      VARCHAR(64)  NOT NULL DEFAULT '',
    created_at    DATETIME(3)  NOT NULL DEFAULT CURRENT_TIMESTAMP(3),
    updated_at    DATETIME(3)  NOT NULL DEFAULT CURRENT_TIMESTAMP(3) ON UPDATE CURRENT_TIMESTAMP(3),
    deleted_at    DATETIME(3)  NULL,
    PRIMARY KEY (id),
    UNIQUE KEY uk_users_phone_hash (phone_hash)
) ENGINE = InnoDB DEFAULT CHARSET = utf8mb4 COLLATE = utf8mb4_0900_ai_ci COMMENT = '账号';

CREATE TABLE IF NOT EXISTS sessions (
    id         CHAR(26)    NOT NULL,
    user_id    CHAR(26)    NOT NULL,
    title      VARCHAR(100) NOT NULL DEFAULT '',
    scene      VARCHAR(16) NOT NULL DEFAULT 'consult',
    created_at DATETIME(3) NOT NULL DEFAULT CURRENT_TIMESTAMP(3),
    updated_at DATETIME(3) NOT NULL DEFAULT CURRENT_TIMESTAMP(3) ON UPDATE CURRENT_TIMESTAMP(3),
    deleted_at DATETIME(3) NULL,
    PRIMARY KEY (id),
    -- 列表页固定是「我的会话，按最近活跃倒序」，这条索引就是那条查询的形状。
    KEY idx_sessions_user_updated (user_id, updated_at)
) ENGINE = InnoDB DEFAULT CHARSET = utf8mb4 COLLATE = utf8mb4_0900_ai_ci COMMENT = '问答会话';

CREATE TABLE IF NOT EXISTS messages (
    id                CHAR(26)    NOT NULL,
    session_id        CHAR(26)    NOT NULL,
    role              VARCHAR(16) NOT NULL,
    content           MEDIUMTEXT  NOT NULL,
    prompt_tokens     INT         NOT NULL DEFAULT 0,
    completion_tokens INT         NOT NULL DEFAULT 0,
    trace_id          VARCHAR(64) NOT NULL DEFAULT '',
    created_at        DATETIME(3) NOT NULL DEFAULT CURRENT_TIMESTAMP(3),
    PRIMARY KEY (id),
    KEY idx_messages_session_created (session_id, created_at)
) ENGINE = InnoDB DEFAULT CHARSET = utf8mb4 COLLATE = utf8mb4_0900_ai_ci COMMENT = '会话消息';

CREATE TABLE IF NOT EXISTS files (
    id           CHAR(26)     NOT NULL,
    user_id      CHAR(26)     NOT NULL,
    filename     VARCHAR(255) NOT NULL COMMENT '仅展示，禁止拼磁盘路径',
    ext          VARCHAR(16)  NOT NULL,
    size_bytes   BIGINT       NOT NULL DEFAULT 0,
    sha256       CHAR(64)     NOT NULL,
    page_count   INT          NOT NULL DEFAULT 0,
    storage_path VARCHAR(255) NOT NULL DEFAULT '' COMMENT '相对 storage root',
    ocr_used     TINYINT(1)   NOT NULL DEFAULT 0,
    confidence   DOUBLE       NOT NULL DEFAULT 0,
    created_at   DATETIME(3)  NOT NULL DEFAULT CURRENT_TIMESTAMP(3),
    updated_at   DATETIME(3)  NOT NULL DEFAULT CURRENT_TIMESTAMP(3) ON UPDATE CURRENT_TIMESTAMP(3),
    PRIMARY KEY (id),
    -- 同一用户重复上传同一份合同要命中缓存、跳过解析。
    UNIQUE KEY uk_files_user_sha256 (user_id, sha256)
) ENGINE = InnoDB DEFAULT CHARSET = utf8mb4 COLLATE = utf8mb4_0900_ai_ci COMMENT = '上传文件元数据';

CREATE TABLE IF NOT EXISTS file_pages (
    id       CHAR(26) NOT NULL,
    file_id  CHAR(26) NOT NULL,
    page_no  INT      NOT NULL COMMENT '0 起',
    width    DOUBLE   NOT NULL COMMENT 'pt',
    height   DOUBLE   NOT NULL COMMENT 'pt',
    PRIMARY KEY (id),
    UNIQUE KEY uk_pages_file_page (file_id, page_no)
) ENGINE = InnoDB DEFAULT CHARSET = utf8mb4 COLLATE = utf8mb4_0900_ai_ci COMMENT = '页版面';

CREATE TABLE IF NOT EXISTS text_blocks (
    id         CHAR(26)    NOT NULL,
    page_id    CHAR(26)    NOT NULL,
    file_id    CHAR(26)    NOT NULL COMMENT '冗余，用于一次查询取回整份文档',
    page_no    INT         NOT NULL,
    bbox_x0    DOUBLE      NOT NULL COMMENT 'pt，原点左下，y 轴向上',
    bbox_y0    DOUBLE      NOT NULL,
    bbox_x1    DOUBLE      NOT NULL,
    bbox_y1    DOUBLE      NOT NULL,
    text       MEDIUMTEXT  NOT NULL,
    char_start INT         NOT NULL COMMENT '全文连续偏移，跨页累计',
    char_end   INT         NOT NULL,
    PRIMARY KEY (id),
    KEY idx_blocks_file_page (file_id, page_no, char_start)
) ENGINE = InnoDB DEFAULT CHARSET = utf8mb4 COLLATE = utf8mb4_0900_ai_ci COMMENT = '文本块（标注层数据源）';

CREATE TABLE IF NOT EXISTS contract_extracts (
    id         CHAR(26)     NOT NULL,
    file_id    CHAR(26)     NOT NULL,
    payload    JSON         NOT NULL COMMENT '抽取结果，字段由 internal/extract 定义',
    model      VARCHAR(64)  NOT NULL DEFAULT '',
    confidence DOUBLE       NOT NULL DEFAULT 0,
    created_at DATETIME(3)  NOT NULL DEFAULT CURRENT_TIMESTAMP(3),
    PRIMARY KEY (id),
    UNIQUE KEY uk_extracts_file (file_id)
) ENGINE = InnoDB DEFAULT CHARSET = utf8mb4 COLLATE = utf8mb4_0900_ai_ci COMMENT = '合同要素抽取';

CREATE TABLE IF NOT EXISTS reviews (
    id               CHAR(26)     NOT NULL,
    file_id          CHAR(26)     NOT NULL,
    user_id          CHAR(26)     NOT NULL,
    status           VARCHAR(16)  NOT NULL DEFAULT 'pending',
    risk_level       VARCHAR(16)  NOT NULL DEFAULT '',
    score            INT          NOT NULL DEFAULT 0,
    clause_pass_rate DECIMAL(5, 4) NOT NULL DEFAULT 0,
    counts_high      INT          NOT NULL DEFAULT 0,
    counts_medium    INT          NOT NULL DEFAULT 0,
    counts_low       INT          NOT NULL DEFAULT 0,
    task_id          CHAR(26)     NOT NULL DEFAULT '',
    created_at       DATETIME(3)  NOT NULL DEFAULT CURRENT_TIMESTAMP(3),
    updated_at       DATETIME(3)  NOT NULL DEFAULT CURRENT_TIMESTAMP(3) ON UPDATE CURRENT_TIMESTAMP(3),
    PRIMARY KEY (id),
    KEY idx_reviews_user_created (user_id, created_at),
    KEY idx_reviews_file (file_id)
) ENGINE = InnoDB DEFAULT CHARSET = utf8mb4 COLLATE = utf8mb4_0900_ai_ci COMMENT = '审查结果';

CREATE TABLE IF NOT EXISTS findings (
    id              CHAR(26)    NOT NULL,
    review_id       CHAR(26)    NOT NULL,
    rule_id         VARCHAR(32) NOT NULL,
    severity        VARCHAR(16) NOT NULL,
    title           VARCHAR(255) NOT NULL DEFAULT '',
    section         VARCHAR(64) NOT NULL DEFAULT '',
    page_no         INT         NOT NULL DEFAULT 0,
    bbox_x0         DOUBLE      NOT NULL DEFAULT 0,
    bbox_y0         DOUBLE      NOT NULL DEFAULT 0,
    bbox_x1         DOUBLE      NOT NULL DEFAULT 0,
    bbox_y1         DOUBLE      NOT NULL DEFAULT 0,
    evidence        TEXT        NOT NULL,
    legal_basis     JSON        NOT NULL COMMENT '法条依据，正文必须来自库',
    suggestion      JSON        NOT NULL,
    economic_impact DOUBLE      NULL,
    created_at      DATETIME(3) NOT NULL DEFAULT CURRENT_TIMESTAMP(3),
    PRIMARY KEY (id),
    KEY idx_findings_review_severity (review_id, severity)
) ENGINE = InnoDB DEFAULT CHARSET = utf8mb4 COLLATE = utf8mb4_0900_ai_ci COMMENT = '风险发现';

CREATE TABLE IF NOT EXISTS tasks (
    id         CHAR(26)    NOT NULL,
    type       VARCHAR(32) NOT NULL,
    status     VARCHAR(16) NOT NULL DEFAULT 'pending',
    payload    JSON        NOT NULL,
    progress   INT         NOT NULL DEFAULT 0,
    stage      VARCHAR(64) NOT NULL DEFAULT '',
    error      VARCHAR(255) NOT NULL DEFAULT '' COMMENT '只放可展示文案',
    attempt    INT         NOT NULL DEFAULT 0,
    created_at DATETIME(3) NOT NULL DEFAULT CURRENT_TIMESTAMP(3),
    updated_at DATETIME(3) NOT NULL DEFAULT CURRENT_TIMESTAMP(3) ON UPDATE CURRENT_TIMESTAMP(3),
    PRIMARY KEY (id),
    KEY idx_tasks_status_updated (status, updated_at),
    KEY idx_tasks_type_created (type, created_at)
) ENGINE = InnoDB DEFAULT CHARSET = utf8mb4 COLLATE = utf8mb4_0900_ai_ci COMMENT = '异步任务';

CREATE TABLE IF NOT EXISTS statutes (
    id           VARCHAR(32)  NOT NULL COMMENT '业务 ID，如 LCL-38',
    law          VARCHAR(64)  NOT NULL,
    article      VARCHAR(32)  NOT NULL,
    title        VARCHAR(255) NOT NULL DEFAULT '',
    text         MEDIUMTEXT   NOT NULL,
    url          VARCHAR(512) NOT NULL DEFAULT '',
    effective_at DATE         NULL,
    created_at   DATETIME(3)  NOT NULL DEFAULT CURRENT_TIMESTAMP(3),
    PRIMARY KEY (id),
    UNIQUE KEY uk_statutes_law_article (law, article),
    -- 500 条法条的规模用 ngram 全文索引足够，不引 ES / 向量库。
    FULLTEXT KEY ft_statutes_text (text) WITH PARSER ngram
) ENGINE = InnoDB DEFAULT CHARSET = utf8mb4 COLLATE = utf8mb4_0900_ai_ci COMMENT = '法条（只读，全量重建）';

CREATE TABLE IF NOT EXISTS cases (
    id         VARCHAR(32)  NOT NULL,
    title      VARCHAR(255) NOT NULL,
    region     VARCHAR(32)  NOT NULL DEFAULT '',
    summary    MEDIUMTEXT   NOT NULL,
    outcome    VARCHAR(255) NOT NULL DEFAULT '',
    win_rate   DOUBLE       NULL,
    decided_at DATE         NULL,
    created_at DATETIME(3)  NOT NULL DEFAULT CURRENT_TIMESTAMP(3),
    PRIMARY KEY (id),
    KEY idx_cases_region (region),
    FULLTEXT KEY ft_cases_summary (summary) WITH PARSER ngram
) ENGINE = InnoDB DEFAULT CHARSET = utf8mb4 COLLATE = utf8mb4_0900_ai_ci COMMENT = '判例（只读，全量重建）';

CREATE TABLE IF NOT EXISTS embeddings (
    owner_type VARCHAR(16) NOT NULL,
    owner_id   VARCHAR(32) NOT NULL,
    model      VARCHAR(64) NOT NULL,
    vector     JSON        NOT NULL COMMENT 'float32 数组，进程内算余弦',
    created_at DATETIME(3) NOT NULL DEFAULT CURRENT_TIMESTAMP(3),
    PRIMARY KEY (owner_type, owner_id, model)
) ENGINE = InnoDB DEFAULT CHARSET = utf8mb4 COLLATE = utf8mb4_0900_ai_ci COMMENT = '向量（可选，M3 之后再启用）';

CREATE TABLE IF NOT EXISTS usage_records (
    id                BIGINT UNSIGNED NOT NULL AUTO_INCREMENT,
    trace_id          VARCHAR(64)  NOT NULL DEFAULT '',
    scene             VARCHAR(32)  NOT NULL,
    model             VARCHAR(64)  NOT NULL,
    prompt_tokens     INT          NOT NULL DEFAULT 0,
    completion_tokens INT          NOT NULL DEFAULT 0,
    latency_ms        INT          NOT NULL DEFAULT 0,
    cost_est          DECIMAL(12, 6) NOT NULL DEFAULT 0 COMMENT '元',
    created_at        DATETIME(3)  NOT NULL DEFAULT CURRENT_TIMESTAMP(3),
    PRIMARY KEY (id),
    KEY idx_usage_created (created_at),
    KEY idx_usage_scene_created (scene, created_at)
) ENGINE = InnoDB DEFAULT CHARSET = utf8mb4 COLLATE = utf8mb4_0900_ai_ci COMMENT = '模型调用计量（成本闸数据源）';
