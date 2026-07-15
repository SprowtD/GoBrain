package store

import (
	"path/filepath"
	"testing"
)

func TestOAuthClientLifecycle(t *testing.T) {
	if err := Init(filepath.Join(t.TempDir(), "test.db")); err != nil {
		t.Fatal(err)
	}

	uris := []string{"http://127.0.0.1:41999/callback", "cursor://cb"}
	clientID, err := RegisterOAuthClient(uris, "Test Agent")
	if err != nil || clientID == "" {
		t.Fatalf("RegisterOAuthClient = %q, %v", clientID, err)
	}

	got, err := OAuthClientRedirectURIs(clientID)
	if err != nil {
		t.Fatalf("OAuthClientRedirectURIs: %v", err)
	}
	if len(got) != 2 || got[0] != uris[0] || got[1] != uris[1] {
		t.Fatalf("redirect URIs = %v; want %v", got, uris)
	}

	if _, err := OAuthClientRedirectURIs("nope"); err != ErrNotFound {
		t.Fatalf("unknown client err = %v; want ErrNotFound", err)
	}
}

func TestOAuthCodeIsSingleUseAndBound(t *testing.T) {
	if err := Init(filepath.Join(t.TempDir(), "test.db")); err != nil {
		t.Fatal(err)
	}

	code, err := CreateOAuthCode("client-1", "http://cb", "challenge-abc", "alice", RoleMember)
	if err != nil || code == "" {
		t.Fatalf("CreateOAuthCode = %q, %v", code, err)
	}

	rec, err := ConsumeOAuthCode(code)
	if err != nil {
		t.Fatalf("ConsumeOAuthCode: %v", err)
	}
	if rec.ClientID != "client-1" || rec.RedirectURI != "http://cb" ||
		rec.CodeChallenge != "challenge-abc" || rec.Label != "alice" || rec.Role != RoleMember {
		t.Fatalf("code binding = %+v; unexpected", rec)
	}

	// A code may only be redeemed once.
	if _, err := ConsumeOAuthCode(code); err != ErrNotFound {
		t.Fatalf("second ConsumeOAuthCode err = %v; want ErrNotFound", err)
	}
}

func TestOAuthTokenIssueVerifyRefresh(t *testing.T) {
	if err := Init(filepath.Join(t.TempDir(), "test.db")); err != nil {
		t.Fatal(err)
	}

	access, refresh, expiresIn, err := CreateOAuthToken("client-1", "bob", RoleAdmin)
	if err != nil || access == "" || refresh == "" || expiresIn <= 0 {
		t.Fatalf("CreateOAuthToken = %q/%q/%d/%v", access, refresh, expiresIn, err)
	}

	label, role, ok := VerifyOAuthAccessToken(access)
	if !ok || label != "bob" || role != RoleAdmin {
		t.Fatalf("VerifyOAuthAccessToken = %q/%q/%v; want bob/admin/true", label, role, ok)
	}
	if _, _, ok := VerifyOAuthAccessToken("garbage"); ok {
		t.Fatal("garbage token verified")
	}

	// Refresh rotates the grant: a new pair is issued and the old grant (its
	// access + refresh) is revoked.
	newAccess, newRefresh, _, err := RefreshOAuthToken(refresh)
	if err != nil {
		t.Fatalf("RefreshOAuthToken: %v", err)
	}
	if newAccess == access || newRefresh == refresh {
		t.Fatal("refresh should mint fresh secrets")
	}
	if _, _, ok := VerifyOAuthAccessToken(newAccess); !ok {
		t.Fatal("new access token should verify")
	}
	if _, _, ok := VerifyOAuthAccessToken(access); ok {
		t.Fatal("old access token should be revoked after refresh rotation")
	}
	if _, _, _, err := RefreshOAuthToken(refresh); err != ErrNotFound {
		t.Fatalf("reused refresh err = %v; want ErrNotFound", err)
	}
}
