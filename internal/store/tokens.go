package store

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
)

// Token roles.
const (
	RoleAdmin  = "admin"  // can mint/list/revoke tokens (and everything a member can)
	RoleMember = "member" // can ingest and read status
)

// Token is a listing view of a token (never includes the secret).
type Token struct {
	ID        string
	Label     string
	Role      string
	Revoked   bool
	CreatedAt string
}

// CreateToken mints a new bearer token with the given role: a 256-bit random
// secret returned to the caller exactly once, with only its SHA-256 hash stored.
func CreateToken(label, role string) (id string, rawToken string, err error) {
	if role != RoleAdmin && role != RoleMember {
		role = RoleMember
	}
	id, err = genID()
	if err != nil {
		return "", "", err
	}

	secret := make([]byte, 32)
	if _, err = rand.Read(secret); err != nil {
		return "", "", err
	}
	rawToken = hex.EncodeToString(secret)

	if _, err = db.Exec(
		`INSERT INTO tokens (id, label, token_hash, role) VALUES (?, ?, ?, ?)`,
		id, label, hashToken(rawToken), role,
	); err != nil {
		return "", "", err
	}
	return id, rawToken, nil
}

// VerifyToken returns the token's label and role if it exists and isn't revoked.
func VerifyToken(raw string) (label, role string, ok bool) {
	err := db.QueryRow(
		`SELECT label, role FROM tokens WHERE token_hash = ? AND revoked = 0`,
		hashToken(raw),
	).Scan(&label, &role)
	if err != nil {
		return "", "", false
	}
	return label, role, true
}

// ListTokens returns all tokens (without secrets), newest first.
func ListTokens() ([]Token, error) {
	rows, err := db.Query(
		`SELECT id, label, role, revoked, CAST(created_at AS TEXT) FROM tokens ORDER BY created_at DESC`,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var tokens []Token
	for rows.Next() {
		var (
			t       Token
			revoked int
		)
		if err := rows.Scan(&t.ID, &t.Label, &t.Role, &revoked, &t.CreatedAt); err != nil {
			return nil, err
		}
		t.Revoked = revoked != 0
		tokens = append(tokens, t)
	}
	return tokens, rows.Err()
}

// EnsureAdminToken installs an admin token with a caller-supplied secret if one
// with that secret isn't already present. Lets an operator set a known token via
// env (no log-scraping, works even if another admin already exists). Returns
// whether it created a new row.
func EnsureAdminToken(label, rawToken string) (created bool, err error) {
	var n int
	if err = db.QueryRow(
		`SELECT COUNT(*) FROM tokens WHERE token_hash = ?`, hashToken(rawToken),
	).Scan(&n); err != nil {
		return false, err
	}
	if n > 0 {
		return false, nil
	}
	id, err := genID()
	if err != nil {
		return false, err
	}
	if _, err = db.Exec(
		`INSERT INTO tokens (id, label, token_hash, role) VALUES (?, ?, ?, ?)`,
		id, label, hashToken(rawToken), RoleAdmin,
	); err != nil {
		return false, err
	}
	return true, nil
}

// HasAdminToken reports whether any non-revoked admin token exists. Used by the
// first-boot bootstrap to decide whether to mint one.
func HasAdminToken() (bool, error) {
	var n int
	err := db.QueryRow(
		`SELECT COUNT(*) FROM tokens WHERE role = ? AND revoked = 0`, RoleAdmin,
	).Scan(&n)
	return n > 0, err
}

// RevokeToken marks a token revoked. Returns ErrNotFound if no such token.
func RevokeToken(id string) error {
	res, err := db.Exec(`UPDATE tokens SET revoked = 1 WHERE id = ?`, id)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

func hashToken(raw string) string {
	sum := sha256.Sum256([]byte(raw))
	return hex.EncodeToString(sum[:])
}
