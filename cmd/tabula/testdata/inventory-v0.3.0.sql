-- Tabula inventory compatibility fixture

-- labels_view
-- list.count
SELECT count(*) FROM (SELECT id FROM labels) AS tabula_count;
-- list.find
SELECT id FROM labels
ORDER BY id DESC;
-- list.page
SELECT id FROM labels
ORDER BY id DESC LIMIT $1 OFFSET $2;

-- notes_view
-- list.count
SELECT count(*) FROM (SELECT id FROM notes) AS tabula_count;
-- list.extra
SELECT id FROM archived_notes;
-- list.find
SELECT id FROM notes
ORDER BY id DESC;
-- list.page
SELECT id FROM notes
ORDER BY id DESC LIMIT $1 OFFSET $2;
-- list.recent.count
SELECT count(*) FROM (SELECT id FROM recent_notes) AS tabula_count;
-- list.recent.find
SELECT id FROM recent_notes
ORDER BY id DESC;
-- list.recent.page
SELECT id FROM recent_notes
ORDER BY id DESC LIMIT $1 OFFSET $2;

-- records
-- count
SELECT count(*) FROM records WHERE true;
-- count_by_category
SELECT category, count(*) FROM records WHERE true GROUP BY category;
-- get
SELECT id, category FROM records WHERE id = $1;
-- list
SELECT id, category FROM records WHERE true;
