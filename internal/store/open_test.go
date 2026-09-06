package store

import (
	"database/sql"
	"errors"
	"path/filepath"
	"testing"
	"time"
)

func TestVersionOneMigrationPreservesHistoryAndEchoOwnership(t *testing.T) {
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
	rows, err := q.Messages(t.Context(), MessagesParams{Room: "#one", ID: 8})
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 || rows[0].ID != 7 || rows[0].Original != "original" || rows[0].SenderUserID != 1 || rows[0].SenderRequestID != "confirmed-request" {
		t.Fatalf("migrated history: %+v", rows)
	}
	confirmed, err := q.GetSend(t.Context(), GetSendParams{UserID: 1, RequestID: "confirmed-request"})
	if err != nil {
		t.Fatal(err)
	}
	if confirmed.State != "confirmed" || confirmed.MessageID != 7 {
		t.Fatalf("lost echo confirmation: %+v", confirmed)
	}
	pending, err := q.GetSend(t.Context(), GetSendParams{UserID: 1, RequestID: "interrupted-request"})
	if err != nil {
		t.Fatal(err)
	}
	if pending.State != "unconfirmed" || pending.ErrorCode != "interrupted" {
		t.Fatalf("interrupted write misrepresented: %+v", pending)
	}
	cached, err := q.GetTranslation(t.Context(), "cache-key")
	if err != nil || cached != "cached translation" {
		t.Fatalf("cache lost: %q %v", cached, err)
	}
	user, err := q.SessionUser(t.Context(), SessionUserParams{TokenHash: []byte{0xaa}, ExpiresAt: 1000})
	if err != nil || user.Email != "alice@example.org" {
		t.Fatalf("session lost: %+v %v", user, err)
	}
	observer, err := q.GetObserver(t.Context())
	if err != nil || observer.IdentityAddress != "observer.b32.i2p" {
		t.Fatalf("observer identity lost: %+v %v", observer, err)
	}
}

func TestExpiredSendDisappearsWithoutReleasingIdempotencyClaim(t *testing.T) {
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
	if len(visible) != 0 {
		t.Fatalf("expired outgoing still visible: %+v", visible)
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
