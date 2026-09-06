ALTER TABLE translations ADD COLUMN created_at INTEGER NOT NULL DEFAULT 0;
UPDATE translations SET created_at = CAST(unixepoch('subsec') * 1000 AS INTEGER);
ALTER TABLE send_requests ADD COLUMN payload_purged INTEGER NOT NULL DEFAULT 0;
CREATE TABLE messages_v3 (
 id INTEGER PRIMARY KEY AUTOINCREMENT,
 room TEXT NOT NULL,
 nick TEXT NOT NULL,
 original TEXT NOT NULL,
 source_language TEXT NOT NULL,
 wire_language TEXT NOT NULL,
 service INTEGER NOT NULL DEFAULT 0,
 created_at INTEGER NOT NULL,
 sender_user_id INTEGER NOT NULL DEFAULT 0,
 sender_request_id TEXT NOT NULL DEFAULT ''
);
INSERT INTO messages_v3 SELECT id,room,nick,original,source_language,wire_language,service,created_at,sender_user_id,sender_request_id FROM messages;
DROP TABLE messages;
ALTER TABLE messages_v3 RENAME TO messages;
CREATE INDEX messages_room_id ON messages(room,id DESC);
CREATE INDEX messages_retention ON messages(created_at);
CREATE INDEX translations_retention ON translations(created_at);
CREATE INDEX send_requests_retention ON send_requests(updated_at) WHERE payload_purged = 0 AND state IN ('confirmed','failed','unconfirmed');
