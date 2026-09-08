package auth

import (
	"bytes"
	"context"
	"crypto/pbkdf2"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"path/filepath"
	"regexp"
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
	missingSalt, err := service.Salt(ctx, " ORDINARYNICK ")
	if err != nil {
		t.Fatal(err)
	}
	proof := browserProof(t, missingSalt, "my account password")
	proofBytes, err := hex.DecodeString(proof)
	if err != nil {
		t.Fatal(err)
	}
	user, err := service.Register(ctx, " ordinaryNick ", proof)
	if err != nil {
		t.Fatal(err)
	}
	registeredSalt, err := service.Salt(ctx, "ordinaryNick")
	if err != nil {
		t.Fatal(err)
	}
	if registeredSalt != missingSalt {
		t.Fatalf("registration changed public salt: %s != %s", registeredSalt, missingSalt)
	}
	absentSalt, err := service.Salt(ctx, "missingNick")
	if err != nil {
		t.Fatal(err)
	}
	if len(absentSalt) != len(missingSalt) {
		t.Fatal("missing account salt shape reveals existence")
	}
	if !regexp.MustCompile(`^[0-9a-f]{30}@gmail\.com$`).MatchString(user.Email) {
		t.Fatalf("internal address is not a synthetic Gmail identifier: %q", user.Email)
	}
	if bytes.Equal(user.PasswordHash, proofBytes) {
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
	loggedIn, err := service.Login(ctx, " ORDINARYNICK ", browserProof(t, registeredSalt, "my account password"))
	if err != nil {
		t.Fatal(err)
	}
	if loggedIn.ID != user.ID || loggedIn.Nick != "ordinaryNick" {
		t.Fatalf("nickname login returned id=%d nick=%q", loggedIn.ID, loggedIn.Nick)
	}
	bad := hex.EncodeToString(bytes.Repeat([]byte{9}, 32))
	for _, nick := range []string{"ordinaryNick", "missingNick"} {
		if _, err := service.Login(ctx, nick, bad); !errors.Is(err, ErrCredentials) {
			t.Fatalf("login %s: %v", nick, err)
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

func TestExistingEmailAccountUsesNicknameWithoutChangingSecrets(t *testing.T) {
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
	// These credentials were derived from the email before nickname authentication.
	const email = "person@example.org"
	const oldSalt = "425bd11fc097b834924af146e07d8668"
	oldProof, err := hex.DecodeString("188d51cdd2eab0d8ce65d0a4ce8007a8a20fe762015fd33d5aa2fce65c1b0c54")
	if err != nil {
		t.Fatal(err)
	}
	identity, err := irc.GenerateIdentity()
	if err != nil {
		t.Fatal(err)
	}
	defer clear(identity.Keys)
	passwordSalt := bytes.Repeat([]byte{5}, 32)
	legacy, err := q.CreateUser(ctx, store.CreateUserParams{
		Email:           email,
		Nick:            "legacyNick",
		PasswordSalt:    passwordSalt,
		PasswordHash:    PasswordDigest(passwordSalt, oldProof),
		IrcPassword:     service.Seal([]byte("existing IRC password"), "irc-password:"+email),
		IdentityKeys:    service.Seal(identity.Keys, "identity:"+email),
		IdentityAddress: identity.Address,
		CreatedAt:       1,
	})
	if err != nil {
		t.Fatal(err)
	}
	original, err := service.Account(ctx, legacy)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(original.ReleaseSensitive)
	before, err := q.UserByID(ctx, legacy.ID)
	if err != nil {
		t.Fatal(err)
	}
	salt, err := service.Salt(ctx, " LEGACYNICK ")
	if err != nil {
		t.Fatal(err)
	}
	if salt != oldSalt {
		t.Fatalf("existing browser salt changed: got %s, want %s", salt, oldSalt)
	}
	loggedIn, err := service.Login(ctx, " LEGACYNICK ", browserProof(t, salt, "unchanged account password"))
	if err != nil {
		t.Fatal(err)
	}
	if loggedIn.ID != legacy.ID || loggedIn.Nick != legacy.Nick {
		t.Fatalf("existing nickname login returned id=%d nick=%q", loggedIn.ID, loggedIn.Nick)
	}
	account, err := service.Account(ctx, loggedIn)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(account.ReleaseSensitive)
	assertSameDestinations(t, account, original)
	if account.Password != "existing IRC password" {
		t.Fatal("existing IRC password changed")
	}
	after, err := q.UserByID(ctx, legacy.ID)
	if err != nil {
		t.Fatal(err)
	}
	if after.Email != before.Email {
		t.Fatal("nickname authentication changed the stored email")
	}
	if !bytes.Equal(after.PasswordSalt, before.PasswordSalt) || !bytes.Equal(after.PasswordHash, before.PasswordHash) || !bytes.Equal(after.IrcPassword, before.IrcPassword) {
		t.Fatal("nickname authentication replaced existing stored credentials")
	}
	if !bytes.Equal(after.IdentityKeys, before.IdentityKeys) || !bytes.Equal(after.IdentityPool, before.IdentityPool) || after.IdentityAddress != before.IdentityAddress {
		t.Fatal("nickname authentication replaced the existing identity")
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
	alice, err := first.Register(ctx, "alice", proof)
	if err != nil {
		t.Fatal(err)
	}
	bob, err := first.Register(ctx, "bobby", proof)
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
	user, err := service.Register(ctx, "alice", hex.EncodeToString(bytes.Repeat([]byte{1}, 32)))
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

func browserProof(t *testing.T, salt, password string) string {
	t.Helper()
	decoded, err := hex.DecodeString(salt)
	if err != nil {
		t.Fatal(err)
	}
	proof, err := pbkdf2.Key(sha256.New, password, decoded, Iterations, 32)
	if err != nil {
		t.Fatal(err)
	}
	return hex.EncodeToString(proof)
}
