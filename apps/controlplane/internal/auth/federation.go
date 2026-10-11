// Federation: the hosted control plane trusts identities from varde.games.
// The site signs a short-lived HS256 JWT; the CP verifies it with the shared
// VARDE_FEDERATION_SECRET and upserts a local user (matched by
// website_user_id). Self-hosted CPs never enable this.
package auth

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/golang-jwt/jwt/v5"

	"github.com/grendalaget/varde/go/ids"

	"github.com/grendalaget/varde/apps/controlplane/internal/store"
)

const (
	// Fixed by the federation contract — tokens meant for a different CP or
	// issuer are rejected before anything else is checked.
	FederationIssuer   = "https://varde.games"
	FederationAudience = "cp.varde.games"

	// The site mints 5-minute tokens; allow a little clock skew either way.
	maxTokenAgeS = 330
)

var ErrBadFederationToken = errors.New("invalid federation token")

// Federation verifies varde.games tokens and turns them into CP sessions.
type Federation struct {
	Local  *Local // session issuing is shared with local login
	secret []byte
}

func NewFederation(l *Local, secret string) (*Federation, error) {
	if strings.TrimSpace(secret) == "" {
		return nil, errors.New("VARDE_FEDERATION_SECRET is required in federated mode")
	}
	return &Federation{Local: l, secret: []byte(secret)}, nil
}

type fedClaims struct {
	Email string `json:"email"`
	Name  string `json:"name"`
	jwt.RegisteredClaims
}

func (f *Federation) verify(raw string) (*fedClaims, error) {
	var claims fedClaims
	_, err := jwt.ParseWithClaims(raw, &claims, func(t *jwt.Token) (any, error) {
		return f.secret, nil
	},
		jwt.WithValidMethods([]string{"HS256"}),
		jwt.WithIssuer(FederationIssuer),
		jwt.WithAudience(FederationAudience),
		jwt.WithExpirationRequired())
	if err != nil {
		return nil, ErrBadFederationToken
	}
	if claims.Subject == "" {
		return nil, ErrBadFederationToken
	}
	// Expired already failed validation; also reject tokens with a longer
	// horizon than the contract's 5 minutes (skew included).
	if claims.ExpiresAt != nil &&
		claims.ExpiresAt.Unix() > time.Now().Unix()+maxTokenAgeS {
		return nil, ErrBadFederationToken
	}
	return &claims, nil
}

// LoginExternal verifies the varde.games token, upserts the CP user by
// website_user_id, and issues a normal session like Local.Login does.
func (f *Federation) LoginExternal(ctx context.Context, raw string) (*store.User, string, error) {
	claims, err := f.verify(raw)
	if err != nil {
		return nil, "", err
	}
	st := f.Local.Store
	sub := claims.Subject
	email := strings.TrimSpace(strings.ToLower(claims.Email))
	if email == "" || !strings.Contains(email, "@") {
		return nil, "", ErrBadFederationToken
	}
	name := strings.TrimSpace(claims.Name)
	if name == "" {
		name = email
	}

	if u, err := st.UserByWebsiteID(ctx, sub); err == nil {
		if u.Email != email || u.DisplayName != name {
			if err := st.UpdateUserIdentity(ctx, u.ID, email, name); err != nil {
				return nil, "", err
			}
			u.Email, u.DisplayName = email, name
		}
		token, err := f.Local.issue(ctx, u.ID)
		return u, token, err
	}

	// No federated user yet. A local account holding the same email is not
	// silently merged — the user proves local ownership by logging in.
	if _, err := st.UserByEmail(ctx, email); err == nil {
		return nil, "", fmt.Errorf("conflict: an account with this email already exists on this control plane")
	}

	u := &store.User{
		ID:            ids.Must(ids.User),
		Email:         email,
		DisplayName:   name,
		PasswordHash:  "external", // no local password; checkPassword can't match
		WebsiteUserID: &sub,
		CreatedAt:     st.NowMs(),
	}
	if err := st.CreateUser(ctx, u); err != nil {
		if store.IsUniqueViolation(err) {
			return nil, "", fmt.Errorf("conflict: account already exists")
		}
		return nil, "", err
	}
	token, err := f.Local.issue(ctx, u.ID)
	return u, token, err
}
