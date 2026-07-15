package api

import (
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"html/template"
	"net/http"
	"net/url"
	"strings"

	"secondbrain-server/internal/store"
)

// This file implements the minimal OAuth 2.1 authorization server that fronts the
// remote MCP endpoint, so an agent can connect to the vault with only a URL:
//
//	1. agent hits /mcp unauthenticated -> 401 with a resource_metadata pointer
//	2. agent reads /.well-known/oauth-protected-resource -> finds this AS
//	3. agent reads /.well-known/oauth-authorization-server -> finds endpoints
//	4. agent self-registers (POST /oauth/register, RFC 7591 DCR)
//	5. agent opens /oauth/authorize in a browser; the user pastes a GoBrain token
//	   once to authorize (this is the "login" — the project has no accounts)
//	6. agent exchanges the code at /oauth/token with PKCE -> access token
//
// Public clients + PKCE (S256) only; no client secrets, per OAuth 2.1 for native
// and browser-based apps.

// --- discovery metadata ---

// ProtectedResourceMetadata (RFC 9728) tells a client which authorization server
// guards this resource. Served at /.well-known/oauth-protected-resource[/mcp].
func ProtectedResourceMetadata(backendURL string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		base := baseURL(r, backendURL)
		writeJSON(w, map[string]any{
			"resource":                 base + "/mcp",
			"authorization_servers":    []string{base},
			"bearer_methods_supported": []string{"header"},
		})
	}
}

// AuthServerMetadata (RFC 8414) advertises the OAuth endpoints and capabilities.
// Served at /.well-known/oauth-authorization-server.
func AuthServerMetadata(backendURL string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		base := baseURL(r, backendURL)
		writeJSON(w, map[string]any{
			"issuer":                                base,
			"authorization_endpoint":                base + "/oauth/authorize",
			"token_endpoint":                        base + "/oauth/token",
			"registration_endpoint":                 base + "/oauth/register",
			"response_types_supported":              []string{"code"},
			"grant_types_supported":                 []string{"authorization_code", "refresh_token"},
			"code_challenge_methods_supported":      []string{"S256"},
			"token_endpoint_auth_methods_supported": []string{"none"},
			"scopes_supported":                      []string{"mcp"},
		})
	}
}

// --- dynamic client registration (RFC 7591) ---

func RegisterClientHandler(w http.ResponseWriter, r *http.Request) {
	var req struct {
		RedirectURIs []string `json:"redirect_uris"`
		ClientName   string   `json:"client_name"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || len(req.RedirectURIs) == 0 {
		oauthError(w, http.StatusBadRequest, "invalid_client_metadata", "redirect_uris is required")
		return
	}
	clientID, err := store.RegisterOAuthClient(req.RedirectURIs, req.ClientName)
	if err != nil {
		oauthError(w, http.StatusInternalServerError, "server_error", "could not register client")
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusCreated)
	json.NewEncoder(w).Encode(map[string]any{
		"client_id":                  clientID,
		"redirect_uris":              req.RedirectURIs,
		"client_name":                req.ClientName,
		"token_endpoint_auth_method": "none",
		"grant_types":                []string{"authorization_code", "refresh_token"},
		"response_types":             []string{"code"},
	})
}

// --- authorization endpoint + consent page ---

type authParams struct {
	ClientID      string
	RedirectURI   string
	State         string
	CodeChallenge string
	Scope         string
	Resource      string
	Error         string
}

var consentTmpl = template.Must(template.New("consent").Parse(consentHTML))

// AuthorizeHandler renders the consent/authorize page (GET) and processes it
// (POST). The "login" is pasting a valid GoBrain token, which both proves the
// user may write to the vault and pins the grant's identity (label + role).
func AuthorizeHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodGet {
		p, ok := parseAuthorizeQuery(w, r)
		if !ok {
			return
		}
		renderConsent(w, p)
		return
	}

	// POST: the user submitted the consent form.
	if err := r.ParseForm(); err != nil {
		http.Error(w, "bad form", http.StatusBadRequest)
		return
	}
	p := authParams{
		ClientID:      r.FormValue("client_id"),
		RedirectURI:   r.FormValue("redirect_uri"),
		State:         r.FormValue("state"),
		CodeChallenge: r.FormValue("code_challenge"),
		Scope:         r.FormValue("scope"),
		Resource:      r.FormValue("resource"),
	}
	if !validClientRedirect(w, p.ClientID, p.RedirectURI) {
		return
	}

	rawToken := strings.TrimSpace(r.FormValue("token"))
	label, role, ok := store.VerifyToken(rawToken)
	if !ok {
		p.Error = "That token isn't valid. Paste a current GoBrain access token."
		renderConsent(w, p)
		return
	}

	code, err := store.CreateOAuthCode(p.ClientID, p.RedirectURI, p.CodeChallenge, label, role)
	if err != nil {
		http.Error(w, "could not issue code", http.StatusInternalServerError)
		return
	}

	// Redirect back to the client with the authorization code.
	u, _ := url.Parse(p.RedirectURI)
	q := u.Query()
	q.Set("code", code)
	if p.State != "" {
		q.Set("state", p.State)
	}
	u.RawQuery = q.Encode()
	http.Redirect(w, r, u.String(), http.StatusFound)
}

// parseAuthorizeQuery validates the authorization request. A bad client or
// redirect_uri is shown as an on-page error (never redirected to, since the
// redirect target is untrusted until validated); other invalid params redirect
// an OAuth error back to the client per spec.
func parseAuthorizeQuery(w http.ResponseWriter, r *http.Request) (authParams, bool) {
	q := r.URL.Query()
	p := authParams{
		ClientID:      q.Get("client_id"),
		RedirectURI:   q.Get("redirect_uri"),
		State:         q.Get("state"),
		CodeChallenge: q.Get("code_challenge"),
		Scope:         q.Get("scope"),
		Resource:      q.Get("resource"),
	}
	if !validClientRedirect(w, p.ClientID, p.RedirectURI) {
		return authParams{}, false
	}
	if q.Get("response_type") != "code" {
		redirectAuthError(w, r, p, "unsupported_response_type")
		return authParams{}, false
	}
	if p.CodeChallenge == "" || (q.Get("code_challenge_method") != "" && q.Get("code_challenge_method") != "S256") {
		redirectAuthError(w, r, p, "invalid_request")
		return authParams{}, false
	}
	return p, true
}

// validClientRedirect ensures the client exists and the redirect_uri is one it
// registered. On failure it writes a safe error page and returns false.
func validClientRedirect(w http.ResponseWriter, clientID, redirectURI string) bool {
	if clientID == "" || redirectURI == "" {
		http.Error(w, "missing client_id or redirect_uri", http.StatusBadRequest)
		return false
	}
	uris, err := store.OAuthClientRedirectURIs(clientID)
	if err != nil {
		http.Error(w, "unknown client", http.StatusBadRequest)
		return false
	}
	for _, u := range uris {
		if u == redirectURI {
			return true
		}
	}
	http.Error(w, "redirect_uri not registered for this client", http.StatusBadRequest)
	return false
}

func redirectAuthError(w http.ResponseWriter, r *http.Request, p authParams, code string) {
	u, err := url.Parse(p.RedirectURI)
	if err != nil {
		http.Error(w, code, http.StatusBadRequest)
		return
	}
	q := u.Query()
	q.Set("error", code)
	if p.State != "" {
		q.Set("state", p.State)
	}
	u.RawQuery = q.Encode()
	http.Redirect(w, r, u.String(), http.StatusFound)
}

func renderConsent(w http.ResponseWriter, p authParams) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	consentTmpl.Execute(w, p)
}

// --- token endpoint ---

func TokenHandler(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		oauthError(w, http.StatusBadRequest, "invalid_request", "bad form encoding")
		return
	}
	switch r.FormValue("grant_type") {
	case "authorization_code":
		tokenFromCode(w, r)
	case "refresh_token":
		tokenFromRefresh(w, r)
	default:
		oauthError(w, http.StatusBadRequest, "unsupported_grant_type", "grant_type must be authorization_code or refresh_token")
	}
}

func tokenFromCode(w http.ResponseWriter, r *http.Request) {
	code := r.FormValue("code")
	verifier := r.FormValue("code_verifier")
	if code == "" || verifier == "" {
		oauthError(w, http.StatusBadRequest, "invalid_request", "code and code_verifier are required")
		return
	}
	rec, err := store.ConsumeOAuthCode(code)
	if err != nil {
		oauthError(w, http.StatusBadRequest, "invalid_grant", "code is invalid or expired")
		return
	}
	// Bind the code to the same client + redirect_uri it was issued for, and
	// verify PKCE (S256) so a stolen code is useless without the verifier.
	if rec.ClientID != r.FormValue("client_id") ||
		rec.RedirectURI != r.FormValue("redirect_uri") ||
		!verifyPKCE(verifier, rec.CodeChallenge) {
		oauthError(w, http.StatusBadRequest, "invalid_grant", "code binding mismatch")
		return
	}
	access, refresh, expiresIn, err := store.CreateOAuthToken(rec.ClientID, rec.Label, rec.Role)
	if err != nil {
		oauthError(w, http.StatusInternalServerError, "server_error", "could not issue token")
		return
	}
	writeTokenResponse(w, access, refresh, expiresIn)
}

func tokenFromRefresh(w http.ResponseWriter, r *http.Request) {
	refresh := r.FormValue("refresh_token")
	if refresh == "" {
		oauthError(w, http.StatusBadRequest, "invalid_request", "refresh_token is required")
		return
	}
	access, newRefresh, expiresIn, err := store.RefreshOAuthToken(refresh)
	if err != nil {
		oauthError(w, http.StatusBadRequest, "invalid_grant", "refresh_token is invalid")
		return
	}
	writeTokenResponse(w, access, newRefresh, expiresIn)
}

func writeTokenResponse(w http.ResponseWriter, access, refresh string, expiresIn int) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	json.NewEncoder(w).Encode(map[string]any{
		"access_token":  access,
		"token_type":    "Bearer",
		"expires_in":    expiresIn,
		"refresh_token": refresh,
		"scope":         "mcp",
	})
}

// --- helpers ---

func verifyPKCE(verifier, challenge string) bool {
	sum := sha256.Sum256([]byte(verifier))
	return base64.RawURLEncoding.EncodeToString(sum[:]) == challenge
}

func oauthError(w http.ResponseWriter, status int, code, desc string) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(map[string]string{"error": code, "error_description": desc})
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(v)
}

// baseURL is the public origin to advertise in OAuth metadata / redirects. It is
// derived from the host the client actually reached us on (via the proxy
// headers), because for OAuth the issuer and endpoint URLs MUST resolve for that
// client — a stale or localhost BACKEND_URL would otherwise hand back
// unreachable URLs (e.g. http://localhost:8080/oauth/authorize → the client
// connects to its own machine → ConnectionRefused). The configured value is only
// a fallback for the rare case the request carries no Host.
func baseURL(r *http.Request, configured string) string {
	scheme := firstHeaderValue(r.Header.Get("X-Forwarded-Proto"))
	if scheme == "" {
		if r.TLS != nil {
			scheme = "https"
		} else {
			scheme = "http"
		}
	}
	host := firstHeaderValue(r.Header.Get("X-Forwarded-Host"))
	if host == "" {
		host = r.Host
	}
	if host != "" {
		return scheme + "://" + host
	}
	return strings.TrimRight(configured, "/")
}

// firstHeaderValue returns the first entry of a possibly comma-separated proxy
// header (e.g. "X-Forwarded-Proto: https,http").
func firstHeaderValue(v string) string {
	if i := strings.IndexByte(v, ','); i >= 0 {
		v = v[:i]
	}
	return strings.TrimSpace(v)
}

func bearerToken(r *http.Request) string {
	h := r.Header.Get("Authorization")
	if after, ok := strings.CutPrefix(h, "Bearer "); ok {
		return strings.TrimSpace(after)
	}
	return ""
}

const consentHTML = `<!doctype html>
<html lang="en">
<head>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1">
<title>Connect to GoBrain</title>
<style>
  :root { color-scheme: dark light; }
  * { box-sizing: border-box; }
  body { margin:0; min-height:100vh; display:grid; place-items:center;
         font:16px/1.5 -apple-system,BlinkMacSystemFont,"Segoe UI",Roboto,sans-serif;
         background:#0e0f13; color:#e7e9ee; padding:24px; }
  .card { width:100%; max-width:420px; background:#171922; border:1px solid #262a37;
          border-radius:16px; padding:28px; box-shadow:0 12px 40px rgba(0,0,0,.4); }
  h1 { font-size:20px; margin:0 0 6px; display:flex; align-items:center; gap:8px; }
  p  { color:#9aa2b1; margin:0 0 18px; font-size:14px; }
  label { display:block; font-size:13px; color:#c3c9d6; margin:0 0 6px; }
  input[type=text],input[type=password] { width:100%; padding:12px 14px; border-radius:10px;
          border:1px solid #2c3040; background:#0e0f13; color:#e7e9ee; font-size:14px; }
  input:focus { outline:none; border-color:#5b7cfa; }
  button { width:100%; margin-top:18px; padding:12px 14px; border:0; border-radius:10px;
           background:#5b7cfa; color:#fff; font-size:15px; font-weight:600; cursor:pointer; }
  button:hover { background:#4a6bf0; }
  .err { background:#2a1417; border:1px solid #5c2530; color:#f4a9b3; padding:10px 12px;
         border-radius:10px; font-size:13px; margin:0 0 16px; }
  .hint { margin-top:14px; font-size:12px; color:#6b7280; }
</style>
</head>
<body>
  <form class="card" method="post" action="/oauth/authorize">
    <h1>🧠 Connect to GoBrain</h1>
    <p>An agent wants read/write access to your second brain. Paste a GoBrain access token to authorize it.</p>
    {{if .Error}}<div class="err">{{.Error}}</div>{{end}}
    <label for="token">Access token</label>
    <input id="token" name="token" type="password" autocomplete="off" autofocus
           placeholder="64-character token">
    <input type="hidden" name="client_id" value="{{.ClientID}}">
    <input type="hidden" name="redirect_uri" value="{{.RedirectURI}}">
    <input type="hidden" name="state" value="{{.State}}">
    <input type="hidden" name="code_challenge" value="{{.CodeChallenge}}">
    <input type="hidden" name="scope" value="{{.Scope}}">
    <input type="hidden" name="resource" value="{{.Resource}}">
    <button type="submit">Authorize</button>
    <div class="hint">Mint one in the web UI (&ldquo;Invite a teammate&rdquo;) or via the admin API. It's stored only by your agent, never shown again here.</div>
  </form>
</body>
</html>`
