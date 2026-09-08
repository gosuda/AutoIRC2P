-- name: CreateUser :one
INSERT INTO users (email,nick,password_salt,password_hash,irc_password,identity_keys,identity_address,created_at) VALUES (?,?,?,?,?,?,?,?) RETURNING *;
-- name: UserByEmail :one
SELECT * FROM users WHERE email = ?;
-- name: UserByID :one
SELECT * FROM users WHERE id = ?;
-- name: RegisterIRC :exec
UPDATE users SET irc_registered = 1 WHERE id = ?;
-- name: UserIdentityPool :one
SELECT identity_pool FROM users WHERE id = ?;
-- name: InitializeUserIdentityPool :one
UPDATE users SET identity_pool = CASE WHEN length(identity_pool) = 0 THEN sqlc.arg(identity_pool) ELSE identity_pool END WHERE id = sqlc.arg(id) RETURNING identity_pool;
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
INSERT INTO observer (id,nick,identity_keys,identity_address) VALUES (1,?,?,?) ON CONFLICT(id) DO NOTHING;
-- name: InitializeObserverIdentityPool :one
UPDATE observer SET identity_pool = CASE WHEN length(identity_pool) = 0 THEN sqlc.arg(identity_pool) ELSE identity_pool END WHERE id = 1 RETURNING identity_pool;
-- name: AddMessage :one
INSERT INTO messages (room,nick,original,service,created_at,sender_user_id,sender_request_id) VALUES (?,?,?,?,?,?,?) RETURNING *;
-- name: Messages :many
SELECT * FROM messages WHERE room = ? AND id < ? ORDER BY id DESC LIMIT 100;
-- name: CachedTranslation :one
SELECT translated FROM translations WHERE cache_key = ?;
-- name: SaveTranslation :exec
INSERT INTO translations (cache_key,translated,created_at) VALUES (?,?,CAST(unixepoch('subsec') * 1000 AS INTEGER)) ON CONFLICT(cache_key) DO UPDATE SET translated = excluded.translated, created_at = excluded.created_at;
-- name: ClaimSend :execrows
INSERT INTO send_requests (user_id,request_id,room,nick,original,original_mode,state,created_at,updated_at,expires_at) VALUES (?,?,?,?,?,?,?,?,?,?) ON CONFLICT DO NOTHING;
-- name: GetSend :one
SELECT * FROM send_requests WHERE user_id = ? AND request_id = ?;
-- name: ListSends :many
SELECT * FROM send_requests WHERE user_id = ? AND room = ? AND payload_purged = 0 AND (state = 'confirmed' OR expires_at > ?) ORDER BY created_at DESC,rowid DESC LIMIT 50;
-- name: FinishSend :one
UPDATE send_requests SET state = ?, error_code = ?, updated_at = ?, expires_at = ? WHERE user_id = ? AND request_id = ? AND state IN ('translating','sending') RETURNING *;
-- name: PrepareSend :one
UPDATE send_requests SET wire_text = ?, state = 'sending', updated_at = ? WHERE user_id = ? AND request_id = ? AND state IN ('translating','sending') RETURNING *;
-- name: PendingEcho :one
SELECT * FROM send_requests WHERE room = ? AND nick = ? COLLATE NOCASE AND wire_text = ? AND payload_purged = 0 AND echo_consumed = 0 AND state IN ('sending','awaiting_echo','unconfirmed') AND created_at >= ? ORDER BY created_at,rowid LIMIT 1;
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
-- name: PruneMessagesBatch :execrows
DELETE FROM messages WHERE id IN (SELECT m.id FROM messages AS m WHERE m.created_at < ? ORDER BY m.created_at LIMIT ?);
-- name: PruneTranslationsBatch :execrows
DELETE FROM translations WHERE cache_key IN (SELECT t.cache_key FROM translations AS t WHERE t.created_at < ? ORDER BY t.created_at LIMIT ?);
-- name: PurgeSendPayloadsBatch :execrows
UPDATE send_requests SET payload_purged = 1, room = '', nick = '', original = '', wire_text = '' WHERE rowid IN (SELECT s.rowid FROM send_requests AS s WHERE s.payload_purged = 0 AND s.state IN ('confirmed','failed','unconfirmed') AND s.updated_at < ? AND s.created_at < ? ORDER BY s.updated_at LIMIT ?);
-- name: BackupUserSecrets :many
SELECT id,email,irc_password,identity_keys FROM users WHERE id > ? ORDER BY id LIMIT 100;
-- name: BackupObserverSecrets :one
SELECT identity_keys FROM observer WHERE id = 1;
-- name: BackupUserIdentityPools :many
SELECT id,email,identity_pool FROM users WHERE id > ? ORDER BY id LIMIT 100;
-- name: BackupObserverIdentityPool :one
SELECT identity_pool FROM observer WHERE id = 1;
