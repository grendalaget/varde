package api

import (
	"encoding/json"
	"net/http"
	"strings"
)

// serveFederatedState answers GET /v1/federated/state — a probe the dashboard
// calls before offering federated sign-in. "enabled" says this control plane
// accepts varde.games federation tokens; "site_session" says the browser
// already carries a domain-wide varde.games session cookie, so the dashboard
// can bounce to the site, get a token minted, and come straight back without
// asking for a second login. It answers without auth by design: it only
// reports whether a cookie is present, never its value.
func (s *Server) serveFederatedState(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeErr(w, http.StatusMethodNotAllowed, "validation", "GET only", nil)
		return
	}
	site := false
	for _, c := range r.Cookies() {
		// better-auth names its session cookie <prefix>.session_token, with a
		// __Secure- prefix on https deployments; the domain-wide variant set on
		// .varde.games arrives here on cp.varde.games.
		if strings.HasSuffix(c.Name, ".session_token") {
			site = true
			break
		}
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{
		"enabled":      s.Federation != nil,
		"site_session": site,
	})
}
