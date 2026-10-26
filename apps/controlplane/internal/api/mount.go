package api

import (
	"bytes"
	"context"
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
	if s.linkFails == nil {
		s.linkFails = newFailLimiter(deviceLinkFailLimit, deviceLinkFailWindowMs)
	}
	strict := gen.NewStrictHandlerWithOptions(s, nil, gen.StrictHTTPServerOptions{
		RequestErrorHandlerFunc: func(w http.ResponseWriter, r *http.Request, err error) {
			if tooLarge(err) {
				writeErr(w, http.StatusRequestEntityTooLarge, gen.LimitExceeded, "request body too large", nil)
				return
			}
			http.Error(w, err.Error(), http.StatusBadRequest)
		},
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
		r = r.WithContext(withRequestBase(r.Context(), r))
		if strings.HasPrefix(p, "/v1/") {
			// Cap every API body before anything reads it: agentAuth buffers
			// the whole body to verify the signature, and only needs a known
			// node id to get that far.
			r.Body = http.MaxBytesReader(w, r.Body, maxBodyBytes(p))
		}
		if r.Method == http.MethodPatch && p == "/v1/me" {
			r.Body = http.MaxBytesReader(w, r.Body, 64<<10)
			raw, err := io.ReadAll(r.Body)
			if err != nil {
				writeErr(w, http.StatusRequestEntityTooLarge, "validation", "request body too large", nil)
				return
			}
			r.Body = io.NopCloser(bytes.NewReader(raw))
		} else if (r.Method == http.MethodPut || r.Method == http.MethodPost) && strings.Contains(p, "/plugins") {
			r.Body = http.MaxBytesReader(w, r.Body, 64<<10)
		}
		switch {
		case p == "/v1/auth/steam/callback" && r.Method == http.MethodGet:
			s.userAuth(http.HandlerFunc(s.steamCallback)).ServeHTTP(w, r)
		case strings.HasPrefix(p, "/v1/agent/"):
			// enroll endpoints are unsigned; everything else needs the signature
			if strings.HasPrefix(p, "/v1/agent/enroll/") {
				genMux.ServeHTTP(w, r)
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
			s.userAuth(s.csrf(s.sessionCookies(genMux))).ServeHTTP(w, r)
		default:
			spa.ServeHTTP(w, r)
		}
	})
}

// maxBodyBytes is the request body cap for an API path. Crash reports carry
// base64 minidumps (the field-level cap runs later); log batches are up to
// 100 lines of unbounded length; everything else is small JSON.
func maxBodyBytes(path string) int64 {
	switch {
	case path == "/v1/agent/crash-reports":
		return 20 << 20
	case strings.HasPrefix(path, "/v1/agent/executions/") && strings.HasSuffix(path, "/logs"):
		return 4 << 20
	}
	return 1 << 20
}

func tooLarge(err error) bool {
	var mbe *http.MaxBytesError
	return errors.As(err, &mbe)
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
