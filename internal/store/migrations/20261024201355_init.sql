CREATE TABLE `users` (`id` integer PRIMARY KEY AUTOINCREMENT,`username` text NOT NULL,`password_hash` text NOT NULL,`role` text NOT NULL DEFAULT "user",`disabled` numeric NOT NULL DEFAULT false,`session_epoch` integer NOT NULL DEFAULT 0,`created_at` datetime,`updated_at` datetime);
CREATE UNIQUE INDEX `idx_users_username` ON `users`(`username`);

