package auth

import (
	"bytes"
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"regexp"
	"strings"
	"time"

	"github.com/gosuda/AutoIRC2P/internal/irc"
	"github.com/gosuda/AutoIRC2P/internal/store"
)

const Iterations = 600000

var (
	ErrCredentials            = errors.New("invalid credentials")
	ErrRegistration           = errors.New("registration unavailable for these details")
	ErrInput                  = errors.New("invalid account details")
	errIdentityPoolSize       = errors.New("invalid I2P destination pool size")
	errIdentityPoolIncomplete = errors.New("incomplete I2P destination pool identity")
	errIdentityPoolPrimary    = errors.New("duplicate primary I2P destination in pool")
	errIdentityPoolDuplicate  = errors.New("duplicate I2P destination in pool")
	nickPattern               = regexp.MustCompile(`^[A-Za-z][A-Za-z0-9_\-]{2,23}$`)
)

type User struct {
	ID   int64  `json:"id"`
	Nick string `json:"nick"`
}

func Public(u store.User) User { return User{ID: u.ID, Nick: u.Nick} }

type Service struct {
	q    *store.Queries
	key  []byte
	aead cipher.AEAD
}

func New(q *store.Queries, key []byte) (*Service, error) {
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	aead, err := cipher.NewGCMWithRandomNonce(block)
	if err != nil {
		return nil, err
	}
	return &Service{q: q, key: append([]byte(nil), key...), aead: aead}, nil
}
func (s *Service) syntheticEmail(nick string) string {
	mac := hmac.New(sha256.New, s.key)
	mac.Write([]byte("synthetic-email\x00"))
	mac.Write([]byte(strings.ToLower(strings.TrimSpace(nick))))
	return hex.EncodeToString(mac.Sum(nil)[:15]) + "@gmail.com"
}
func (s *Service) Salt(ctx context.Context, nick string) (string, error) {
	nick = strings.TrimSpace(nick)
	user, err := s.q.UserByNick(ctx, nick)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return "", err
	}
	email := user.Email
	if errors.Is(err, sql.ErrNoRows) {
		email = s.syntheticEmail(nick)
	}
	// Stored emails bind existing browser proofs and must keep their original salt.
	mac := hmac.New(sha256.New, s.key)
	mac.Write([]byte("browser-salt\x00"))
	mac.Write([]byte(strings.ToLower(strings.TrimSpace(email))))
	return hex.EncodeToString(mac.Sum(nil)[:16]), nil
}
func PasswordDigest(salt, derived []byte) []byte {
	h := sha256.New()
	h.Write(salt)
	h.Write(derived)
	return h.Sum(nil)
}
func decodeProof(proof string) ([]byte, error) {
	b, err := hex.DecodeString(proof)
	if err != nil || len(b) != 32 {
		return nil, ErrInput
	}
	return b, nil
}
func (s *Service) Seal(value []byte, purpose string) []byte {
	return s.aead.Seal(nil, nil, value, []byte(purpose))
}
func (s *Service) Open(value []byte, purpose string) ([]byte, error) {
	return s.aead.Open(nil, nil, value, []byte(purpose))
}
func Token() string { var b [32]byte; rand.Read(b[:]); return hex.EncodeToString(b[:]) }
func (s *Service) Register(ctx context.Context, nick, proof string) (store.User, error) {
	nick = strings.TrimSpace(nick)
	derived, err := decodeProof(proof)
	if err != nil {
		return store.User{}, err
	}
	if !nickPattern.MatchString(nick) || IsServiceNick(nick) {
		return store.User{}, ErrInput
	}
	email := s.syntheticEmail(nick)
	identity, err := irc.GenerateIdentity()
	if err != nil {
		return store.User{}, err
	}
	defer clear(identity.Keys)
	salt := make([]byte, 32)
	rand.Read(salt)
	user, err := s.q.CreateUser(ctx, store.CreateUserParams{Email: email, Nick: nick, PasswordSalt: salt, PasswordHash: PasswordDigest(salt, derived), IrcPassword: s.Seal([]byte(Token()), "irc-password:"+email), IdentityKeys: s.Seal(identity.Keys, "identity:"+email), IdentityAddress: identity.Address, CreatedAt: time.Now().UnixMilli()})
	if err != nil {
		var sqliteErr interface{ Code() int }
		if errors.As(err, &sqliteErr) && sqliteErr.Code()&255 == 19 {
			return store.User{}, ErrRegistration
		}
		return store.User{}, err
	}
	return user, nil
}
func IsServiceNick(nick string) bool {
	switch strings.ToLower(nick) {
	case "nickserv", "chanserv", "memoserv", "operserv", "hostserv", "botserv", "helpserv":
		return true
	}
	return false
}
func (s *Service) Login(ctx context.Context, nick, proof string) (store.User, error) {
	derived, err := decodeProof(proof)
	if err != nil {
		return store.User{}, ErrCredentials
	}
	user, err := s.q.UserByNick(ctx, strings.TrimSpace(nick))
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return store.User{}, err
	}
	salt := user.PasswordSalt
	expected := user.PasswordHash
	if errors.Is(err, sql.ErrNoRows) {
		salt = make([]byte, 32)
		expected = make([]byte, 32)
	}
	equal := subtle.ConstantTimeCompare(PasswordDigest(salt, derived), expected)
	if equal != 1 || err != nil {
		return store.User{}, ErrCredentials
	}
	return user, nil
}
func (s *Service) Session(ctx context.Context, token string) (store.User, error) {
	if len(token) != 64 {
		return store.User{}, ErrCredentials
	}
	hash := sha256.Sum256([]byte(token))
	return s.q.SessionUser(ctx, store.SessionUserParams{TokenHash: hash[:], ExpiresAt: time.Now().Unix()})
}
func (s *Service) CreateSession(ctx context.Context, id int64) (string, error) {
	token := Token()
	hash := sha256.Sum256([]byte(token))
	now := time.Now()
	if err := s.q.PruneSessions(ctx, now.Unix()); err != nil {
		return "", err
	}
	err := s.q.CreateSession(ctx, store.CreateSessionParams{TokenHash: hash[:], UserID: id, ExpiresAt: now.Add(7 * 24 * time.Hour).Unix()})
	return token, err
}
func (s *Service) Logout(ctx context.Context, token string) error {
	hash := sha256.Sum256([]byte(token))
	return s.q.DeleteSession(ctx, hash[:])
}
func (s *Service) Account(ctx context.Context, u store.User) (irc.Account, error) {
	keys, err := s.Open(u.IdentityKeys, "identity:"+u.Email)
	if err != nil {
		return irc.Account{}, err
	}
	account := irc.Account{ID: u.ID, Nick: u.Nick, Email: u.Nick + "@irc.invalid", Identity: irc.Identity{Keys: keys, Address: u.IdentityAddress}, Registered: u.IrcRegistered != 0}
	password, err := s.Open(u.IrcPassword, "irc-password:"+u.Email)
	if err != nil {
		account.ReleaseSensitive()
		return irc.Account{}, err
	}
	defer clear(password)
	pool := u.IdentityPool
	if len(pool) == 0 {
		pool, err = s.q.UserIdentityPool(ctx, u.ID)
		if err != nil {
			account.ReleaseSensitive()
			return irc.Account{}, err
		}
	}
	account.Alternates, err = s.loadIdentityPool(ctx, account.Identity, pool, "identity-pool:"+u.Email, func(ctx context.Context, encrypted []byte) ([]byte, error) {
		return s.q.InitializeUserIdentityPool(ctx, store.InitializeUserIdentityPoolParams{ID: u.ID, IdentityPool: encrypted})
	})
	if err != nil {
		account.ReleaseSensitive()
		return irc.Account{}, err
	}
	account.Password = string(password)
	return account, nil
}
func (s *Service) Observer(ctx context.Context) (irc.Account, error) {
	row, err := s.q.GetObserver(ctx)
	if errors.Is(err, sql.ErrNoRows) {
		identity, identityErr := irc.GenerateIdentity()
		if identityErr != nil {
			return irc.Account{}, identityErr
		}
		err = s.q.CreateObserver(ctx, store.CreateObserverParams{Nick: "guest" + Token()[:10], IdentityKeys: s.Seal(identity.Keys, "observer"), IdentityAddress: identity.Address})
		clear(identity.Keys)
		if err != nil {
			return irc.Account{}, err
		}
		row, err = s.q.GetObserver(ctx)
	}
	if err != nil {
		return irc.Account{}, err
	}
	keys, err := s.Open(row.IdentityKeys, "observer")
	if err != nil {
		return irc.Account{}, err
	}
	account := irc.Account{Nick: row.Nick, Identity: irc.Identity{Keys: keys, Address: row.IdentityAddress}}
	account.Alternates, err = s.loadIdentityPool(ctx, account.Identity, row.IdentityPool, "observer-pool", s.q.InitializeObserverIdentityPool)
	if err != nil {
		account.ReleaseSensitive()
		return irc.Account{}, err
	}
	return account, nil
}

func (s *Service) loadIdentityPool(ctx context.Context, primary irc.Identity, encrypted []byte, purpose string, initialize func(context.Context, []byte) ([]byte, error)) ([]irc.Identity, error) {
	if len(encrypted) != 0 {
		return s.openIdentityPool(primary, encrypted, purpose)
	}
	generated := irc.Account{Alternates: make([]irc.Identity, 0, irc.DestinationPoolSize-1)}
	defer generated.ReleaseSensitive()
	for range irc.DestinationPoolSize - 1 {
		identity, err := irc.GenerateIdentity()
		if err != nil {
			return nil, err
		}
		generated.Alternates = append(generated.Alternates, identity)
	}
	if err := validateIdentityPool(primary, generated.Alternates); err != nil {
		return nil, err
	}
	plaintext, err := json.Marshal(generated.Alternates)
	if err != nil {
		return nil, err
	}
	defer clear(plaintext)
	proposed := s.Seal(plaintext, purpose)
	winner, err := initialize(ctx, proposed)
	if err != nil {
		return nil, err
	}
	if bytes.Equal(winner, proposed) {
		pool := generated.Alternates
		generated.Alternates = nil
		return pool, nil
	}
	return s.openIdentityPool(primary, winner, purpose)
}

func (s *Service) openIdentityPool(primary irc.Identity, encrypted []byte, purpose string) ([]irc.Identity, error) {
	plaintext, err := s.Open(encrypted, purpose)
	if err != nil {
		return nil, err
	}
	defer clear(plaintext)
	var decoded irc.Account
	if err := json.Unmarshal(plaintext, &decoded.Alternates); err != nil {
		decoded.ReleaseSensitive()
		return nil, err
	}
	if err := validateIdentityPool(primary, decoded.Alternates); err != nil {
		decoded.ReleaseSensitive()
		return nil, err
	}
	return decoded.Alternates, nil
}

func validateIdentityPool(primary irc.Identity, pool []irc.Identity) error {
	if len(pool) != irc.DestinationPoolSize-1 {
		return errIdentityPoolSize
	}
	for i, identity := range pool {
		if len(identity.Keys) == 0 || identity.Address == "" {
			return errIdentityPoolIncomplete
		}
		if identity.Address == primary.Address || bytes.Equal(identity.Keys, primary.Keys) {
			return errIdentityPoolPrimary
		}
		for _, previous := range pool[:i] {
			if identity.Address == previous.Address || bytes.Equal(identity.Keys, previous.Keys) {
				return errIdentityPoolDuplicate
			}
		}
	}
	return nil
}
