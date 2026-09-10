package store

import (
	"bytes"
	"database/sql"
	"errors"
	"fmt"
	"path/filepath"
	"testing"
	"time"
)

func TestLegacyMigrationPreservesHistoryAndEchoOwnership(t *testing.T) {
	for _, version := range []int{1, 2, 3, 4, 5, 6} {
		t.Run(fmt.Sprintf("version%d", version), func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "legacy.sqlite")
			legacy, err := sql.Open("sqlite", path)
			if err != nil {
				t.Fatal(err)
			}
			_, err = legacy.Exec(`
 CREATE TABLE users (id INTEGER PRIMARY KEY,email TEXT NOT NULL UNIQUE,nick TEXT NOT NULL UNIQUE COLLATE NOCASE,password_salt BLOB NOT NULL,password_hash BLOB NOT NULL,irc_password BLOB NOT NULL,identity_keys BLOB NOT NULL,identity_address TEXT NOT NULL,irc_registered INTEGER NOT NULL DEFAULT 0,created_at INTEGER NOT NULL);
 CREATE TABLE sessions (token_hash BLOB PRIMARY KEY,user_id INTEGER NOT NULL REFERENCES users(id),expires_at INTEGER NOT NULL);
 CREATE TABLE observer (id INTEGER PRIMARY KEY,nick TEXT NOT NULL,identity_keys BLOB NOT NULL,identity_address TEXT NOT NULL);
 CREATE TABLE messages (id INTEGER PRIMARY KEY,room TEXT NOT NULL,nick TEXT NOT NULL,original TEXT NOT NULL,source_language TEXT NOT NULL,wire_language TEXT NOT NULL,service INTEGER NOT NULL DEFAULT 0,created_at INTEGER NOT NULL);
 CREATE TABLE translations (cache_key TEXT PRIMARY KEY,translated TEXT NOT NULL);
 CREATE TABLE send_requests (user_id INTEGER NOT NULL REFERENCES users(id),request_id TEXT NOT NULL,state TEXT NOT NULL,message_id INTEGER NOT NULL DEFAULT 0,room TEXT NOT NULL DEFAULT '',nick TEXT NOT NULL DEFAULT '',original TEXT NOT NULL DEFAULT '',wire_text TEXT NOT NULL DEFAULT '',created_at INTEGER NOT NULL DEFAULT 0,echo_consumed INTEGER NOT NULL DEFAULT 0,PRIMARY KEY(user_id,request_id));
 INSERT INTO users VALUES(1,'alice@example.org','alice',X'01',X'02',X'03',X'04','alice.b32.i2p',1,1000);
 INSERT INTO sessions VALUES(X'AA',1,9999999999999);
 INSERT INTO observer VALUES(1,'observer',X'05','observer.b32.i2p');
 INSERT INTO messages VALUES(7,'#one','alice','original','en','en',0,1000);
 INSERT INTO translations VALUES('cache-key','cached translation');
 INSERT INTO send_requests VALUES(1,'confirmed-request','sent',7,'#one','alice','original','wire',1000,1);
 INSERT INTO send_requests VALUES(1,'interrupted-request','sending',0,'#one','alice','pending','pending',1000,0);
 PRAGMA user_version=1;`)
			if err != nil {
				t.Fatal(errors.Join(err, legacy.Close()))
			}
			if version >= 2 {
				if _, err := legacy.Exec(migration2 + "\nPRAGMA user_version=2;"); err != nil {
					t.Fatal(errors.Join(err, legacy.Close()))
				}
			}
			lastID := int64(7)
			if version >= 3 {
				if _, err := legacy.Exec(migration3 + `
 INSERT INTO messages VALUES(100,'#one','alice','deleted','en','en',0,1000,0,'');
 DELETE FROM messages WHERE id=100;
 INSERT INTO send_requests(user_id,request_id,state,message_id,created_at,echo_consumed,original_mode,error_code,updated_at,expires_at,payload_purged) VALUES(1,'purged-request','failed',100,1000,0,1,'translation_failed',2000,122000,1);
 PRAGMA user_version=3;`); err != nil {
					t.Fatal(errors.Join(err, legacy.Close()))
				}
				lastID = 100
			}
			if version >= 4 {
				if _, err := legacy.Exec(migration4 + "\nPRAGMA user_version=4;"); err != nil {
					t.Fatal(errors.Join(err, legacy.Close()))
				}
			}
			if version >= 5 {
				if _, err := legacy.Exec(migration5 + `
 UPDATE users SET identity_pool=X'0607';
 UPDATE observer SET identity_pool=X'0809';
 PRAGMA user_version=5;`); err != nil {
					t.Fatal(errors.Join(err, legacy.Close()))
				}
			}
			if version >= 6 {
				if _, err := legacy.Exec(migration6 + "\nPRAGMA user_version=6;"); err != nil {
					t.Fatal(errors.Join(err, legacy.Close()))
				}
			}
			if err := validateSchema(t.Context(), legacy, version); err != nil {
				t.Fatal(errors.Join(err, legacy.Close()))
			}
			if err := legacy.Close(); err != nil {
				t.Fatal(err)
			}
			db, q, err := NewSQLite(t.Context(), path)
			if err != nil {
				t.Fatal(err)
			}
			defer func() {
				if err := db.Close(); err != nil {
					t.Error(err)
				}
			}()
			var currentVersion int
			if err := db.QueryRowContext(t.Context(), "PRAGMA user_version").Scan(&currentVersion); err != nil {
				t.Fatal(err)
			}
			if currentVersion != schemaVersion {
				t.Fatalf("database version = %d, want %d", currentVersion, schemaVersion)
			}
			if err := validateSchema(t.Context(), db, currentVersion); err != nil {
				t.Fatal(err)
			}
			var languageColumns int
			if err := db.QueryRowContext(t.Context(), "SELECT COUNT(*) FROM pragma_table_info('messages') WHERE name IN ('source_language','wire_language')").Scan(&languageColumns); err != nil {
				t.Fatal(err)
			}
			if languageColumns != 0 {
				t.Fatalf("detected language columns remain: %d", languageColumns)
			}
			rows, err := q.Messages(t.Context(), MessagesParams{Room: "#one", ID: 8})
			if err != nil {
				t.Fatal(err)
			}
			if len(rows) != 1 {
				t.Fatalf("migrated message count = %d, want 1", len(rows))
			}
			message := rows[0]
			if message.ID != 7 || message.Room != "#one" || message.Nick != "alice" {
				t.Fatalf("message identity changed: %+v", message)
			}
			if message.Original != "original" || message.Service != 0 || message.CreatedAt != 1000 {
				t.Fatalf("message content changed: %+v", message)
			}
			if message.SenderUserID != 1 || message.SenderRequestID != "confirmed-request" {
				t.Fatalf("message echo ownership changed: %+v", message)
			}
			summaries, err := q.RoomSummaries(t.Context(), []string{"#one"}, map[string]int64{"#one": 0}, 0)
			if err != nil || len(summaries) != 1 || summaries[0].LatestMessageID != 7 || summaries[0].Cursor != 0 || summaries[0].UnreadCount != 1 {
				t.Fatalf("migrated room summary: %+v %v", summaries, err)
			}
			confirmed, err := q.GetSend(t.Context(), GetSendParams{UserID: 1, RequestID: "confirmed-request"})
			if err != nil {
				t.Fatal(err)
			}
			if confirmed.State != "confirmed" || confirmed.MessageID != 7 || confirmed.EchoConsumed != 1 {
				t.Fatalf("lost echo confirmation: %+v", confirmed)
			}
			if confirmed.Room != "#one" || confirmed.Nick != "alice" {
				t.Fatalf("send identity changed: %+v", confirmed)
			}
			if confirmed.Original != "original" || confirmed.WireText != "wire" {
				t.Fatalf("send payload changed: %+v", confirmed)
			}
			if confirmed.CreatedAt != 1000 || confirmed.UpdatedAt != 1000 || confirmed.ExpiresAt != 121000 {
				t.Fatalf("send timestamps changed: %+v", confirmed)
			}
			if confirmed.OriginalMode != 0 || confirmed.ErrorCode != "" || confirmed.PayloadPurged != 0 {
				t.Fatalf("send recovery metadata changed: %+v", confirmed)
			}
			pending, err := q.GetSend(t.Context(), GetSendParams{UserID: 1, RequestID: "interrupted-request"})
			if err != nil {
				t.Fatal(err)
			}
			if pending.State != "unconfirmed" || pending.ErrorCode != "interrupted" {
				t.Fatalf("interrupted write misrepresented: %+v", pending)
			}
			if version >= 3 {
				purged, err := q.GetSend(t.Context(), GetSendParams{UserID: 1, RequestID: "purged-request"})
				if err != nil {
					t.Fatal(err)
				}
				if purged.State != "failed" || purged.MessageID != 100 || purged.OriginalMode != 1 {
					t.Fatalf("purged send outcome changed: %+v", purged)
				}
				if purged.CreatedAt != 1000 || purged.UpdatedAt != 2000 || purged.ExpiresAt != 122000 {
					t.Fatalf("purged send timestamps changed: %+v", purged)
				}
				if purged.ErrorCode != "translation_failed" || purged.PayloadPurged != 1 {
					t.Fatalf("purged send recovery metadata changed: %+v", purged)
				}
				claimed, err := q.ClaimSend(t.Context(), ClaimSendParams{UserID: 1, RequestID: "purged-request", State: "translating"})
				if err != nil || claimed != 0 {
					t.Fatalf("purged request reclaimed: count=%d err=%v", claimed, err)
				}
			}
			cached, err := q.GetTranslation(t.Context(), "cache-key")
			if err != nil || cached != "cached translation" {
				t.Fatalf("cache lost: %q %v", cached, err)
			}
			user, err := q.SessionUser(t.Context(), SessionUserParams{TokenHash: []byte{0xaa}, ExpiresAt: 1000})
			if err != nil {
				t.Fatal(err)
			}
			if user.ID != 1 || user.Email != "alice@example.org" || user.Nick != "alice" {
				t.Fatalf("account identity changed: %+v", user)
			}
			if !bytes.Equal(user.PasswordSalt, []byte{1}) || !bytes.Equal(user.PasswordHash, []byte{2}) {
				t.Fatal("account password data changed")
			}
			if !bytes.Equal(user.IrcPassword, []byte{3}) || !bytes.Equal(user.IdentityKeys, []byte{4}) {
				t.Fatal("IRC credentials changed")
			}
			if user.IdentityAddress != "alice.b32.i2p" || user.IrcRegistered != 1 || user.CreatedAt != 1000 {
				t.Fatalf("IRC identity metadata changed: %+v", user)
			}
			observer, err := q.GetObserver(t.Context())
			if err != nil || observer.Nick != "observer" || !bytes.Equal(observer.IdentityKeys, []byte{5}) || observer.IdentityAddress != "observer.b32.i2p" {
				t.Fatalf("observer identity lost: %+v %v", observer, err)
			}
			if version >= 5 {
				if !bytes.Equal(user.IdentityPool, []byte{6, 7}) || !bytes.Equal(observer.IdentityPool, []byte{8, 9}) {
					t.Fatal("migration changed existing destination keys")
				}
			} else if len(user.IdentityPool) != 0 || len(observer.IdentityPool) != 0 {
				t.Fatal("migration invented destination keys")
			}
			retained, err := q.Prune(t.Context(), time.Now(), RetentionPolicy{Translations: time.Hour, BatchSize: 1})
			if err != nil || retained.TranslationsDeleted != 0 {
				t.Fatalf("migrated cache prematurely pruned: %+v %v", retained, err)
			}
			if _, err := db.ExecContext(t.Context(), "DELETE FROM messages"); err != nil {
				t.Fatal(err)
			}
			message, err = q.AddMessage(t.Context(), AddMessageParams{Room: "#one", Nick: "alice", Original: "after migration", CreatedAt: 2000})
			if err != nil {
				t.Fatal(err)
			}
			if message.ID <= lastID {
				t.Fatalf("message cursor regressed: new=%d deleted=%d", message.ID, lastID)
			}
		})
	}
}

func TestExpiredSendRemainsActionableWithoutReleasingIdempotencyClaim(t *testing.T) {
	db, q, err := NewSQLite(t.Context(), filepath.Join(t.TempDir(), "chat.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := db.Close(); err != nil {
			t.Error(err)
		}
	}()
	user, err := q.CreateUser(t.Context(), CreateUserParams{Email: "alice@example.org", Nick: "alice", PasswordSalt: []byte{1}, PasswordHash: []byte{1}, IrcPassword: []byte{1}, IdentityKeys: []byte{1}, IdentityAddress: "alice.b32.i2p"})
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UnixMilli()
	claim := ClaimSendParams{UserID: user.ID, RequestID: "expired-request", Room: "#one", Nick: "alice", Original: "hello", State: "sending", CreatedAt: now - 120000, UpdatedAt: now - 120000, ExpiresAt: now}
	if _, err := q.ClaimSend(t.Context(), claim); err != nil {
		t.Fatal(err)
	}
	if _, err := q.PrepareSend(t.Context(), PrepareSendParams{WireText: "hello", UpdatedAt: now - 120000, UserID: user.ID, RequestID: claim.RequestID}); err != nil {
		t.Fatal(err)
	}
	if _, err := q.FinishSend(t.Context(), FinishSendParams{State: "awaiting_echo", UpdatedAt: now - 120000, ExpiresAt: now, UserID: user.ID, RequestID: claim.RequestID}); err != nil {
		t.Fatal(err)
	}
	expired, err := q.ExpireEchoes(t.Context(), ExpireEchoesParams{UpdatedAt: now, ExpiresAt: now})
	if err != nil {
		t.Fatal(err)
	}
	if len(expired) != 1 || expired[0].State != "unconfirmed" || expired[0].ErrorCode != "echo_timeout" || expired[0].ExpiresAt > now {
		t.Fatalf("expiry transition: %+v", expired)
	}
	visible, err := q.ListSends(t.Context(), ListSendsParams{UserID: user.ID, Room: "#one", ExpiresAt: now})
	if err != nil {
		t.Fatal(err)
	}
	if len(visible) != 1 || visible[0].State != "unconfirmed" {
		t.Fatalf("expired outgoing is not actionable: %+v", visible)
	}
	count, err := q.ClaimSend(t.Context(), claim)
	if err != nil || count != 0 {
		t.Fatalf("expired request can resend: claimed=%d err=%v", count, err)
	}
	echo, err := q.PendingEcho(t.Context(), PendingEchoParams{Room: "#one", Nick: "alice", WireText: "hello", CreatedAt: now - 1200000})
	if err != nil || echo.RequestID != claim.RequestID {
		t.Fatalf("late echo association unavailable: %+v %v", echo, err)
	}
	if _, err := q.ConsumeEcho(t.Context(), ConsumeEchoParams{MessageID: 9, UpdatedAt: now + 1, UserID: user.ID, RequestID: claim.RequestID}); err != nil {
		t.Fatal(err)
	}
	visible, err = q.ListSends(t.Context(), ListSendsParams{UserID: user.ID, Room: "#one", ExpiresAt: now + 1})
	if err != nil || len(visible) != 1 || visible[0].State != "confirmed" {
		t.Fatalf("real late echo hidden: %+v %v", visible, err)
	}
}
