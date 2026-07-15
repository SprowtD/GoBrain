package store

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"time"
)

// OAuth 2.1 persistence for the remote MCP connector. The flow lets an agent add
// the vault by URL alone: the agent registers as a public client (DCR), the user
// authorizes in a browser by pasting a minted GoBrain token once, and the agent
// exchanges the resulting code for an access token bound to that token's identity
// (label + role). Access/refresh tokens are stored only as SHA-256 hashes, like
// the primary token table.
//
// The tables are created in migrate() (store.go) alongside the rest of the schema.

// oauthTokenTTL / oauthCodeTTL bound the lifetime of issued artifacts. Access
// tokens are short-ish and refreshed silently by the client; codes are one-time
// and near-immediate.
const (
	oauthCodeTTL  = 5 * time.Minute
	oauthTokenTTL = 30 * 24 * time.Hour // access token lifetime; refreshed transparently
)

// RegisterOAuthClient records a dynamically-registered public client (RFC 7591)
// and returns a generated client_id. Clients are public (no secret): security
// comes from PKCE + the exact redirect_uri match, per OAuth 2.1 for native apps.
func RegisterOAuthClient(redirectURIs []string, name string) (clientID string, err error) {
	clientID, err = genID()
	if err != nil {
		return "", err
	}
	uris, err := json.Marshal(redirectURIs)
	if err != nil {
		return "", err
	}
	if _, err = db.Exec(
		`INSERT INTO oauth_clients (client_id, redirect_uris, client_name) VALUES (?, ?, ?)`,
		clientID, string(uris), name,
	); err != nil {
		return "", err
	}
	return clientID, nil
}

// OAuthClientRedirectURIs returns the registered redirect URIs for a client, or
// ErrNotFound if the client_id is unknown.
func OAuthClientRedirectURIs(clientID string) ([]string, error) {
	var raw string
	err := db.QueryRow(`SELECT redirect_uris FROM oauth_clients WHERE client_id = ?`, clientID).Scan(&raw)
	if err != nil {
		return nil, ErrNotFound
	}
	var uris []string
	if err := json.Unmarshal([]byte(raw), &uris); err != nil {
		return nil, err
	}
	return uris, nil
}

// CreateOAuthCode mints a one-time authorization code bound to the client, the
// redirect_uri, the PKCE challenge, and the authorizing identity (label + role).
// Returns the raw code; only its hash is stored.
func CreateOAuthCode(clientID, redirectURI, codeChallenge, label, role string) (code string, err error) {
	code, err = randToken()
	if err != nil {
		return "", err
	}
	expires := time.Now().UTC().Add(oauthCodeTTL)
	if _, err = db.Exec(
		`INSERT INTO oauth_codes (code_hash, client_id, redirect_uri, code_challenge, label, role, expires_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?)`,
		hashToken(code), clientID, redirectURI, codeChallenge, label, role, expires,
	); err != nil {
		return "", err
	}
	return code, nil
}

// OAuthCode is a redeemed authorization code's bound data.
type OAuthCode struct {
	ClientID      string
	RedirectURI   string
	CodeChallenge string
	Label         string
	Role          string
}

// ConsumeOAuthCode atomically fetches and deletes an authorization code (codes
// are single-use). Returns ErrNotFound if the code is unknown or expired.
func ConsumeOAuthCode(code string) (OAuthCode, error) {
	var c OAuthCode
	var expires time.Time
	h := hashToken(code)
	err := db.QueryRow(
		`SELECT client_id, redirect_uri, code_challenge, label, role, expires_at
		 FROM oauth_codes WHERE code_hash = ?`, h,
	).Scan(&c.ClientID, &c.RedirectURI, &c.CodeChallenge, &c.Label, &c.Role, &expires)
	if err != nil {
		return OAuthCode{}, ErrNotFound
	}
	db.Exec(`DELETE FROM oauth_codes WHERE code_hash = ?`, h)
	if time.Now().UTC().After(expires) {
		return OAuthCode{}, ErrNotFound
	}
	return c, nil
}

// CreateOAuthToken issues an access token (and refresh token) for an identity.
// Returns the raw secrets; only hashes are stored. expiresIn is the access
// token's remaining lifetime in seconds.
func CreateOAuthToken(clientID, label, role string) (access, refresh string, expiresIn int, err error) {
	access, err = randToken()
	if err != nil {
		return "", "", 0, err
	}
	refresh, err = randToken()
	if err != nil {
		return "", "", 0, err
	}
	expires := time.Now().UTC().Add(oauthTokenTTL)
	if _, err = db.Exec(
		`INSERT INTO oauth_tokens (token_hash, refresh_hash, client_id, label, role, expires_at)
		 VALUES (?, ?, ?, ?, ?, ?)`,
		hashToken(access), hashToken(refresh), clientID, label, role, expires,
	); err != nil {
		return "", "", 0, err
	}
	return access, refresh, int(oauthTokenTTL.Seconds()), nil
}

// VerifyOAuthAccessToken returns the identity behind a non-expired access token.
func VerifyOAuthAccessToken(access string) (label, role string, ok bool) {
	var expires time.Time
	err := db.QueryRow(
		`SELECT label, role, expires_at FROM oauth_tokens WHERE token_hash = ?`, hashToken(access),
	).Scan(&label, &role, &expires)
	if err != nil {
		return "", "", false
	}
	if time.Now().UTC().After(expires) {
		return "", "", false
	}
	return label, role, true
}

// RefreshOAuthToken rotates a refresh token: the old access+refresh pair is
// revoked and a fresh pair is issued for the same identity. Returns ErrNotFound
// if the refresh token is unknown.
func RefreshOAuthToken(refresh string) (access, newRefresh string, expiresIn int, err error) {
	var clientID, label, role string
	h := hashToken(refresh)
	if err = db.QueryRow(
		`SELECT client_id, label, role FROM oauth_tokens WHERE refresh_hash = ?`, h,
	).Scan(&clientID, &label, &role); err != nil {
		return "", "", 0, ErrNotFound
	}
	db.Exec(`DELETE FROM oauth_tokens WHERE refresh_hash = ?`, h)
	return CreateOAuthToken(clientID, label, role)
}

// randToken returns a 256-bit random secret as hex.
func randToken() (string, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}
