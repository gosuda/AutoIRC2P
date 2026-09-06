package auth

import (
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"database/sql"
	"encoding/hex"
	"errors"
	"net/mail"
	"regexp"
	"strings"
	"time"

	"github.com/gosuda/AutoIRC2P/internal/irc"
	"github.com/gosuda/AutoIRC2P/internal/store"
)

const Iterations = 600000

var (
	ErrCredentials  = errors.New("invalid credentials")
	ErrRegistration = errors.New("registration unavailable for these details")
	ErrInput        = errors.New("invalid account details")
	nickPattern     = regexp.MustCompile(`^[A-Za-z][A-Za-z0-9_\-]{2,23}$`)
)

type User struct {
	ID    int64  `json:"id"`
	Email string `json:"email"`
	Nick  string `json:"nick"`
}

func Public(u store.User) User { return User{ID: u.ID, Email: u.Email, Nick: u.Nick} }

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
func NormalizeEmail(email string) string { return strings.ToLower(strings.TrimSpace(email)) }
func (s *Service) Salt(email string) string {
	mac := hmac.New(sha256.New, s.key)
	mac.Write([]byte("browser-salt\x00" + NormalizeEmail(email)))
	return hex.EncodeToString(mac.Sum(nil)[:16])
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
func (s *Service) Register(ctx context.Context, email, nick, proof string) (store.User, error) {
	email = NormalizeEmail(email)
	derived, err := decodeProof(proof)
	if err != nil {
		return store.User{}, err
	}
	address, err := mail.ParseAddress(email)
	if err != nil || address.Address != email || len(email) > 254 || !nickPattern.MatchString(nick) || IsServiceNick(nick) {
		return store.User{}, ErrInput
	}
	identity, err := irc.GenerateIdentity()
	if err != nil {
		return store.User{}, err
	}
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
func (s *Service) Login(ctx context.Context, email, proof string) (store.User, error) {
	derived, err := decodeProof(proof)
	if err != nil {
		return store.User{}, ErrCredentials
	}
	user, err := s.q.UserByEmail(ctx, NormalizeEmail(email))
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
func (s *Service) Account(u store.User) (irc.Account, error) {
	keys, err := s.Open(u.IdentityKeys, "identity:"+u.Email)
	if err != nil {
		return irc.Account{}, err
	}
	password, err := s.Open(u.IrcPassword, "irc-password:"+u.Email)
	if err != nil {
		return irc.Account{}, err
	}
	return irc.Account{ID: u.ID, Nick: u.Nick, Password: string(password), Email: u.Nick + "@irc.invalid", Identity: irc.Identity{Keys: keys, Address: u.IdentityAddress}, Registered: u.IrcRegistered != 0}, nil
}
func (s *Service) Observer(ctx context.Context) (irc.Account, error) {
	row, err := s.q.GetObserver(ctx)
	if errors.Is(err, sql.ErrNoRows) {
		identity, err := irc.GenerateIdentity()
		if err != nil {
			return irc.Account{}, err
		}
		nick := "guest" + Token()[:10]
		err = s.q.CreateObserver(ctx, store.CreateObserverParams{Nick: nick, IdentityKeys: s.Seal(identity.Keys, "observer"), IdentityAddress: identity.Address})
		if err != nil {
			return irc.Account{}, err
		}
		return irc.Account{Nick: nick, Identity: identity}, nil
	}
	if err != nil {
		return irc.Account{}, err
	}
	keys, err := s.Open(row.IdentityKeys, "observer")
	if err != nil {
		return irc.Account{}, err
	}
	return irc.Account{Nick: row.Nick, Identity: irc.Identity{Keys: keys, Address: row.IdentityAddress}}, nil
}
