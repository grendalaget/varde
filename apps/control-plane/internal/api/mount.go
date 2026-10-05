package api

import (
	"encoding/json"
	"errors"
	"net/http"
	"strings"

	"github.com/prometheus/client_golang/prometheus/promhttp"

	"github.com/grendalaget/varde/apps/control-plane/internal/api/gen"
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
			// enroll endpoints are unsigned; everything else needs the signature
			if strings.HasPrefix(p, "/v1/agent/enroll/") {
				genMux.ServeHTTP(w, r)
			} else {
				agentSigned.ServeHTTP(w, r)
			}
		case p == "/metrics":
			metrics.ServeHTTP(w, r)
		case strings.HasSuffix(p, "/events/stream") && strings.HasPrefix(p, "/v1/groups/"):
			s.userAuth(http.HandlerFunc(s.ServeSSE)).ServeHTTP(w, r)
		case strings.HasPrefix(p, "/v1/") || p == "/healthz" || p == "/readyz":
			s.userAuth(genMux).ServeHTTP(w, r)
		default:
			spa.ServeHTTP(w, r)
		}
	})
}

var _ = json.Marshal
