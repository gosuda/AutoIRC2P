-- name: CreateUser :one
INSERT INTO users (email,nick,password_salt,password_hash,irc_password,identity_keys,identity_address,created_at) VALUES (?,?,?,?,?,?,?,?) RETURNING *;
-- name: UserByEmail :one
SELECT * FROM users WHERE email = ?;
-- name: UserByID :one
SELECT * FROM users WHERE id = ?;
-- name: RegisterIRC :exec
UPDATE users SET irc_registered = 1 WHERE id = ?;
-- name: CreateSession :exec
INSERT INTO sessions (token_hash,user_id,expires_at) VALUES (?,?,?);
-- name: SessionUser :one
SELECT users.* FROM users JOIN sessions ON users.id = sessions.user_id WHERE sessions.token_hash = ? AND sessions.expires_at > ?;
-- name: DeleteSession :exec
DELETE FROM sessions WHERE token_hash = ?;
-- name: PruneSessions :exec
DELETE FROM sessions WHERE expires_at <= ?;
-- name: GetObserver :one
SELECT * FROM observer WHERE id = 1;
-- name: CreateObserver :exec
INSERT INTO observer (id,nick,identity_keys,identity_address) VALUES (1,?,?,?);
-- name: AddMessage :one
INSERT INTO messages (room,nick,original,source_language,wire_language,service,created_at,sender_user_id,sender_request_id) VALUES (?,?,?,?,?,?,?,?,?) RETURNING *;
-- name: Messages :many
SELECT * FROM messages WHERE room = ? AND id < ? ORDER BY id DESC LIMIT 100;
-- name: RoomLanguages :many
SELECT wire_language FROM messages WHERE room = ? AND service = 0 ORDER BY id DESC LIMIT 100;
-- name: CachedTranslation :one
SELECT translated FROM translations WHERE cache_key = ?;
-- name: SaveTranslation :exec
INSERT INTO translations (cache_key,translated) VALUES (?,?) ON CONFLICT(cache_key) DO UPDATE SET translated = excluded.translated;
-- name: ClaimSend :execrows
INSERT INTO send_requests (user_id,request_id,room,nick,original,original_mode,state,created_at,updated_at,expires_at) VALUES (?,?,?,?,?,?,?,?,?,?) ON CONFLICT DO NOTHING;
-- name: GetSend :one
SELECT * FROM send_requests WHERE user_id = ? AND request_id = ?;
-- name: ListSends :many
SELECT * FROM send_requests WHERE user_id = ? AND room = ? AND (state = 'confirmed' OR expires_at > ?) ORDER BY created_at DESC,rowid DESC LIMIT 50;
-- name: FinishSend :one
UPDATE send_requests SET state = ?, error_code = ?, updated_at = ?, expires_at = ? WHERE user_id = ? AND request_id = ? AND state IN ('translating','sending') RETURNING *;
-- name: PrepareSend :one
UPDATE send_requests SET wire_text = ?, state = 'sending', updated_at = ? WHERE user_id = ? AND request_id = ? AND state IN ('translating','sending') RETURNING *;
-- name: PendingEcho :one
SELECT * FROM send_requests WHERE room = ? AND nick = ? COLLATE NOCASE AND wire_text = ? AND echo_consumed = 0 AND state IN ('sending','awaiting_echo','unconfirmed') AND created_at >= ? ORDER BY created_at,rowid LIMIT 1;
-- name: ConsumeEcho :one
UPDATE send_requests SET echo_consumed = 1, message_id = ?, state = 'confirmed', error_code = '', updated_at = ? WHERE user_id = ? AND request_id = ? RETURNING *;
-- name: ExpireEchoes :many
UPDATE send_requests SET state = 'unconfirmed', error_code = 'echo_timeout', updated_at = ? WHERE state = 'awaiting_echo' AND expires_at <= ? RETURNING *;
-- name: RecoverSends :exec
UPDATE send_requests SET state = CASE WHEN state = 'translating' THEN 'failed' ELSE 'unconfirmed' END, error_code = 'interrupted' WHERE state IN ('translating','sending','awaiting_echo');
-- name: LatestRoomMessage :one
SELECT CAST(COALESCE(MAX(id),0) AS INTEGER) FROM messages WHERE room = ?;
-- name: BoundReadCursor :one
SELECT CAST(COALESCE(MAX(id),0) AS INTEGER) FROM messages WHERE room = ? AND id <= ?;
-- name: UnreadMessages :one
SELECT COUNT(*) FROM messages WHERE room = ? AND id > ? AND service = 0 AND (sender_user_id = 0 OR sender_user_id != ?);
