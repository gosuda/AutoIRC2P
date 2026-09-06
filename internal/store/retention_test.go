package store

import (
	"database/sql"
	"errors"
	"path/filepath"
	"testing"
	"time"
)

func TestRetentionPreservesReplayProtectionAndMessageSequence(t *testing.T) {
	db, q, err := NewSQLite(t.Context(), filepath.Join(t.TempDir(), "chat.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := db.Close(); err != nil {
			t.Error(err)
		}
	})
	now := time.Now()
	old := now.Add(-48 * time.Hour).UnixMilli()
	user, err := q.CreateUser(t.Context(), CreateUserParams{Email: "alice@example.org", Nick: "alice", PasswordSalt: []byte{1}, PasswordHash: []byte{1}, IrcPassword: []byte{1}, IdentityKeys: []byte{1}, IdentityAddress: "alice.b32.i2p"})
	if err != nil {
		t.Fatal(err)
	}
	var lastID int64
	for _, state := range []string{"confirmed", "failed", "unconfirmed", "translating", "sending", "awaiting_echo"} {
		message, err := q.AddMessage(t.Context(), AddMessageParams{Room: "#one", Nick: "alice", Original: "old payload", CreatedAt: old})
		if err != nil {
			t.Fatal(err)
		}
		lastID = message.ID
		if _, err := q.ClaimSend(t.Context(), ClaimSendParams{UserID: user.ID, RequestID: state, State: state, Room: "#one", Nick: "alice", Original: "old payload", CreatedAt: old, UpdatedAt: old, ExpiresAt: old}); err != nil {
			t.Fatal(err)
		}
	}
	if err := q.PutTranslation(t.Context(), "fresh", "recent translation"); err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(t.Context(), "INSERT INTO translations(cache_key,translated,created_at) VALUES('old','old translation',?)", old); err != nil {
		t.Fatal(err)
	}
	result, err := q.Prune(t.Context(), now, RetentionPolicy{Messages: 24 * time.Hour, Translations: 24 * time.Hour, SendPayloads: 24 * time.Hour, BatchSize: 2})
	if err != nil {
		t.Fatal(err)
	}
	if result.MessagesDeleted != 6 || result.TranslationsDeleted != 1 || result.SendPayloadsPurged != 3 {
		t.Fatalf("pruned counts: %+v", result)
	}
	for _, state := range []string{"confirmed", "failed", "unconfirmed"} {
		request, err := q.GetSend(t.Context(), GetSendParams{UserID: user.ID, RequestID: state})
		if err != nil {
			t.Fatal(err)
		}
		if request.PayloadPurged != 1 || request.Original != "" || request.WireText != "" || request.Room != "" || request.Nick != "" {
			t.Fatalf("payload survived purge: %+v", request)
		}
		count, err := q.ClaimSend(t.Context(), ClaimSendParams{UserID: user.ID, RequestID: state, State: "translating"})
		if err != nil || count != 0 {
			t.Fatalf("purged request reclaimed: count=%d err=%v", count, err)
		}
	}
	for _, state := range []string{"translating", "sending", "awaiting_echo"} {
		request, err := q.GetSend(t.Context(), GetSendParams{UserID: user.ID, RequestID: state})
		if err != nil || request.PayloadPurged != 0 || request.Original != "old payload" {
			t.Fatalf("active request purged: %+v %v", request, err)
		}
	}
	hidden, err := q.ListSends(t.Context(), ListSendsParams{UserID: user.ID, Room: "", ExpiresAt: now.UnixMilli()})
	if err != nil || len(hidden) != 0 {
		t.Fatalf("purged statuses leaked: %+v %v", hidden, err)
	}
	_, err = q.PendingEcho(t.Context(), PendingEchoParams{Room: "", Nick: "", WireText: "", CreatedAt: old})
	if !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("purged payload matched echo: %v", err)
	}
	if value, err := q.GetTranslation(t.Context(), "fresh"); err != nil || value != "recent translation" {
		t.Fatalf("fresh translation lost: %q %v", value, err)
	}
	if _, err := q.GetTranslation(t.Context(), "old"); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("old cache survived: %v", err)
	}
	message, err := q.AddMessage(t.Context(), AddMessageParams{Room: "#one", Nick: "alice", Original: "new payload", CreatedAt: now.UnixMilli()})
	if err != nil {
		t.Fatal(err)
	}
	if message.ID <= lastID {
		t.Fatalf("message cursor regressed: new=%d deleted=%d", message.ID, lastID)
	}
}

func TestPruneRejectsUnsafePolicyBeforeDeleting(t *testing.T) {
	db, q, err := NewSQLite(t.Context(), filepath.Join(t.TempDir(), "chat.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := db.Close(); err != nil {
			t.Error(err)
		}
	})
	message, err := q.AddMessage(t.Context(), AddMessageParams{Room: "#one", Original: "keep", CreatedAt: 1})
	if err != nil {
		t.Fatal(err)
	}
	_, err = q.Prune(t.Context(), time.Now(), RetentionPolicy{Messages: time.Hour, SendPayloads: time.Minute})
	if err == nil {
		t.Fatal("short late-echo protection accepted")
	}
	rows, err := q.Messages(t.Context(), MessagesParams{Room: "#one", ID: message.ID + 1})
	if err != nil || len(rows) != 1 || rows[0].Original != "keep" {
		t.Fatalf("invalid policy deleted data: %+v %v", rows, err)
	}
}
