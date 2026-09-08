package auth

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"path/filepath"
	"testing"

	"github.com/gosuda/AutoIRC2P/internal/irc"
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
	account, err := service.Account(ctx, user)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(account.ReleaseSensitive)
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

func TestIdentityPoolsConvergeAcrossConcurrentLoadsAndReopen(t *testing.T) {
	ctx := t.Context()
	path := filepath.Join(t.TempDir(), "chat.sqlite")
	key := bytes.Repeat([]byte{0x37}, 32)
	db, q, err := store.NewSQLite(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := db.Close(); err != nil {
			t.Error(err)
		}
	})
	first, err := New(q, key)
	if err != nil {
		t.Fatal(err)
	}
	otherDB, otherQ, err := store.NewSQLite(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := otherDB.Close(); err != nil {
			t.Error(err)
		}
	})
	second, err := New(otherQ, key)
	if err != nil {
		t.Fatal(err)
	}
	proof := hex.EncodeToString(bytes.Repeat([]byte{1}, 32))
	alice, err := first.Register(ctx, "alice@example.org", "alice", proof)
	if err != nil {
		t.Fatal(err)
	}
	bob, err := first.Register(ctx, "bob@example.org", "bobby", proof)
	if err != nil {
		t.Fatal(err)
	}
	owners := []struct {
		name    string
		load    func(*Service) (irc.Account, error)
		pool    func() ([]byte, error)
		purpose string
	}{
		{"alice", func(s *Service) (irc.Account, error) { return s.Account(ctx, alice) }, func() ([]byte, error) { return q.UserIdentityPool(ctx, alice.ID) }, "identity-pool:" + alice.Email},
		{"bob", func(s *Service) (irc.Account, error) { return s.Account(ctx, bob) }, func() ([]byte, error) { return q.UserIdentityPool(ctx, bob.ID) }, "identity-pool:" + bob.Email},
		{"observer", func(s *Service) (irc.Account, error) { return s.Observer(ctx) }, func() ([]byte, error) { return q.BackupObserverIdentityPool(ctx) }, "observer-pool"},
	}
	originals := make([]irc.Account, len(owners))
	addresses := make(map[string]string)
	for i, owner := range owners {
		type result struct {
			account irc.Account
			err     error
		}
		start := make(chan struct{})
		results := make(chan result, 2)
		for _, service := range []*Service{first, second} {
			go func() {
				<-start
				account, err := owner.load(service)
				results <- result{account, err}
			}()
		}
		close(start)
		left, right := <-results, <-results
		t.Cleanup(left.account.ReleaseSensitive)
		t.Cleanup(right.account.ReleaseSensitive)
		if err := errors.Join(left.err, right.err); err != nil {
			t.Fatalf("concurrent %s population: %v", owner.name, err)
		}
		assertSameDestinations(t, left.account, right.account)
		originals[i] = left.account
		for _, identity := range append([]irc.Identity{left.account.Identity}, left.account.Alternates...) {
			if previous, exists := addresses[identity.Address]; exists {
				t.Fatalf("%s shares destination with %s", owner.name, previous)
			}
			addresses[identity.Address] = owner.name
		}
		encrypted, err := owner.pool()
		if err != nil {
			t.Fatal(err)
		}
		plaintext, err := json.Marshal(left.account.Alternates)
		if err != nil {
			t.Fatal(err)
		}
		defer clear(plaintext)
		if bytes.Equal(encrypted, plaintext) {
			t.Fatalf("%s alternate keys stored as plaintext", owner.name)
		}
		opened, err := first.Open(encrypted, owner.purpose)
		defer clear(opened)
		if err != nil || !bytes.Equal(opened, plaintext) {
			t.Fatalf("%s pool ciphertext does not preserve returned keys: %v", owner.name, err)
		}
	}
	if err := errors.Join(db.Close(), otherDB.Close()); err != nil {
		t.Fatal(err)
	}
	reopenedDB, reopenedQ, err := store.NewSQLite(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := reopenedDB.Close(); err != nil {
			t.Error(err)
		}
	})
	reopened, err := New(reopenedQ, key)
	if err != nil {
		t.Fatal(err)
	}
	for i, owner := range owners {
		account, err := owner.load(reopened)
		if err != nil {
			t.Fatalf("reopen %s: %v", owner.name, err)
		}
		t.Cleanup(account.ReleaseSensitive)
		assertSameDestinations(t, account, originals[i])
	}
}

func TestInvalidStoredPoolsFailWithoutReplacingIdentity(t *testing.T) {
	ctx := t.Context()
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
	user, err := service.Register(ctx, "alice@example.org", "alice", hex.EncodeToString(bytes.Repeat([]byte{1}, 32)))
	if err != nil {
		t.Fatal(err)
	}
	original, err := service.Account(ctx, user)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(original.ReleaseSensitive)
	seal := func(pool []irc.Identity, purpose string) []byte {
		plaintext, err := json.Marshal(pool)
		if err != nil {
			t.Fatal(err)
		}
		defer clear(plaintext)
		return service.Seal(plaintext, purpose)
	}
	purpose := "identity-pool:" + user.Email
	for _, test := range []struct {
		name      string
		encrypted []byte
	}{
		{"damaged-ciphertext", []byte{0}},
		{"wrong-account", seal(original.Alternates, "identity-pool:another@example.org")},
		{"invalid-json", service.Seal([]byte("{"), purpose)},
		{"wrong-size", seal(original.Alternates[:1], purpose)},
		{"primary-collision", seal([]irc.Identity{original.Identity, original.Alternates[0]}, purpose)},
		{"alternate-collision", seal([]irc.Identity{original.Alternates[0], original.Alternates[0]}, purpose)},
	} {
		t.Run(test.name, func(t *testing.T) {
			if _, err := db.ExecContext(ctx, "UPDATE users SET identity_pool=? WHERE id=?", test.encrypted, user.ID); err != nil {
				t.Fatal(err)
			}
			account, err := service.Account(ctx, user)
			account.ReleaseSensitive()
			if err == nil {
				t.Fatal("invalid pool accepted")
			}
			stored, err := q.UserByID(ctx, user.ID)
			if err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(stored.IdentityKeys, user.IdentityKeys) || stored.IdentityAddress != user.IdentityAddress || !bytes.Equal(stored.IdentityPool, test.encrypted) {
				t.Fatal("failed pool load replaced persisted identity")
			}
		})
	}
}

func assertSameDestinations(t *testing.T, got, want irc.Account) {
	t.Helper()
	if got.Identity.Address != want.Identity.Address || !bytes.Equal(got.Identity.Keys, want.Identity.Keys) {
		t.Fatal("primary destination changed")
	}
	if len(got.Alternates) != irc.DestinationPoolSize-1 || len(want.Alternates) != irc.DestinationPoolSize-1 {
		t.Fatalf("alternate pool sizes = %d and %d, want %d", len(got.Alternates), len(want.Alternates), irc.DestinationPoolSize-1)
	}
	for i, identity := range got.Alternates {
		if identity.Address != want.Alternates[i].Address || !bytes.Equal(identity.Keys, want.Alternates[i].Keys) {
			t.Errorf("alternate destination %d changed", i)
		}
	}
}
