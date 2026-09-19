CREATE TABLE buddy_nuance_lessons (
 id VARCHAR(64) NOT NULL PRIMARY KEY,
 user_id VARCHAR(255) NOT NULL,
 status VARCHAR(16) NOT NULL,
 created_at BIGINT NOT NULL,
 revision INT NOT NULL DEFAULT 0,
 content_json JSON NULL,
 state_json JSON NOT NULL,
 KEY idx_user_created (user_id, created_at)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci;

CREATE TABLE buddy_nuance_attempts (
 lesson_id VARCHAR(64) NOT NULL,
 revision INT NOT NULL,
 question_id VARCHAR(64) NOT NULL,
 selected_word VARCHAR(255) NOT NULL,
 correct BOOLEAN NOT NULL,
 repeat_review BOOLEAN NOT NULL DEFAULT FALSE,
 reviewed_at BIGINT NOT NULL,
 PRIMARY KEY (lesson_id, revision),
 FOREIGN KEY (lesson_id) REFERENCES buddy_nuance_lessons(id) ON DELETE CASCADE
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci;
