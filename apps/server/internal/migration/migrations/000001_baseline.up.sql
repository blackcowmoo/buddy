CREATE TABLE IF NOT EXISTS buddy_sessions (
    user_id VARCHAR(255) NOT NULL, id VARCHAR(64) NOT NULL,
    title VARCHAR(255) NOT NULL DEFAULT '', summary TEXT NOT NULL, recent TEXT NOT NULL,
    title_generated TINYINT(1) NOT NULL DEFAULT 0, ended TINYINT(1) NOT NULL DEFAULT 0,
    study_summary TEXT NOT NULL, study_summary_status VARCHAR(16) NOT NULL DEFAULT '',
    quiz TEXT NOT NULL, quiz_status VARCHAR(16) NOT NULL DEFAULT '',
    quiz_completed TINYINT(1) NOT NULL DEFAULT 0, instant TINYINT(1) NOT NULL DEFAULT 0,
    created_at BIGINT NOT NULL, updated_at BIGINT NOT NULL,
    PRIMARY KEY (user_id, id), INDEX idx_user_updated (user_id, updated_at)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci;

CREATE TABLE IF NOT EXISTS buddy_turns (
    user_id VARCHAR(255) NOT NULL, session_id VARCHAR(64) NOT NULL, turn INT NOT NULL,
    role VARCHAR(16) NOT NULL, text TEXT NOT NULL, refined TINYINT(1) NOT NULL DEFAULT 0,
    source VARCHAR(8) NOT NULL DEFAULT '', correction TEXT NULL, translation TEXT NULL,
    meta TEXT NULL, created_at BIGINT NOT NULL,
    PRIMARY KEY (user_id, session_id, turn, role)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci;

CREATE TABLE IF NOT EXISTS buddy_user_settings (
    user_id VARCHAR(255) NOT NULL, interlocutor_style TEXT NOT NULL, learner_profile TEXT NOT NULL,
    word_auto_add_status VARCHAR(16) NOT NULL DEFAULT '', word_auto_add_count INT NOT NULL DEFAULT 0,
    updated_at BIGINT NOT NULL, PRIMARY KEY (user_id)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci;

CREATE TABLE IF NOT EXISTS buddy_jobs (
    user_id VARCHAR(255) NOT NULL, session_id VARCHAR(64) NOT NULL, turn INT NOT NULL,
    kind VARCHAR(24) NOT NULL, status VARCHAR(16) NOT NULL DEFAULT 'pending', error TEXT NULL,
    created_at BIGINT NOT NULL, updated_at BIGINT NOT NULL,
    PRIMARY KEY (user_id, session_id, turn, kind)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci;

CREATE TABLE IF NOT EXISTS buddy_recordings (
    id VARCHAR(64) NOT NULL, user_id VARCHAR(255) NOT NULL, session_id VARCHAR(64) NOT NULL DEFAULT '',
    s3_key VARCHAR(512) NOT NULL, duration_ms INT NOT NULL, size_bytes BIGINT NOT NULL, created_at BIGINT NOT NULL,
    PRIMARY KEY (id), KEY idx_user_created (user_id, created_at), KEY idx_user_session (user_id, session_id)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci;

CREATE TABLE IF NOT EXISTS buddy_tts_cache (
    cache_key VARCHAR(255) NOT NULL, s3_key VARCHAR(512) NOT NULL, version VARCHAR(64) NOT NULL DEFAULT '',
    size_bytes BIGINT NOT NULL, created_at BIGINT NOT NULL, PRIMARY KEY (cache_key), KEY idx_created (created_at)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci;

CREATE TABLE IF NOT EXISTS buddy_writing_prompts (
    id VARCHAR(64) NOT NULL, user_id VARCHAR(255) NOT NULL, korean TEXT NOT NULL,
    status VARCHAR(16) NOT NULL, created_at BIGINT NOT NULL, PRIMARY KEY (id), KEY idx_user_created (user_id, created_at)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci;
