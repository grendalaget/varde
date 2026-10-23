package api

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"

	"github.com/prometheus/client_golang/prometheus/promhttp"

	"github.com/grendalaget/varde/apps/controlplane/internal/api/gen"
)

// NewHandler builds the root handler: agent endpoints behind the signature
// middleware, public API behind session middleware, SSE + metrics + readyz
// as raw handlers, SPA fallback for everything else.
func NewHandler(s *Server, spa http.Handler) http.Handler {
	strict := gen.NewStrictHandlerWithOptions(s, nil, gen.StrictHTTPServerOptions{
		ResponseErrorHandlerFunc: func(w http.ResponseWriter, r *http.Request, err error) {
			var ae *apiError
			if errors.As(err, &ae) {
				var details map[string]any
				if ae.details != nil {
					details = *ae.details
				}
				writeErr(w, statusFor(ae.code), ae.code, ae.msg, details)
				return
			}
			writeErr(w, http.StatusInternalServerError, gen.Internal, "internal error", nil)
		},
	})

	genMux := gen.HandlerWithOptions(strict, gen.StdHTTPServerOptions{})

	// agent endpoints needing signature auth
	agentSigned := s.agentAuth(genMux)

	metrics := promhttp.Handler()

	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		p := r.URL.Path
		switch {
		case strings.HasPrefix(p, "/v1/agent/"):
			// crash reports carry base64 minidumps; cap the request before
			// the strict decoder reads it (the field-level cap runs later).
			if p == "/v1/agent/crash-reports" {
				r.Body = http.MaxBytesReader(w, r.Body, 20<<20)
			}
			// enroll endpoints are unsigned; everything else needs the signature
			if strings.HasPrefix(p, "/v1/agent/enroll/") {
				genMux.ServeHTTP(w, r.WithContext(withRequestBase(r.Context(), r)))
			} else {
				agentSigned.ServeHTTP(w, r)
			}
		case p == "/metrics":
			metrics.ServeHTTP(w, r)
		case p == "/v1/federated/state":
			s.serveFederatedState(w, r)
		case strings.HasSuffix(p, "/events/stream") && strings.HasPrefix(p, "/v1/groups/"):
			s.userAuth(http.HandlerFunc(s.ServeSSE)).ServeHTTP(w, r)
		case strings.HasPrefix(p, "/v1/") || p == "/healthz" || p == "/readyz":
			s.userAuth(s.csrf(s.sessionCookies(captureSteamIDPresence(genMux)))).ServeHTTP(w, r)
		default:
			spa.ServeHTTP(w, r)
		}
	})
}

type steamIDPresenceKey struct{}

func steamIDWasProvided(ctx context.Context) bool {
	provided, _ := ctx.Value(steamIDPresenceKey{}).(bool)
	return provided
}

func captureSteamIDPresence(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/me" && r.Method == http.MethodPatch {
			body, err := io.ReadAll(r.Body)
			if err == nil {
				r.Body = io.NopCloser(bytes.NewReader(body))
				var fields map[string]json.RawMessage
				present := false
				if json.Unmarshal(body, &fields) == nil {
					_, present = fields["steam_id"]
				}
				r = r.WithContext(context.WithValue(r.Context(), steamIDPresenceKey{}, present))
			}
		}
		next.ServeHTTP(w, r)
	})
}

type requestBaseKey struct{}

// withRequestBase records scheme://host of the request so handlers can build
// URLs the caller can reach when no public URL is configured. Only echoed back
// to the same caller, so trusting X-Forwarded-Proto is harmless.
func withRequestBase(ctx context.Context, r *http.Request) context.Context {
	scheme := "http"
	if r.TLS != nil || strings.EqualFold(r.Header.Get("X-Forwarded-Proto"), "https") {
		scheme = "https"
	}
	if r.Host == "" {
		return ctx
	}
	return context.WithValue(ctx, requestBaseKey{}, scheme+"://"+r.Host)
}

// publicBase is the configured public URL, else the request's own base.
func (s *Server) publicBase(ctx context.Context) string {
	if s.Cfg.PublicURL != "" {
		return strings.TrimRight(s.Cfg.PublicURL, "/")
	}
	if b, ok := ctx.Value(requestBaseKey{}).(string); ok {
		return b
	}
	return "http://localhost:8080"
}
