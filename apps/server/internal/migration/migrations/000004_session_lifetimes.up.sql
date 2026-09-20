CREATE TABLE IF NOT EXISTS buddy_session_lifetimes (
 user_id VARCHAR(255) NOT NULL,
 session_id VARCHAR(64) NOT NULL,
 deleted TINYINT(1) NOT NULL DEFAULT 0,
 PRIMARY KEY (user_id, session_id)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci;

ALTER TABLE buddy_user_settings ADD COLUMN learner_profile_revision BIGINT NOT NULL DEFAULT 0;
