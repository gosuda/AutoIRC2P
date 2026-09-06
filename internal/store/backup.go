package store

import (
	"bytes"
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"path/filepath"
)

const backupDatabase = "chat.sqlite"
const backupKey = "application.key"
const backupManifest = "manifest.json"

var errInvalidBundle = errors.New("invalid backup bundle")
var errKeyMismatch = errors.New("application key does not match database secrets")
var errKeyChanged = errors.New("application key changed during backup")

// Checksums detect damage, not malicious replacement. Bundles contain live credentials.
type backupMetadata struct {
	FormatVersion  int    `json:"format_version"`
	SchemaVersion  int    `json:"schema_version"`
	DatabaseSHA256 string `json:"database_sha256"`
	KeySHA256      string `json:"key_sha256"`
}

func Backup(ctx context.Context, databasePath, keyPath, destinationDir string) (err error) {
	if err = ctx.Err(); err != nil {
		return err
	}
	stage, err := stageBundle(destinationDir)
	if err != nil {
		return err
	}
	defer func() { err = errors.Join(err, os.RemoveAll(stage)) }()
	key, err := readKey(keyPath)
	if err != nil {
		return err
	}
	defer clear(key)
	source, err := regularFile(databasePath)
	if err != nil {
		return err
	}
	db, err := newSnapshotDB(ctx, databasePath, false)
	if err != nil {
		return err
	}
	snapshotPath := filepath.Join(stage, backupDatabase)
	// SQLite reads committed pages from both the database and its live WAL.
	_, snapshotErr := db.ExecContext(ctx, "VACUUM INTO ?", snapshotPath)
	if err = errors.Join(snapshotErr, db.Close()); err != nil {
		return err
	}
	after, err := regularFile(databasePath)
	if err != nil {
		return err
	}
	if !os.SameFile(source, after) {
		return fmt.Errorf("database replaced during backup: %w", errInvalidBundle)
	}
	afterKey, err := readKey(keyPath)
	if err != nil {
		return err
	}
	sameKey := bytes.Equal(key, afterKey)
	clear(afterKey)
	if !sameKey {
		return errKeyChanged
	}
	if err = os.Chmod(snapshotPath, 0600); err != nil {
		return err
	}
	if err = detachSnapshotWAL(ctx, snapshotPath); err != nil {
		return err
	}
	version, err := validateSnapshot(ctx, snapshotPath, key)
	if err != nil {
		return err
	}
	if err = writePrivate(filepath.Join(stage, backupKey), key); err != nil {
		return err
	}
	databaseHash, err := hashFile(ctx, snapshotPath)
	if err != nil {
		return err
	}
	keyHash := sha256.Sum256(key)
	metadata := backupMetadata{FormatVersion: 1, SchemaVersion: version, DatabaseSHA256: databaseHash, KeySHA256: hex.EncodeToString(keyHash[:])}
	encoded, err := json.Marshal(metadata)
	if err != nil {
		return err
	}
	if err = writePrivate(filepath.Join(stage, backupManifest), encoded); err != nil {
		return err
	}
	return publishBundle(ctx, stage, destinationDir)
}

func Restore(ctx context.Context, backupDir, destinationDir string) (err error) {
	if err = ctx.Err(); err != nil {
		return err
	}
	info, err := os.Lstat(backupDir)
	if err != nil {
		return err
	}
	if !info.IsDir() {
		return fmt.Errorf("backup must be a real directory: %w", errInvalidBundle)
	}
	entries, err := os.ReadDir(backupDir)
	if err != nil {
		return err
	}
	if len(entries) != 3 {
		return fmt.Errorf("backup must contain exactly three fixed files: %w", errInvalidBundle)
	}
	for _, entry := range entries {
		if entry.Name() != backupDatabase && entry.Name() != backupKey && entry.Name() != backupManifest {
			return errInvalidBundle
		}
	}
	stage, err := stageBundle(destinationDir)
	if err != nil {
		return err
	}
	defer func() { err = errors.Join(err, os.RemoveAll(stage)) }()
	for _, name := range []string{backupDatabase, backupKey, backupManifest} {
		if err = copyPrivate(ctx, filepath.Join(backupDir, name), filepath.Join(stage, name)); err != nil {
			return err
		}
	}
	encoded, err := readSmallFile(filepath.Join(stage, backupManifest), 65536)
	if err != nil {
		return err
	}
	var metadata backupMetadata
	decoder := json.NewDecoder(bytes.NewReader(encoded))
	decoder.DisallowUnknownFields()
	if err = decoder.Decode(&metadata); err != nil {
		return fmt.Errorf("manifest: %w", err)
	}
	if decoder.Decode(new(any)) != io.EOF || metadata.FormatVersion != 1 {
		return errInvalidBundle
	}
	for name, expected := range map[string]string{backupDatabase: metadata.DatabaseSHA256, backupKey: metadata.KeySHA256} {
		actual, hashErr := hashFile(ctx, filepath.Join(stage, name))
		if hashErr != nil {
			return hashErr
		}
		if actual != expected {
			return fmt.Errorf("%s checksum: %w", name, errInvalidBundle)
		}
	}
	key, err := readKey(filepath.Join(stage, backupKey))
	if err != nil {
		return err
	}
	defer clear(key)
	version, err := validateSnapshot(ctx, filepath.Join(stage, backupDatabase), key)
	if err != nil {
		return err
	}
	if version != metadata.SchemaVersion {
		return fmt.Errorf("schema version disagrees with manifest: %w", errInvalidBundle)
	}
	return publishBundle(ctx, stage, destinationDir)
}

func stageBundle(destination string) (string, error) {
	if _, err := os.Lstat(destination); !errors.Is(err, os.ErrNotExist) {
		if err == nil {
			return "", fmt.Errorf("destination already exists: %w", os.ErrExist)
		}
		return "", err
	}
	return os.MkdirTemp(filepath.Dir(filepath.Clean(destination)), ".autoirc-backup-")
}

func publishBundle(ctx context.Context, stage, destination string) error {
	for _, name := range []string{backupDatabase, backupKey, backupManifest} {
		if err := syncPath(filepath.Join(stage, name)); err != nil {
			return err
		}
	}
	if err := syncPath(stage); err != nil {
		return err
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := renameExclusive(stage, destination); err != nil {
		return err
	}
	return syncPath(filepath.Dir(filepath.Clean(destination)))
}

func syncPath(path string) error {
	file, err := os.Open(path)
	if err != nil {
		return err
	}
	return errors.Join(file.Sync(), file.Close())
}

func regularFile(path string) (os.FileInfo, error) {
	info, err := os.Lstat(path)
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() {
		return nil, fmt.Errorf("%s is not a regular file: %w", filepath.Base(path), errInvalidBundle)
	}
	return info, nil
}

func openRegular(path string) (*os.File, error) {
	before, err := regularFile(path)
	if err != nil {
		return nil, err
	}
	file, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	opened, statErr := file.Stat()
	after, pathErr := regularFile(path)
	if statErr != nil || pathErr != nil {
		return nil, errors.Join(statErr, pathErr, file.Close())
	}
	if !os.SameFile(before, opened) || !os.SameFile(opened, after) {
		return nil, errors.Join(errInvalidBundle, file.Close())
	}
	return file, nil
}

func readSmallFile(path string, limit int64) (data []byte, err error) {
	file, err := openRegular(path)
	if err != nil {
		return nil, err
	}
	defer func() { err = errors.Join(err, file.Close()) }()
	data, err = io.ReadAll(io.LimitReader(file, limit+1))
	if err == nil && int64(len(data)) > limit {
		return nil, errInvalidBundle
	}
	return data, err
}

func readKey(path string) ([]byte, error) {
	key, err := readSmallFile(path, 32)
	if err != nil {
		return nil, err
	}
	if len(key) != 32 {
		clear(key)
		return nil, fmt.Errorf("application key must contain 32 bytes: %w", errInvalidBundle)
	}
	return key, nil
}

func writePrivate(path string, content []byte) (err error) {
	file, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if err != nil {
		return err
	}
	defer func() { err = errors.Join(err, file.Close()) }()
	_, err = file.Write(content)
	return err
}

type contextReader struct {
	ctx    context.Context
	reader io.Reader
}

func (r contextReader) Read(buffer []byte) (int, error) {
	if err := r.ctx.Err(); err != nil {
		return 0, err
	}
	return r.reader.Read(buffer)
}

func copyPrivate(ctx context.Context, source, target string) (err error) {
	input, err := openRegular(source)
	if err != nil {
		return err
	}
	defer func() { err = errors.Join(err, input.Close()) }()
	output, err := os.OpenFile(target, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if err != nil {
		return err
	}
	defer func() { err = errors.Join(err, output.Close()) }()
	_, err = io.Copy(output, contextReader{ctx: ctx, reader: input})
	return err
}

func hashFile(ctx context.Context, path string) (digest string, err error) {
	file, err := openRegular(path)
	if err != nil {
		return "", err
	}
	defer func() { err = errors.Join(err, file.Close()) }()
	hash := sha256.New()
	if _, err = io.Copy(hash, contextReader{ctx: ctx, reader: file}); err != nil {
		return "", err
	}
	return hex.EncodeToString(hash.Sum(nil)), nil
}

func newSnapshotDB(ctx context.Context, path string, writable bool) (*sql.DB, error) {
	if _, err := regularFile(path); err != nil {
		return nil, err
	}
	for _, suffix := range []string{"-wal", "-shm", "-journal"} {
		if _, err := regularFile(path + suffix); err != nil && !errors.Is(err, os.ErrNotExist) {
			return nil, err
		}
	}
	absolute, err := filepath.Abs(path)
	if err != nil {
		return nil, err
	}
	mode := "ro"
	if writable {
		mode = "rw"
	}
	location := url.URL{Scheme: "file", Path: absolute, RawQuery: "mode=" + mode}
	db, err := sql.Open("sqlite", location.String())
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	if _, err = db.ExecContext(ctx, "PRAGMA busy_timeout=5000; PRAGMA trusted_schema=OFF;"); err != nil {
		return nil, errors.Join(err, db.Close())
	}
	return db, nil
}

func validateSnapshot(ctx context.Context, path string, key []byte) (version int, err error) {
	db, err := newSnapshotDB(ctx, path, false)
	if err != nil {
		return 0, err
	}
	defer func() { err = errors.Join(err, db.Close()) }()
	if err = db.QueryRowContext(ctx, "PRAGMA user_version").Scan(&version); err != nil {
		return 0, err
	}
	if version < 1 || version > schemaVersion {
		return 0, fmt.Errorf("unsupported database version %d", version)
	}
	var journal string
	if err = db.QueryRowContext(ctx, "PRAGMA journal_mode").Scan(&journal); err != nil {
		return 0, err
	}
	if journal != "delete" {
		return 0, fmt.Errorf("snapshot is not standalone: %w", errInvalidBundle)
	}
	var integrity string
	if err = db.QueryRowContext(ctx, "PRAGMA integrity_check").Scan(&integrity); err != nil {
		return 0, err
	}
	if integrity != "ok" {
		return 0, fmt.Errorf("SQLite integrity check failed: %w", errInvalidBundle)
	}
	rows, err := db.QueryContext(ctx, "PRAGMA foreign_key_check")
	if err != nil {
		return 0, err
	}
	hasViolation := rows.Next()
	if err = errors.Join(rows.Err(), rows.Close()); err != nil {
		return 0, err
	}
	if hasViolation {
		return 0, fmt.Errorf("SQLite foreign key check failed: %w", errInvalidBundle)
	}
	if err = validateSchema(ctx, db, version); err != nil {
		return 0, err
	}
	if err = validateKey(ctx, New(db), key); err != nil {
		return 0, err
	}
	return version, nil
}

func detachSnapshotWAL(ctx context.Context, path string) error {
	db, err := newSnapshotDB(ctx, path, true)
	if err != nil {
		return err
	}
	// Standalone bundles must not depend on sidecar WAL/SHM files.
	_, err = db.ExecContext(ctx, "PRAGMA journal_mode=DELETE;")
	return errors.Join(err, db.Close())
}

func validateSchema(ctx context.Context, db *sql.DB, version int) error {
	columns := map[string]string{
		"users":         "id,email,nick,password_salt,password_hash,irc_password,identity_keys,identity_address,irc_registered,created_at",
		"sessions":      "token_hash,user_id,expires_at",
		"observer":      "id,nick,identity_keys,identity_address",
		"messages":      "id,room,nick,original,source_language,wire_language,service,created_at",
		"translations":  "cache_key,translated",
		"send_requests": "user_id,request_id,state,message_id,room,nick,original,wire_text,created_at,echo_consumed",
	}
	if version >= 2 {
		columns["messages"] += ",sender_user_id,sender_request_id"
		columns["send_requests"] += ",original_mode,error_code,updated_at,expires_at"
	}
	if version >= 3 {
		columns["translations"] += ",created_at"
		columns["send_requests"] += ",payload_purged"
	}
	for table, fields := range columns {
		var kind string
		if err := db.QueryRowContext(ctx, "SELECT type FROM sqlite_schema WHERE name = ?", table).Scan(&kind); err != nil {
			return err
		}
		if kind != "table" {
			return errInvalidBundle
		}
		rows, err := db.QueryContext(ctx, "SELECT "+fields+" FROM "+table+" LIMIT 0")
		if err != nil {
			return fmt.Errorf("schema %s: %w", table, err)
		}
		if err := rows.Close(); err != nil {
			return err
		}
	}
	return nil
}

func validateKey(ctx context.Context, q *Queries, key []byte) error {
	block, err := aes.NewCipher(key)
	if err != nil {
		return err
	}
	aead, err := cipher.NewGCMWithRandomNonce(block)
	if err != nil {
		return err
	}
	check := func(encrypted []byte, purpose string) error {
		plaintext, err := aead.Open(nil, nil, encrypted, []byte(purpose))
		clear(plaintext)
		if err != nil {
			return errKeyMismatch
		}
		return nil
	}
	var after int64
	for {
		users, err := q.BackupUserSecrets(ctx, after)
		if err != nil {
			return err
		}
		for _, user := range users {
			if err = check(user.IdentityKeys, "identity:"+user.Email); err != nil {
				return err
			}
			if err = check(user.IrcPassword, "irc-password:"+user.Email); err != nil {
				return err
			}
			after = user.ID
		}
		if len(users) < 100 {
			break
		}
	}
	observer, err := q.GetObserver(ctx)
	if errors.Is(err, sql.ErrNoRows) {
		return nil
	}
	if err != nil {
		return err
	}
	return check(observer.IdentityKeys, "observer")
}
