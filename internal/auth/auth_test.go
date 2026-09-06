package auth

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"path/filepath"
	"testing"

	"github.com/gosuda/AutoIRC2P/internal/store"
)

func TestRegistrationKeepsSaltStableAndSecretsPrivate(t *testing.T) {
	ctx := context.Background()
	db, q, err := store.NewSQLite(ctx, filepath.Join(t.TempDir(), "chat.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := db.Close(); err != nil {
			t.Error(err)
		}
	})
	service, err := New(q, bytes.Repeat([]byte{7}, 32))
	if err != nil {
		t.Fatal(err)
	}
	missingSalt := service.Salt(" Person@example.org ")
	proofBytes := sha256.Sum256([]byte("browser stretched proof"))
	proof := hex.EncodeToString(proofBytes[:])
	user, err := service.Register(ctx, "person@example.org", "ordinaryNick", proof)
	if err != nil {
		t.Fatal(err)
	}
	if got := service.Salt("person@example.org"); got != missingSalt {
		t.Fatalf("registration changed public salt: %s != %s", got, missingSalt)
	}
	if len(service.Salt("missing@example.org")) != len(missingSalt) {
		t.Fatal("missing account salt shape reveals existence")
	}
	if bytes.Equal(user.PasswordHash, proofBytes[:]) {
		t.Fatal("stored browser credential without server hashing")
	}
	account, err := service.Account(user)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Equal(user.IdentityKeys, account.Identity.Keys) || bytes.Equal(user.IrcPassword, []byte(account.Password)) {
		t.Fatal("IRC credentials stored unencrypted")
	}
	if account.Email == user.Email {
		t.Fatal("application email exposed to IRC")
	}
	if _, err := service.Login(ctx, "PERSON@example.org", proof); err != nil {
		t.Fatal(err)
	}
	bad := hex.EncodeToString(bytes.Repeat([]byte{9}, 32))
	for _, email := range []string{"person@example.org", "missing@example.org"} {
		if _, err := service.Login(ctx, email, bad); !errors.Is(err, ErrCredentials) {
			t.Fatalf("login %s: %v", email, err)
		}
	}
	token, err := service.CreateSession(ctx, user.ID)
	if err != nil {
		t.Fatal(err)
	}
	restored, err := service.Session(ctx, token)
	if err != nil || restored.ID != user.ID {
		t.Fatalf("session restore: %v", err)
	}
	if err := service.Logout(ctx, token); err != nil {
		t.Fatal(err)
	}
	if _, err := service.Session(ctx, token); err == nil {
		t.Fatal("logged-out session accepted")
	}
}
