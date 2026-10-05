// Package auth: users (argon2id), sessions (hashed tokens), signup policy,
// and the Provider seam OIDC can later implement.
package auth

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"

	"golang.org/x/crypto/argon2"

	"github.com/grendalaget/varde/go/ids"

	"github.com/grendalaget/varde/apps/control-plane/internal/store"
)

const (
	SessionCookie = "varde_session"
	sessionTTLms  = 30 * 24 * 3600 * 1000 // 30 days

	argonTime    = 3
	argonMemory  = 64 * 1024
	argonThreads = 2
	argonKeyLen  = 32
)

var ErrBadCredentials = errors.New("invalid email or password")

// SignupPolicy mirrors --signup=open|invite|closed.
type SignupPolicy string

const (
	SignupOpen   SignupPolicy = "open"
	SignupInvite SignupPolicy = "invite"
	SignupClosed SignupPolicy = "closed"
)

// Provider is the seam for future OIDC: everything user-facing goes through
// it. The builtin provider is email+password.
type Provider interface {
	Signup(ctx context.Context, email, password, displayName, inviteCode string) (*store.User, string, error)
	Login(ctx context.Context, email, password string) (*store.User, string, error)
	SessionUser(ctx context.Context, token string) (*store.User, error)
	Logout(ctx context.Context, token string) error
}

// Local is the builtin email+password Provider.
type Local struct {
	Store *store.Store
	// Policy, effective value resolved at call time (open until first user).
	Policy SignupPolicy
}

func hashPassword(pw string) (string, error) {
	salt := make([]byte, 16)
	if _, err := rand.Read(salt); err != nil {
		return "", err
	}
	key := argon2.IDKey([]byte(pw), salt, argonTime, argonMemory, argonThreads, argonKeyLen)
	return fmt.Sprintf("argon2id$%d$%d$%s$%s",
		argonTime, argonMemory,
		base64.RawStdEncoding.EncodeToString(salt),
		base64.RawStdEncoding.EncodeToString(key)), nil
}

func checkPassword(hash, pw string) bool {
	parts := strings.Split(hash, "$")
	if len(parts) != 5 || parts[0] != "argon2id" {
		return false
	}
	var t, m uint32
	if _, err := fmt.Sscanf(parts[1], "%d", &t); err != nil {
		return false
	}
	if _, err := fmt.Sscanf(parts[2], "%d", &m); err != nil {
		return false
	}
	salt, err := base64.RawStdEncoding.DecodeString(parts[3])
	if err != nil {
		return false
	}
	want, err := base64.RawStdEncoding.DecodeString(parts[4])
	if err != nil {
		return false
	}
	got := argon2.IDKey([]byte(pw), salt, t, m, argonThreads, uint32(len(want)))
	return subtle.ConstantTimeCompare(got, want) == 1
}

func tokenHash(token string) string {
	sum := sha256.Sum256([]byte(token))
	return hex.EncodeToString(sum[:])
}

func newToken() (string, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(b), nil
}

// EffectivePolicy: open until the first user exists, then invite.
func (l *Local) effectivePolicy(ctx context.Context) SignupPolicy {
	if l.Policy != "" {
		return l.Policy
	}
	n, err := l.Store.UserCount(ctx)
	if err != nil || n > 0 {
		return SignupInvite
	}
	return SignupOpen
}

func (l *Local) Signup(ctx context.Context, email, password, displayName, inviteCode string) (*store.User, string, error) {
	email = strings.TrimSpace(strings.ToLower(email))
	if email == "" || !strings.Contains(email, "@") {
		return nil, "", fmt.Errorf("validation: invalid email")
	}
	if len(password) < 8 {
		return nil, "", fmt.Errorf("validation: password must be at least 8 characters")
	}
	if displayName == "" {
		displayName = email
	}

	policy := l.effectivePolicy(ctx)
	if policy == SignupClosed {
		return nil, "", fmt.Errorf("forbidden: signup is closed")
	}

	// First user is operator; later signups need a valid invite under 'invite'.
	n, _ := l.Store.UserCount(ctx)
	first := n == 0
	var inv *store.Invite
	if !first && policy == SignupInvite {
		if inviteCode == "" {
			return nil, "", fmt.Errorf("forbidden: invite code required")
		}
		var err error
		inv, err = l.Store.GetInvite(ctx, inviteCode)
		if err != nil {
			return nil, "", fmt.Errorf("forbidden: invalid invite code")
		}
	}

	ph, err := hashPassword(password)
	if err != nil {
		return nil, "", err
	}
	u := &store.User{
		ID:           ids.Must(ids.User),
		Email:        email,
		DisplayName:  displayName,
		PasswordHash: ph,
		CreatedAt:    l.Store.NowMs(),
	}
	if first {
		u.IsOperator = 1
	}
	if err := l.Store.CreateUser(ctx, u); err != nil {
		if store.IsUniqueViolation(err) {
			return nil, "", fmt.Errorf("conflict: email already registered")
		}
		return nil, "", err
	}
	if inv != nil {
		if err := l.Store.ConsumeInvite(ctx, inv.Code); err != nil {
			return nil, "", fmt.Errorf("forbidden: invite expired or exhausted")
		}
		if err := l.Store.AddMember(ctx, inv.GroupID, u.ID, inv.Role, l.Store.NowMs()); err != nil {
			return nil, "", err
		}
	}
	token, err := l.issue(ctx, u.ID)
	return u, token, err
}

func (l *Local) Login(ctx context.Context, email, password string) (*store.User, string, error) {
	u, err := l.Store.UserByEmail(ctx, strings.TrimSpace(strings.ToLower(email)))
	if err != nil || !checkPassword(u.PasswordHash, password) {
		return nil, "", ErrBadCredentials
	}
	token, err := l.issue(ctx, u.ID)
	return u, token, err
}

func (l *Local) issue(ctx context.Context, userID string) (string, error) {
	token, err := newToken()
	if err != nil {
		return "", err
	}
	now := l.Store.NowMs()
	return token, l.Store.CreateSession(ctx, &store.Session{
		TokenHash: tokenHash(token),
		UserID:    userID,
		CreatedAt: now,
		ExpiresAt: now + sessionTTLms,
	})
}

func (l *Local) SessionUser(ctx context.Context, token string) (*store.User, error) {
	if token == "" {
		return nil, ErrBadCredentials
	}
	sess, err := l.Store.SessionByTokenHash(ctx, tokenHash(token))
	if err != nil || sess.ExpiresAt <= l.Store.NowMs() {
		return nil, ErrBadCredentials
	}
	return l.Store.UserByID(ctx, sess.UserID)
}

func (l *Local) Logout(ctx context.Context, token string) error {
	return l.Store.DeleteSession(ctx, tokenHash(token))
}
