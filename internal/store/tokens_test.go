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

func TestEnsureAdminToken(t *testing.T) {
	if err := Init(filepath.Join(t.TempDir(), "test.db")); err != nil {
		t.Fatal(err)
	}

	const secret = "operator-chosen-secret-123"

	// First install creates the token and it verifies as admin.
	created, err := EnsureAdminToken("owner", secret)
	if err != nil || !created {
		t.Fatalf("first EnsureAdminToken = created:%v err:%v; want created:true", created, err)
	}
	if label, role, ok := VerifyToken(secret); !ok || role != RoleAdmin || label != "owner" {
		t.Fatalf("VerifyToken = %q/%q/%v; want owner/admin/true", label, role, ok)
	}

	// Second call is idempotent — no duplicate row.
	created, err = EnsureAdminToken("owner", secret)
	if err != nil || created {
		t.Fatalf("second EnsureAdminToken = created:%v err:%v; want created:false", created, err)
	}

	// Works even when an unrelated admin already exists.
	if _, _, err := CreateToken("other-admin", RoleAdmin); err != nil {
		t.Fatal(err)
	}
	created, err = EnsureAdminToken("owner2", "different-secret")
	if err != nil || !created {
		t.Fatalf("EnsureAdminToken with existing admin = created:%v err:%v; want created:true", created, err)
	}
	if _, role, ok := VerifyToken("different-secret"); !ok || role != RoleAdmin {
		t.Fatalf("second installed token not usable as admin: role:%q ok:%v", role, ok)
	}
}
