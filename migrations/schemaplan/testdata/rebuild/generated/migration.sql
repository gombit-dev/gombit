-- Disable the enforcement of foreign-keys constraints
PRAGMA foreign_keys = off;
-- Create "new_items" table
CREATE TABLE `new_items` (`id` integer NULL, `name` text NOT NULL DEFAULT 'changed', `price` integer NOT NULL DEFAULT 1, `doubled` integer NULL AS (price * 2) VIRTUAL, PRIMARY KEY (`id`));
-- Copy rows from old table "items" to new temporary table "new_items"
INSERT INTO `new_items` (`id`, `name`, `price`) SELECT `id`, IFNULL(`name`, 'changed') AS `name`, `price` FROM `items`;
-- Drop "items" table after copying rows
DROP TABLE `items`;
-- Rename temporary table "new_items" to "items"
ALTER TABLE `new_items` RENAME TO `items`;
-- Enable back the enforcement of foreign-keys constraints
PRAGMA foreign_keys = on;
