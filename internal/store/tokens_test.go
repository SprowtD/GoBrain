package store

import (
	"path/filepath"
	"testing"
)

func TestHasAdminToken(t *testing.T) {
	if err := Init(filepath.Join(t.TempDir(), "test.db")); err != nil {
		t.Fatal(err)
	}

	if ok, err := HasAdminToken(); err != nil || ok {
		t.Fatalf("fresh db: HasAdminToken = %v, %v; want false", ok, err)
	}

	// A member token must not count as admin.
	if _, _, err := CreateToken("member", RoleMember); err != nil {
		t.Fatal(err)
	}
	if ok, _ := HasAdminToken(); ok {
		t.Fatal("member token should not satisfy HasAdminToken")
	}

	// An admin token flips it true.
	id, _, err := CreateToken("admin", RoleAdmin)
	if err != nil {
		t.Fatal(err)
	}
	if ok, _ := HasAdminToken(); !ok {
		t.Fatal("expected HasAdminToken true after minting admin")
	}

	// Revoking the only admin flips it back to false.
	if err := RevokeToken(id); err != nil {
		t.Fatal(err)
	}
	if ok, _ := HasAdminToken(); ok {
		t.Fatal("revoked admin should not count")
	}
}
