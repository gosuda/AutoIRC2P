package store_test

import (
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gosuda/AutoIRC2P/internal/auth"
	"github.com/gosuda/AutoIRC2P/internal/store"
)

type recoveryFixture struct {
	root    string
	path    string
	keyPath string
	db      *sql.DB
	q       *store.Queries
	service *auth.Service
	user    store.User
	proof   string
	token   string
}

func newRecoveryFixture(t *testing.T) recoveryFixture {
	t.Helper()
	root := t.TempDir()
	path := filepath.Join(root, "chat.sqlite")
	db, q, err := store.NewSQLite(t.Context(), path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := db.Close(); err != nil {
			t.Error(err)
		}
	})
	key := bytes.Repeat([]byte{0x37}, 32)
	keyPath := filepath.Join(root, "application.key")
	if err := os.WriteFile(keyPath, key, 0600); err != nil {
		t.Fatal(err)
	}
	service, err := auth.New(q, key)
	if err != nil {
		t.Fatal(err)
	}
	proof := strings.Repeat("ab", 32)
	user, err := service.Register(t.Context(), "alice@example.org", "alice", proof)
	if err != nil {
		t.Fatal(err)
	}
	token, err := service.CreateSession(t.Context(), user.ID)
	if err != nil {
		t.Fatal(err)
	}
	return recoveryFixture{root: root, path: path, keyPath: keyPath, db: db, q: q, service: service, user: user, proof: proof, token: token}
}

func TestLiveWALBackupRestoresAuthenticationAndIdentity(t *testing.T) {
	f := newRecoveryFixture(t)
	if _, err := f.db.ExecContext(t.Context(), "PRAGMA wal_checkpoint(TRUNCATE); PRAGMA wal_autocheckpoint=0;"); err != nil {
		t.Fatal(err)
	}
	observer, err := f.service.Observer(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if err := f.q.RegisterIRC(t.Context(), f.user.ID); err != nil {
		t.Fatal(err)
	}
	if err := f.q.PutTranslation(t.Context(), "cache", "translated WAL text"); err != nil {
		t.Fatal(err)
	}
	message, err := f.q.AddMessage(t.Context(), store.AddMessageParams{Room: "#one", Nick: "alice", Original: "committed WAL text", CreatedAt: 123})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.q.ClaimSend(t.Context(), store.ClaimSendParams{UserID: f.user.ID, RequestID: "active", State: "sending", Room: "#one", Original: "pending"}); err != nil {
		t.Fatal(err)
	}
	wal, err := os.Stat(f.path + "-wal")
	if err != nil || wal.Size() <= 32 {
		t.Fatalf("expected committed live WAL: %v %v", wal, err)
	}
	backup := filepath.Join(f.root, "backup")
	if err := store.Backup(t.Context(), f.path, f.keyPath, backup); err != nil {
		t.Fatal(err)
	}
	pending, err := f.q.GetSend(t.Context(), store.GetSendParams{UserID: f.user.ID, RequestID: "active"})
	if err != nil || pending.State != "sending" {
		t.Fatalf("backup recovered live sends: %+v %v", pending, err)
	}
	if _, err := f.q.AddMessage(t.Context(), store.AddMessageParams{Room: "#one", Original: "after snapshot"}); err != nil {
		t.Fatal(err)
	}
	restored := filepath.Join(f.root, "restored")
	if err := store.Restore(t.Context(), backup, restored); err != nil {
		t.Fatal(err)
	}
	for _, directory := range []string{backup, restored} {
		info, err := os.Stat(directory)
		if err != nil {
			t.Fatal(err)
		}
		if info.Mode().Perm() != 0700 {
			t.Fatalf("private directory mode: %o", info.Mode().Perm())
		}
		for _, name := range []string{"chat.sqlite", "application.key", "manifest.json"} {
			info, err := os.Stat(filepath.Join(directory, name))
			if err != nil {
				t.Fatal(err)
			}
			if info.Mode().Perm() != 0600 {
				t.Fatalf("%s mode: %o", name, info.Mode().Perm())
			}
		}
	}
	db, q, err := store.NewSQLite(t.Context(), filepath.Join(restored, "chat.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := db.Close(); err != nil {
			t.Error(err)
		}
	})
	key, err := os.ReadFile(filepath.Join(restored, "application.key"))
	if err != nil {
		t.Fatal(err)
	}
	recovered, err := auth.New(q, key)
	if err != nil {
		t.Fatal(err)
	}
	if recovered.Salt(f.user.Email) != f.service.Salt(f.user.Email) {
		t.Fatal("browser salt changed across recovery")
	}
	login, err := recovered.Login(t.Context(), f.user.Email, f.proof)
	if err != nil || login.ID != f.user.ID {
		t.Fatalf("restored login: user=%d err=%v", login.ID, err)
	}
	session, err := recovered.Session(t.Context(), f.token)
	if err != nil || session.ID != f.user.ID {
		t.Fatalf("restored session: user=%d err=%v", session.ID, err)
	}
	account, err := recovered.Account(login)
	if err != nil {
		t.Fatal(err)
	}
	original, err := f.service.Account(f.user)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(account.Identity.Keys, original.Identity.Keys) || account.Identity.Address != original.Identity.Address || account.Password != original.Password || !account.Registered {
		t.Fatal("IRC account identity or credentials changed")
	}
	restoredObserver, err := recovered.Observer(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if restoredObserver.Nick != observer.Nick || restoredObserver.Identity.Address != observer.Identity.Address || !bytes.Equal(restoredObserver.Identity.Keys, observer.Identity.Keys) {
		t.Fatal("observer identity changed")
	}
	messages, err := q.Messages(t.Context(), store.MessagesParams{Room: "#one", ID: message.ID + 2})
	if err != nil || len(messages) != 1 || messages[0].Original != "committed WAL text" {
		t.Fatalf("snapshot boundary: %+v %v", messages, err)
	}
	translation, err := q.GetTranslation(t.Context(), "cache")
	if err != nil || translation != "translated WAL text" {
		t.Fatalf("WAL cache missing: %q %v", translation, err)
	}
}

func TestRestoreRefusesCorruptOrMismatchedBundles(t *testing.T) {
	for _, damage := range []string{"database", "manifest", "key", "mismatched-key", "symlink", "extra-file"} {
		t.Run(damage, func(t *testing.T) {
			f := newRecoveryFixture(t)
			backup := filepath.Join(f.root, "backup")
			if err := store.Backup(t.Context(), f.path, f.keyPath, backup); err != nil {
				t.Fatal(err)
			}
			switch damage {
			case "database":
				if err := os.WriteFile(filepath.Join(backup, "chat.sqlite"), []byte("not SQLite"), 0600); err != nil {
					t.Fatal(err)
				}
			case "manifest":
				if err := os.WriteFile(filepath.Join(backup, "manifest.json"), []byte("{"), 0600); err != nil {
					t.Fatal(err)
				}
			case "key", "mismatched-key":
				key := bytes.Repeat([]byte{0x91}, 32)
				if err := os.WriteFile(filepath.Join(backup, "application.key"), key, 0600); err != nil {
					t.Fatal(err)
				}
				if damage == "mismatched-key" {
					path := filepath.Join(backup, "manifest.json")
					content, err := os.ReadFile(path)
					if err != nil {
						t.Fatal(err)
					}
					var manifest map[string]any
					if err := json.Unmarshal(content, &manifest); err != nil {
						t.Fatal(err)
					}
					sum := sha256.Sum256(key)
					manifest["key_sha256"] = hex.EncodeToString(sum[:])
					content, err = json.Marshal(manifest)
					if err != nil {
						t.Fatal(err)
					}
					if err := os.WriteFile(path, content, 0600); err != nil {
						t.Fatal(err)
					}
				}
			case "symlink":
				path := filepath.Join(backup, "application.key")
				if err := os.Remove(path); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink(f.keyPath, path); err != nil {
					t.Fatal(err)
				}
			case "extra-file":
				if err := os.WriteFile(filepath.Join(backup, "unexpected"), nil, 0600); err != nil {
					t.Fatal(err)
				}
			}
			target := filepath.Join(f.root, "restored")
			if err := store.Restore(t.Context(), backup, target); err == nil {
				t.Fatal("invalid bundle restored")
			}
			assertNoRecoveryOutput(t, f.root, target)
		})
	}
}

func TestRecoveryRefusesExistingDestinations(t *testing.T) {
	f := newRecoveryFixture(t)
	backup := filepath.Join(f.root, "backup")
	if err := store.Backup(t.Context(), f.path, f.keyPath, backup); err != nil {
		t.Fatal(err)
	}
	for _, occupied := range []string{"empty-directory", "populated-directory", "file"} {
		t.Run(occupied, func(t *testing.T) {
			target := filepath.Join(f.root, occupied)
			if occupied == "file" {
				if err := os.WriteFile(target, []byte("keep"), 0600); err != nil {
					t.Fatal(err)
				}
			} else {
				if err := os.Mkdir(target, 0700); err != nil {
					t.Fatal(err)
				}
				if occupied == "populated-directory" {
					if err := os.WriteFile(filepath.Join(target, "keep"), []byte("keep"), 0600); err != nil {
						t.Fatal(err)
					}
				}
			}
			if err := store.Backup(t.Context(), f.path, f.keyPath, target); !errors.Is(err, os.ErrExist) {
				t.Fatalf("existing backup target: %v", err)
			}
			if err := store.Restore(t.Context(), backup, target); !errors.Is(err, os.ErrExist) {
				t.Fatalf("existing restore target: %v", err)
			}
			if occupied != "empty-directory" {
				path := target
				if occupied == "populated-directory" {
					path = filepath.Join(target, "keep")
				}
				content, err := os.ReadFile(path)
				if err != nil || string(content) != "keep" {
					t.Fatalf("existing data replaced: %q %v", content, err)
				}
			} else {
				entries, err := os.ReadDir(target)
				if err != nil || len(entries) != 0 {
					t.Fatalf("empty target overwritten: %v %v", entries, err)
				}
			}
		})
	}
}

func TestBackupRejectsWrongKeyAndUnsupportedSchema(t *testing.T) {
	for _, invalid := range []string{"wrong-key", "short-key", "schema"} {
		t.Run(invalid, func(t *testing.T) {
			f := newRecoveryFixture(t)
			switch invalid {
			case "wrong-key":
				if err := os.WriteFile(f.keyPath, bytes.Repeat([]byte{1}, 32), 0600); err != nil {
					t.Fatal(err)
				}
			case "short-key":
				if err := os.WriteFile(f.keyPath, []byte{1}, 0600); err != nil {
					t.Fatal(err)
				}
			case "schema":
				if _, err := f.db.ExecContext(t.Context(), "PRAGMA user_version=999"); err != nil {
					t.Fatal(err)
				}
			}
			target := filepath.Join(f.root, "backup")
			if err := store.Backup(t.Context(), f.path, f.keyPath, target); err == nil {
				t.Fatal("invalid source backed up")
			}
			assertNoRecoveryOutput(t, f.root, target)
		})
	}
}

func TestRestoreValidatesDatabaseBeyondChecksums(t *testing.T) {
	for _, corruption := range []string{"integrity", "foreign-key", "schema"} {
		t.Run(corruption, func(t *testing.T) {
			f := newRecoveryFixture(t)
			backup := filepath.Join(f.root, "backup")
			if err := store.Backup(t.Context(), f.path, f.keyPath, backup); err != nil {
				t.Fatal(err)
			}
			database := filepath.Join(backup, "chat.sqlite")
			if corruption == "integrity" {
				if err := os.WriteFile(database, []byte("invalid SQLite with a valid checksum"), 0600); err != nil {
					t.Fatal(err)
				}
			} else {
				db, err := sql.Open("sqlite", database)
				if err != nil {
					t.Fatal(err)
				}
				statement := "PRAGMA foreign_keys=OFF; UPDATE sessions SET user_id=999999;"
				if corruption == "schema" {
					statement = "DROP TABLE translations;"
				}
				_, operationErr := db.ExecContext(t.Context(), statement)
				if err := errors.Join(operationErr, db.Close()); err != nil {
					t.Fatal(err)
				}
			}
			data, err := os.ReadFile(database)
			if err != nil {
				t.Fatal(err)
			}
			sum := sha256.Sum256(data)
			path := filepath.Join(backup, "manifest.json")
			data, err = os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			var manifest map[string]any
			if err := json.Unmarshal(data, &manifest); err != nil {
				t.Fatal(err)
			}
			manifest["database_sha256"] = hex.EncodeToString(sum[:])
			data, err = json.Marshal(manifest)
			if err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(path, data, 0600); err != nil {
				t.Fatal(err)
			}
			target := filepath.Join(f.root, "restored")
			if err := store.Restore(t.Context(), backup, target); err == nil {
				t.Fatal("invalid database accepted despite valid checksum")
			}
			assertNoRecoveryOutput(t, f.root, target)
		})
	}
}

func TestCancelledRecoveryLeavesNoOutput(t *testing.T) {
	f := newRecoveryFixture(t)
	backup := filepath.Join(f.root, "backup")
	if err := store.Backup(t.Context(), f.path, f.keyPath, backup); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	target := filepath.Join(f.root, "cancelled")
	if err := store.Backup(ctx, f.path, f.keyPath, target); !errors.Is(err, context.Canceled) {
		t.Fatalf("backup cancellation: %v", err)
	}
	if err := store.Restore(ctx, backup, target); !errors.Is(err, context.Canceled) {
		t.Fatalf("restore cancellation: %v", err)
	}
	assertNoRecoveryOutput(t, f.root, target)
}

func assertNoRecoveryOutput(t *testing.T, root, target string) {
	t.Helper()
	if _, err := os.Lstat(target); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("failed recovery published target: %v", err)
	}
	entries, err := os.ReadDir(root)
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		if strings.HasPrefix(entry.Name(), ".autoirc-backup-") {
			t.Fatalf("staging directory leaked: %s", entry.Name())
		}
	}
}
