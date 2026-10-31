CREATE TABLE buddy_nuance_reviews (
 user_id VARCHAR(255) NOT NULL PRIMARY KEY,
 items_json JSON NOT NULL
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci;
