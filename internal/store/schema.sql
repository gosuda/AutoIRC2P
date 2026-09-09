CREATE TABLE users (
 id INTEGER PRIMARY KEY,
 email TEXT NOT NULL UNIQUE,
 nick TEXT NOT NULL UNIQUE COLLATE NOCASE,
 password_salt BLOB NOT NULL,
 password_hash BLOB NOT NULL,
 irc_password BLOB NOT NULL,
 identity_keys BLOB NOT NULL,
 identity_address TEXT NOT NULL,
 identity_pool BLOB NOT NULL DEFAULT X'',
 irc_registered INTEGER NOT NULL DEFAULT 0,
 created_at INTEGER NOT NULL
);
CREATE TABLE sessions (
 token_hash BLOB PRIMARY KEY,
 user_id INTEGER NOT NULL REFERENCES users(id) ON DELETE CASCADE,
 expires_at INTEGER NOT NULL
);
CREATE INDEX sessions_expiry ON sessions(expires_at);
CREATE TABLE observer (
 id INTEGER PRIMARY KEY CHECK (id = 1),
 nick TEXT NOT NULL,
 identity_keys BLOB NOT NULL,
 identity_address TEXT NOT NULL,
 identity_pool BLOB NOT NULL DEFAULT X''
);
CREATE TABLE messages (
 id INTEGER PRIMARY KEY AUTOINCREMENT,
 room TEXT NOT NULL,
 nick TEXT NOT NULL,
 original TEXT NOT NULL,
 service INTEGER NOT NULL DEFAULT 0,
 created_at INTEGER NOT NULL
 ,sender_user_id INTEGER NOT NULL DEFAULT 0
 ,sender_request_id TEXT NOT NULL DEFAULT ''
);
CREATE INDEX messages_room_id ON messages(room, id DESC);
CREATE INDEX messages_unread ON messages(room,id,sender_user_id) WHERE service = 0;
CREATE TABLE translations (
 cache_key TEXT PRIMARY KEY,
 translated TEXT NOT NULL,
 created_at INTEGER NOT NULL
);
CREATE TABLE send_requests (
 user_id INTEGER NOT NULL REFERENCES users(id) ON DELETE CASCADE,
 request_id TEXT NOT NULL,
 state TEXT NOT NULL,
 message_id INTEGER NOT NULL DEFAULT 0,
 room TEXT NOT NULL DEFAULT '',
 nick TEXT NOT NULL DEFAULT '',
 original TEXT NOT NULL DEFAULT '',
 wire_text TEXT NOT NULL DEFAULT '',
 created_at INTEGER NOT NULL DEFAULT 0,
 echo_consumed INTEGER NOT NULL DEFAULT 0,
 original_mode INTEGER NOT NULL DEFAULT 0,
 error_code TEXT NOT NULL DEFAULT '',
 updated_at INTEGER NOT NULL DEFAULT 0,
 expires_at INTEGER NOT NULL DEFAULT 0,
 payload_purged INTEGER NOT NULL DEFAULT 0,
 PRIMARY KEY (user_id, request_id)
);
CREATE INDEX send_requests_echo ON send_requests(room,nick COLLATE NOCASE,wire_text,created_at);
CREATE INDEX send_requests_user_room ON send_requests(user_id,room,created_at DESC);
CREATE INDEX messages_retention ON messages(created_at);
CREATE INDEX translations_retention ON translations(created_at);
CREATE INDEX send_requests_retention ON send_requests(updated_at) WHERE payload_purged = 0 AND state IN ('confirmed','failed','unconfirmed');
