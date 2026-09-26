-- Disable the enforcement of foreign-keys constraints
PRAGMA foreign_keys = off;
-- Create "new_items" table
CREATE TABLE `new_items` (`id` integer NULL, `name` text NOT NULL, `stamped` text NOT NULL DEFAULT (datetime('now')), PRIMARY KEY (`id`));
-- Copy rows from old table "items" to new temporary table "new_items"
INSERT INTO `new_items` (`id`, `name`, `stamped`) SELECT `id`, `name`, IFNULL(`stamped`, (datetime('now'))) AS `stamped` FROM `items`;
-- Drop "items" table after copying rows
DROP TABLE `items`;
-- Rename temporary table "new_items" to "items"
ALTER TABLE `new_items` RENAME TO `items`;
-- Enable back the enforcement of foreign-keys constraints
PRAGMA foreign_keys = on;
