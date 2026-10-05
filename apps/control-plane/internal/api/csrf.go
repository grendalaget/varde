package api

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/url"
	"strings"

	"github.com/grendalaget/varde/apps/control-plane/internal/api/gen"
	"github.com/grendalaget/varde/apps/control-plane/internal/auth"
)

// sessionCookieMaxAge is 30 days.
const sessionCookieMaxAge = 30 * 24 * 3600

// secureCookies reports whether the session cookie should carry Secure:
// only when the public URL is https.
func (s *Server) secureCookies() bool {
	u, err := url.Parse(s.Cfg.PublicURL)
	return err == nil && u.Scheme == "https"
}

// sessionCookie builds the varde_session Set-Cookie value for a live token
// (maxAge <= 0 produces the expired cookie used on logout).
func (s *Server) sessionCookie(token string, maxAge int) *http.Cookie {
	c := &http.Cookie{
		Name:     auth.SessionCookie,
		Value:    token,
		Path:     "/",
		HttpOnly: true,
		SameSite: http.SameSiteLaxMode,
		MaxAge:   maxAge,
		Secure:   s.secureCookies(),
	}
	return c
}

// cookieAuthPaths get a Set-Cookie on success: login and signup stash the
// token they just issued, logout expires the cookie.
var cookieAuthPaths = map[string]bool{
	"/v1/auth/login":  true,
	"/v1/auth/signup": true,
	"/v1/auth/logout": true,
}

// sessionCookies injects Set-Cookie on the auth endpoints. The strict
// handlers have no access to the ResponseWriter, so responses for those
// three paths are buffered, inspected, then flushed.
func (s *Server) sessionCookies(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !cookieAuthPaths[r.URL.Path] {
			next.ServeHTTP(w, r)
			return
		}
		bw := &bufferedWriter{header: http.Header{}}
		next.ServeHTTP(bw, r)
		status := bw.status
		if status == 0 {
			status = http.StatusOK
		}
		if status >= 200 && status < 300 {
			if r.URL.Path == "/v1/auth/logout" {
				http.SetCookie(w, s.sessionCookie("", -1))
			} else {
				var body struct {
					Token string `json:"token"`
				}
				if json.Unmarshal(bw.buf.Bytes(), &body) == nil && body.Token != "" {
					http.SetCookie(w, s.sessionCookie(body.Token, sessionCookieMaxAge))
				}
			}
		}
		for k, vs := range bw.header {
			for _, v := range vs {
				w.Header().Add(k, v)
			}
		}
		w.WriteHeader(status)
		_, _ = io.Copy(w, &bw.buf)
	})
}

type bufferedWriter struct {
	header http.Header
	buf    bytes.Buffer
	status int
}

func (b *bufferedWriter) Header() http.Header         { return b.header }
func (b *bufferedWriter) WriteHeader(code int)        { b.status = code }
func (b *bufferedWriter) Write(p []byte) (int, error) { return b.buf.Write(p) }

// csrf guards cookie-authenticated unsafe requests: a browser session must
// prove same-origin via Origin (or Referer when Origin is absent). Bearer
// requests (CLI, agent) are exempt — the middleware only fires when the
// session actually resolved from the cookie.
func (s *Server) csrf(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodPost, http.MethodPut, http.MethodPatch, http.MethodDelete:
		default:
			next.ServeHTTP(w, r)
			return
		}
		fromCookie, _ := r.Context().Value(ctxAuthCookie).(bool)
		if !fromCookie {
			next.ServeHTTP(w, r)
			return
		}
		origin := r.Header.Get("Origin")
		if origin == "" {
			origin = r.Header.Get("Referer")
		}
		if origin == "" || !s.originAllowed(origin, r) {
			writeErr(w, http.StatusForbidden, gen.Forbidden, "cross-origin request rejected", nil)
			return
		}
		next.ServeHTTP(w, r)
	})
}

// originAllowed accepts an Origin/Referer whose scheme+host equals the
// configured public URL's origin or the request's own scheme+Host.
func (s *Server) originAllowed(origin string, r *http.Request) bool {
	u, err := url.Parse(origin)
	if err != nil || u.Host == "" {
		return false
	}
	got := u.Scheme + "://" + strings.ToLower(u.Host)
	for _, want := range s.allowedOrigins(r) {
		if got == want {
			return true
		}
	}
	return false
}

func (s *Server) allowedOrigins(r *http.Request) []string {
	var out []string
	if u, err := url.Parse(s.Cfg.PublicURL); err == nil && u.Host != "" {
		out = append(out, u.Scheme+"://"+strings.ToLower(u.Host))
	}
	scheme := "http"
	if r.TLS != nil {
		scheme = "https"
	}
	out = append(out, scheme+"://"+strings.ToLower(r.Host))
	return out
}
