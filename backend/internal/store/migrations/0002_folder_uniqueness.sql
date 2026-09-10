-- Fix folder name uniqueness.
--
-- The table's UNIQUE(user_id, parent_folder_id, name) constraint does not
-- actually constrain anything in v1. SQLite treats NULLs as distinct from one
-- another in a UNIQUE index, and parent_folder_id is always NULL while folders
-- are flat (decision D5) — so every root folder looked unique regardless of
-- name, and duplicates inserted silently.
--
-- COALESCE folds the NULL into a real value so the index bites, and lower()
-- makes it case-insensitive: "Trips" and "trips" sitting side by side in the
-- sidebar is a bug report waiting to happen. The expression keeps working
-- unchanged when nesting arrives in v2.

CREATE UNIQUE INDEX idx_folders_unique_name
  ON folders(user_id, COALESCE(parent_folder_id, 0), lower(name));
