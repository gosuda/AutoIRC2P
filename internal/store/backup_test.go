package store_test

import (
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gosuda/AutoIRC2P/internal/auth"
	"github.com/gosuda/AutoIRC2P/internal/irc"
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
	user, err := service.Register(t.Context(), "alice", proof)
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
	t.Cleanup(observer.ReleaseSensitive)
	original, err := f.service.Account(t.Context(), f.user)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(original.ReleaseSensitive)
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
	originalSalt, err := f.service.Salt(t.Context(), f.user.Nick)
	if err != nil {
		t.Fatal(err)
	}
	restoredSalt, err := recovered.Salt(t.Context(), f.user.Nick)
	if err != nil {
		t.Fatal(err)
	}
	if restoredSalt != originalSalt {
		t.Fatal("browser salt changed across recovery")
	}
	login, err := recovered.Login(t.Context(), f.user.Nick, f.proof)
	if err != nil || login.ID != f.user.ID {
		t.Fatalf("restored login: user=%d err=%v", login.ID, err)
	}
	session, err := recovered.Session(t.Context(), f.token)
	if err != nil || session.ID != f.user.ID {
		t.Fatalf("restored session: user=%d err=%v", session.ID, err)
	}
	account, err := recovered.Account(t.Context(), login)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(account.ReleaseSensitive)
	if !bytes.Equal(account.Identity.Keys, original.Identity.Keys) || account.Identity.Address != original.Identity.Address || account.Password != original.Password || !account.Registered {
		t.Fatal("IRC account identity or credentials changed")
	}
	restoredObserver, err := recovered.Observer(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(restoredObserver.ReleaseSensitive)
	if restoredObserver.Nick != observer.Nick || restoredObserver.Identity.Address != observer.Identity.Address || !bytes.Equal(restoredObserver.Identity.Keys, observer.Identity.Keys) {
		t.Fatal("observer identity changed")
	}
	for _, pair := range []struct{ got, want irc.Account }{{account, original}, {restoredObserver, observer}} {
		if len(pair.got.Alternates) != irc.DestinationPoolSize-1 || len(pair.want.Alternates) != irc.DestinationPoolSize-1 {
			t.Fatal("backup lost alternate destinations")
		}
		for i, identity := range pair.got.Alternates {
			if identity.Address != pair.want.Alternates[i].Address || !bytes.Equal(identity.Keys, pair.want.Alternates[i].Keys) {
				t.Errorf("restored alternate destination %d changed", i)
			}
		}
	}
	messages, err := q.Messages(t.Context(), store.MessagesParams{Room: "#one", ID: message.ID + 2})
	if err != nil || len(messages) != 1 || messages[0].Original != "committed WAL text" {
		t.Fatalf("snapshot boundary: %+v %v", messages, err)
	}
	summaries, err := q.RoomSummaries(t.Context(), []string{"#one"}, map[string]int64{"#one": 0}, f.user.ID)
	if err != nil || len(summaries) != 1 || summaries[0].LatestMessageID != message.ID || summaries[0].UnreadCount != 1 {
		t.Fatalf("restored room summary: %+v %v", summaries, err)
	}
	translation, err := q.GetTranslation(t.Context(), "cache")
	if err != nil || translation != "translated WAL text" {
		t.Fatalf("WAL cache missing: %q %v", translation, err)
	}
}

func TestHistoricalBackupsRestoreAuthenticationAndMessageOwnership(t *testing.T) {
	for _, version := range []int{1, 2, 3, 4, 5} {
		t.Run(fmt.Sprintf("version%d", version), func(t *testing.T) {
			f := newRecoveryFixture(t)
			observer, err := f.service.Observer(t.Context())
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(observer.ReleaseSensitive)
			path := filepath.Join(f.root, "historical.sqlite")
			legacy, err := sql.Open("sqlite", path)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() {
				if err := legacy.Close(); err != nil {
					t.Error(err)
				}
			})
			_, err = legacy.ExecContext(t.Context(), `
CREATE TABLE users (id INTEGER PRIMARY KEY,email TEXT NOT NULL UNIQUE,nick TEXT NOT NULL UNIQUE COLLATE NOCASE,password_salt BLOB NOT NULL,password_hash BLOB NOT NULL,irc_password BLOB NOT NULL,identity_keys BLOB NOT NULL,identity_address TEXT NOT NULL,irc_registered INTEGER NOT NULL DEFAULT 0,created_at INTEGER NOT NULL);
CREATE TABLE sessions (token_hash BLOB PRIMARY KEY,user_id INTEGER NOT NULL REFERENCES users(id),expires_at INTEGER NOT NULL);
CREATE TABLE observer (id INTEGER PRIMARY KEY,nick TEXT NOT NULL,identity_keys BLOB NOT NULL,identity_address TEXT NOT NULL);
CREATE TABLE messages (id INTEGER PRIMARY KEY,room TEXT NOT NULL,nick TEXT NOT NULL,original TEXT NOT NULL,source_language TEXT NOT NULL,wire_language TEXT NOT NULL,service INTEGER NOT NULL DEFAULT 0,created_at INTEGER NOT NULL);
CREATE TABLE translations (cache_key TEXT PRIMARY KEY,translated TEXT NOT NULL);
CREATE TABLE send_requests (user_id INTEGER NOT NULL REFERENCES users(id),request_id TEXT NOT NULL,state TEXT NOT NULL,message_id INTEGER NOT NULL DEFAULT 0,room TEXT NOT NULL DEFAULT '',nick TEXT NOT NULL DEFAULT '',original TEXT NOT NULL DEFAULT '',wire_text TEXT NOT NULL DEFAULT '',created_at INTEGER NOT NULL DEFAULT 0,echo_consumed INTEGER NOT NULL DEFAULT 0,PRIMARY KEY(user_id,request_id));
PRAGMA user_version=1;`)
			if err != nil {
				t.Fatal(err)
			}
			_, err = legacy.ExecContext(t.Context(), "INSERT INTO users VALUES(?,?,?,?,?,?,?,?,1,?)", f.user.ID, f.user.Email, f.user.Nick, f.user.PasswordSalt, f.user.PasswordHash, f.user.IrcPassword, f.user.IdentityKeys, f.user.IdentityAddress, f.user.CreatedAt)
			if err != nil {
				t.Fatal(err)
			}
			_, err = legacy.ExecContext(t.Context(), "INSERT INTO observer VALUES(1,?,?,?)", observer.Nick, f.service.Seal(observer.Identity.Keys, "observer"), observer.Identity.Address)
			if err != nil {
				t.Fatal(err)
			}
			_, err = legacy.ExecContext(t.Context(), `INSERT INTO messages VALUES(7,'#one','alice','historical text','en','en',0,1000);
INSERT INTO send_requests VALUES(1,'confirmed-request','sent',7,'#one','alice','historical text','wire',1000,1);`)
			if err != nil {
				t.Fatal(err)
			}
			for next := 2; next <= version; next++ {
				migration, err := os.ReadFile(fmt.Sprintf("migration%d.sql", next))
				if err != nil {
					t.Fatal(err)
				}
				if _, err := legacy.ExecContext(t.Context(), string(migration)+fmt.Sprintf("\nPRAGMA user_version=%d;", next)); err != nil {
					t.Fatal(err)
				}
			}
			if err := legacy.Close(); err != nil {
				t.Fatal(err)
			}
			backup := filepath.Join(f.root, "historical-backup")
			if err := store.Backup(t.Context(), path, f.keyPath, backup); err != nil {
				t.Fatal(err)
			}
			restored := filepath.Join(f.root, "restored")
			if err := store.Restore(t.Context(), backup, restored); err != nil {
				t.Fatal(err)
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
			defer clear(key)
			recovered, err := auth.New(q, key)
			if err != nil {
				t.Fatal(err)
			}
			login, err := recovered.Login(t.Context(), f.user.Nick, f.proof)
			if err != nil {
				t.Fatal(err)
			}
			account, err := recovered.Account(t.Context(), login)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(account.ReleaseSensitive)
			primary, err := f.service.Open(f.user.IdentityKeys, "identity:"+f.user.Email)
			defer clear(primary)
			if err != nil || !bytes.Equal(account.Identity.Keys, primary) || account.Identity.Address != f.user.IdentityAddress || !account.Registered {
				t.Fatalf("historical account identity changed: %v", err)
			}
			restoredObserver, err := recovered.Observer(t.Context())
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(restoredObserver.ReleaseSensitive)
			if restoredObserver.Nick != observer.Nick || restoredObserver.Identity.Address != observer.Identity.Address || !bytes.Equal(restoredObserver.Identity.Keys, observer.Identity.Keys) {
				t.Fatal("historical observer identity changed")
			}
			if len(account.Alternates) != irc.DestinationPoolSize-1 || len(restoredObserver.Alternates) != irc.DestinationPoolSize-1 {
				t.Fatal("historical identities did not acquire destination pools")
			}
			messages, err := q.Messages(t.Context(), store.MessagesParams{Room: "#one", ID: 8})
			if err != nil || len(messages) != 1 || messages[0].Original != "historical text" || messages[0].SenderRequestID != "confirmed-request" {
				t.Fatalf("historical message ownership lost: %v", err)
			}
			summaries, err := q.RoomSummaries(t.Context(), []string{"#one"}, map[string]int64{"#one": 0}, 0)
			if err != nil || len(summaries) != 1 || summaries[0].LatestMessageID != 7 || summaries[0].UnreadCount != 1 {
				t.Fatalf("historical room summary lost: %+v %v", summaries, err)
			}
			claimed, err := q.ClaimSend(t.Context(), store.ClaimSendParams{UserID: f.user.ID, RequestID: "confirmed-request", State: "translating"})
			if err != nil || claimed != 0 {
				t.Fatalf("historical confirmed send reclaimed: claimed=%d err=%v", claimed, err)
			}
		})
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
	for _, corruption := range []string{"integrity", "foreign-key", "schema", "user-pool", "observer-pool", "user-pool-column", "observer-pool-column"} {
		t.Run(corruption, func(t *testing.T) {
			f := newRecoveryFixture(t)
			if strings.HasPrefix(corruption, "observer-") {
				observer, err := f.service.Observer(t.Context())
				if err != nil {
					t.Fatal(err)
				}
				t.Cleanup(observer.ReleaseSensitive)
			}
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
				switch corruption {
				case "schema":
					statement = "DROP TABLE translations;"
				case "user-pool":
					statement = "UPDATE users SET identity_pool=X'00';"
				case "observer-pool":
					statement = "UPDATE observer SET identity_pool=X'00';"
				case "user-pool-column":
					statement = "ALTER TABLE users DROP COLUMN identity_pool;"
				case "observer-pool-column":
					statement = "ALTER TABLE observer DROP COLUMN identity_pool;"
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
