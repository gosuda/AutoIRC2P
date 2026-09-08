package store

import (
	"path/filepath"
	"testing"
)

func TestUsersForPrewarmPrefersRecentSessionsThenNewestAccounts(t *testing.T) {
	db, queries, err := NewSQLite(t.Context(), filepath.Join(t.TempDir(), "warmup.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := db.Close(); err != nil {
			t.Error(err)
		}
	}()
	var users []User
	for _, nick := range []string{"alice", "bob", "carol", "dave"} {
		user, err := queries.CreateUser(t.Context(), CreateUserParams{
			Email: nick + "@gmail.com", Nick: nick, PasswordSalt: []byte{1}, PasswordHash: []byte{2},
			IrcPassword: []byte{3}, IdentityKeys: []byte{4}, IdentityAddress: nick + ".b32.i2p", CreatedAt: 100,
		})
		if err != nil {
			t.Fatal(err)
		}
		users = append(users, user)
	}
	selected, err := queries.UsersForPrewarm(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if len(selected) != 2 || selected[0].ID != users[3].ID || selected[1].ID != users[2].ID {
		t.Fatalf("sessionless warmup selected %v, want newest two accounts", selected)
	}
	for i, session := range []CreateSessionParams{
		{UserID: users[0].ID, ExpiresAt: 100},
		{UserID: users[0].ID, ExpiresAt: 300},
		{UserID: users[1].ID, ExpiresAt: 200},
		{UserID: users[2].ID, ExpiresAt: 150},
	} {
		session.TokenHash = []byte{byte(i)}
		if err := queries.CreateSession(t.Context(), session); err != nil {
			t.Fatal(err)
		}
	}
	selected, err = queries.UsersForPrewarm(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if len(selected) != 2 || selected[0].ID != users[0].ID || selected[1].ID != users[1].ID {
		t.Fatalf("session-prioritized warmup selected %v, want alice then bob without duplicate accounts", selected)
	}
}
