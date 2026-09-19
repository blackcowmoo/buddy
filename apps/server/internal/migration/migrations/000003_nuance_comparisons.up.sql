CREATE TABLE buddy_nuance_comparisons (
 user_id VARCHAR(255) NOT NULL,
 comparison_key BINARY(32) NOT NULL,
 lesson_id VARCHAR(64) NOT NULL,
 PRIMARY KEY (user_id, comparison_key),
 UNIQUE KEY idx_lesson (lesson_id),
 FOREIGN KEY (lesson_id) REFERENCES buddy_nuance_lessons(id) ON DELETE CASCADE
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci;
