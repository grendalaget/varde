// Package api wires the OpenAPI-generated strict server to store, auth,
// scheduler and reconciler, and hosts the auth/agent-signature middleware.
package api

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/arnemolland/p2pgames/go/identity"

	"github.com/arnemolland/p2pgames/apps/control-plane/internal/api/gen"
	"github.com/arnemolland/p2pgames/apps/control-plane/internal/auth"
	"github.com/arnemolland/p2pgames/apps/control-plane/internal/catalog"
	"github.com/arnemolland/p2pgames/apps/control-plane/internal/reconciler"
	"github.com/arnemolland/p2pgames/apps/control-plane/internal/store"
)

// Config carries the flag/env configuration used by handlers.
type Config struct {
	PublicURL string
	Timings   reconciler.Timings
	Relays    []RelayConf
	Version   string
}

// RelayConf is one --relay id=…,addr=… entry.
type RelayConf struct {
	ID   string
	Addr string
}

// Server implements gen.StrictServerInterface.
type Server struct {
	Store    *store.Store
	Auth     auth.Provider
	Cfg      Config
	Recon    *reconciler.Reconciler
	RelayKey ed25519.PrivateKey // CP signing key for relay tokens
	Log      *slog.Logger
}

type ctxKey int

const (
	ctxUser ctxKey = iota
	ctxNode
	ctxToken
)

func userFrom(ctx context.Context) *store.User {
	u, _ := ctx.Value(ctxUser).(*store.User)
	return u
}

func nodeFrom(ctx context.Context) *store.Node {
	n, _ := ctx.Value(ctxNode).(*store.Node)
	return n
}

// ---------- error helpers ----------

func errResp(code gen.ErrorCode, msg string, details map[string]any) error {
	d := details
	if d != nil {
		return &apiError{code: code, msg: msg, details: &d}
	}
	return &apiError{code: code, msg: msg}
}

type apiError struct {
	code    gen.ErrorCode
	msg     string
	details *map[string]any
}

func (e *apiError) Error() string { return e.msg }

func statusFor(code gen.ErrorCode) int {
	switch code {
	case gen.NotFound:
		return http.StatusNotFound
	case gen.Forbidden:
		return http.StatusForbidden
	case gen.Unauthorized:
		return http.StatusUnauthorized
	case gen.StaleEpoch, gen.LatestSaveUnavailable, gen.Conflict:
		return http.StatusConflict
	case gen.LimitExceeded:
		return http.StatusForbidden
	case gen.Validation, gen.Unschedulable:
		return http.StatusBadRequest
	}
	return http.StatusInternalServerError
}

// writeErr renders an apiError (used where strict-server can't).
func writeErr(w http.ResponseWriter, status int, code gen.ErrorCode, msg string, details map[string]any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	body := gen.Error{Code: code, Message: msg, Details: &details}
	if details == nil {
		body.Details = nil
	}
	_ = json.NewEncoder(w).Encode(body)
}

// ---------- auth middleware (public API) ----------

// userAuth resolves cookie or bearer sessions; leaves ctxUser nil when absent.
func (s *Server) userAuth(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		token := ""
		if c, err := r.Cookie(auth.SessionCookie); err == nil {
			token = c.Value
		} else if h := r.Header.Get("Authorization"); strings.HasPrefix(h, "Bearer ") {
			token = strings.TrimPrefix(h, "Bearer ")
		}
		if token != "" {
			if u, err := s.Auth.SessionUser(r.Context(), token); err == nil {
				ctx := context.WithValue(r.Context(), ctxUser, u)
				r = r.WithContext(context.WithValue(ctx, ctxToken, token))
			}
		}
		next.ServeHTTP(w, r)
	})
}

// requireUser returns the authed user or the error response was written.
func (s *Server) requireUser(ctx context.Context) (*store.User, error) {
	u := userFrom(ctx)
	if u == nil {
		return nil, errResp(gen.Unauthorized, "authentication required", nil)
	}
	return u, nil
}

func (s *Server) requireRole(ctx context.Context, groupID string, min string) (*store.User, string, error) {
	u, err := s.requireUser(ctx)
	if err != nil {
		return nil, "", err
	}
	role, err := s.Store.MemberRole(ctx, groupID, u.ID)
	if err != nil {
		if u.IsOperator == 1 {
			return u, "owner", nil
		}
		return nil, "", errResp(gen.Forbidden, "not a member of this group", nil)
	}
	order := map[string]int{"member": 1, "admin": 2, "owner": 3}
	if order[role] < order[min] {
		return nil, "", errResp(gen.Forbidden, "insufficient role", nil)
	}
	return u, role, nil
}

func (s *Server) requireOperator(ctx context.Context) (*store.User, error) {
	u, err := s.requireUser(ctx)
	if err != nil {
		return nil, err
	}
	if u.IsOperator != 1 {
		return nil, errResp(gen.Forbidden, "operator only", nil)
	}
	return u, nil
}

// ---------- agent signature middleware ----------

const agentSkewMs = 60_000

// agentAuth verifies X-P2PG-* headers on /v1/agent calls that need a node.
// Enrollment endpoints (device/token) are unsigned and skip this middleware.
func (s *Server) agentAuth(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		nodeID := r.Header.Get("X-P2PG-Node")
		tsStr := r.Header.Get("X-P2PG-Timestamp")
		sigB64 := r.Header.Get("X-P2PG-Signature")
		if nodeID == "" || tsStr == "" || sigB64 == "" {
			writeErr(w, http.StatusUnauthorized, gen.Unauthorized, "missing signature headers", nil)
			return
		}
		ts, err := strconv.ParseInt(tsStr, 10, 64)
		if err != nil {
			writeErr(w, http.StatusUnauthorized, gen.Unauthorized, "bad timestamp", nil)
			return
		}
		if delta := abs64(s.Store.NowMs() - ts); delta > agentSkewMs {
			writeErr(w, http.StatusUnauthorized, gen.Unauthorized, "timestamp skew too large", nil)
			return
		}
		node, err := s.Store.GetNode(r.Context(), nodeID)
		if err != nil {
			writeErr(w, http.StatusUnauthorized, gen.Unauthorized, "unknown node", nil)
			return
		}
		if node.AdminState == "disabled" {
			writeErr(w, http.StatusForbidden, gen.Forbidden, "node disabled", nil)
			return
		}
		pubBytes, err := base64.StdEncoding.DecodeString(node.PublicKey)
		if err != nil || len(pubBytes) != ed25519.PublicKeySize {
			writeErr(w, http.StatusInternalServerError, gen.Internal, "bad stored public key", nil)
			return
		}
		body, err := io.ReadAll(r.Body)
		if err != nil {
			writeErr(w, http.StatusBadRequest, gen.Validation, "body read failed", nil)
			return
		}
		r.Body = io.NopCloser(bytes.NewReader(body))
		if err := identity.VerifyRequest(pubBytes, r.Method, r.URL.Path, ts, body, sigB64); err != nil {
			writeErr(w, http.StatusUnauthorized, gen.Unauthorized, "invalid signature", nil)
			return
		}
		ctx := context.WithValue(r.Context(), ctxNode, node)
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

func abs64(v int64) int64 {
	if v < 0 {
		return -v
	}
	return v
}

// ---------- plumbing ----------

var _ = json.Marshal
var _ = time.Now
var _ = errors.Is
var _ = catalog.All
