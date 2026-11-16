package auth

import (
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/velocor/velocor/internal/store"
)

func TestAuthManagerLoginTokensAndRBAC(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "auth-test.db")
	st, err := store.OpenBoltStore(dbPath)
	if err != nil {
		t.Fatalf("OpenBoltStore failed: %v", err)
	}
	defer func() { _ = st.Close() }()

	mgr, err := NewManager(st, "initial-secret-123")
	if err != nil {
		t.Fatalf("NewManager failed: %v", err)
	}

	// 1. Verify wrong password fails.
	if _, _, _, err := mgr.Login("admin", "wrong"); !errors.Is(err, ErrUnauthorized) {
		t.Fatalf("expected ErrUnauthorized on bad password, got %v", err)
	}

	// 2. Verify valid login issues a working JWT.
	jwtTok, exp, role, err := mgr.Login("admin", "initial-secret-123")
	if err != nil || role != store.RoleAdmin || exp.Before(time.Now()) {
		t.Fatalf("Login failed: tok=%q, role=%s, err=%v", jwtTok, role, err)
	}

	sub, gotRole, err := mgr.AuthenticateBearer(jwtTok)
	if err != nil || sub != "admin" || gotRole != store.RoleAdmin {
		t.Fatalf("AuthenticateBearer(jwt) got (%q, %s, %v)", sub, gotRole, err)
	}

	// 3. Verify password change works.
	if _, err := mgr.ChangePassword("admin", "initial-secret-123", "new-password-456"); err != nil {
		t.Fatalf("ChangePassword failed: %v", err)
	}
	if _, _, _, err := mgr.Login("admin", "initial-secret-123"); !errors.Is(err, ErrUnauthorized) {
		t.Fatalf("expected old password to be rejected after ChangePassword")
	}
	if _, _, _, err := mgr.Login("admin", "new-password-456"); err != nil {
		t.Fatalf("expected new password to succeed: %v", err)
	}

	// 4. Create a CSI provisioner Personal/Service Access Token (velo_pat_...).
	tokMeta, rawSecret, err := mgr.CreateApiToken("k8s-csi", store.RoleCSIProvisioner, time.Hour)
	if err != nil {
		t.Fatalf("CreateApiToken failed: %v", err)
	}
	if !strings.HasPrefix(rawSecret, TokenPrefix) {
		t.Fatalf("expected secret token to start with %s, got %q", TokenPrefix, rawSecret)
	}

	patSub, patRole, err := mgr.AuthenticateBearer(rawSecret)
	if err != nil || patSub != "token:k8s-csi" || patRole != store.RoleCSIProvisioner {
		t.Fatalf("AuthenticateBearer(PAT) got (%q, %s, %v)", patSub, patRole, err)
	}

	// 5. Verify RBAC authorization hierarchy.
	if err := Authorize(patRole, store.RoleReadOnly); err != nil {
		t.Fatalf("expected CSI_PROVISIONER to satisfy READONLY: %v", err)
	}
	if err := Authorize(patRole, store.RoleCSIProvisioner); err != nil {
		t.Fatalf("expected CSI_PROVISIONER to satisfy CSI_PROVISIONER: %v", err)
	}
	if err := Authorize(patRole, store.RoleAdmin); !errors.Is(err, ErrForbidden) {
		t.Fatalf("expected CSI_PROVISIONER to fail ADMIN check with ErrForbidden, got %v", err)
	}

	// 6. Revoke token and verify it is no longer accepted.
	if err := st.DeleteApiToken(tokMeta.ID); err != nil {
		t.Fatalf("DeleteApiToken failed: %v", err)
	}
	if _, _, err := mgr.AuthenticateBearer(rawSecret); !errors.Is(err, ErrUnauthorized) {
		t.Fatalf("expected revoked PAT to fail with ErrUnauthorized, got %v", err)
	}
}

func newTestManager(t *testing.T) (*Manager, *store.BoltStore) {
	t.Helper()
	st, err := store.OpenBoltStore(filepath.Join(t.TempDir(), "a.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	mgr, err := NewManager(st, "bootstrap-pass-1")
	if err != nil {
		t.Fatal(err)
	}
	return mgr, st
}

func TestNoBuiltInDefaultPassword(t *testing.T) {
	st, err := store.OpenBoltStore(filepath.Join(t.TempDir(), "a.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = st.Close() }()
	if _, err := NewManager(st, ""); err == nil {
		t.Fatalf("first start without a bootstrap password must fail rather than use a shared default")
	}
}

func TestBootstrapAdminMustChangePassword(t *testing.T) {
	mgr, _ := newTestManager(t)
	sess, err := mgr.LoginSession("admin", "bootstrap-pass-1")
	if err != nil || !sess.MustChangePassword {
		t.Fatalf("bootstrap admin must be flagged: %+v %v", sess, err)
	}
	p, _ := mgr.Authenticate(sess.Token)
	if !p.MustChangePassword {
		t.Fatalf("principal must carry the flag")
	}
	fresh, err := mgr.ChangePassword("admin", "bootstrap-pass-1", "a-new-password")
	if err != nil {
		t.Fatal(err)
	}
	if p, err := mgr.Authenticate(fresh.Token); err != nil || p.MustChangePassword {
		t.Fatalf("flag must clear after change: %+v %v", p, err)
	}
}

func TestSessionsAreRevokedOnPasswordChangeDemotionAndDeletion(t *testing.T) {
	mgr, _ := newTestManager(t)
	if _, err := mgr.CreateUser("ops", "ops-password-1", store.RoleAdmin); err != nil {
		t.Fatal(err)
	}
	tok, _, _, err := mgr.Login("ops", "ops-password-1")
	if err != nil {
		t.Fatal(err)
	}

	// Demotion takes effect immediately (the token's embedded role is not trusted) and
	// revokes the session.
	ro := store.RoleReadOnly
	if _, err := mgr.UpdateUser("ops", &ro, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := mgr.Authenticate(tok); !errors.Is(err, ErrUnauthorized) {
		t.Fatalf("session must be revoked after role change, got %v", err)
	}
	tok2, _, role, err := mgr.Login("ops", "ops-password-1")
	if err != nil || role != store.RoleReadOnly {
		t.Fatalf("re-login: %s %v", role, err)
	}

	// Deleting the user (and re-creating the name) must not revive the old session.
	if err := mgr.DeleteUser("ops", "admin"); err != nil {
		t.Fatal(err)
	}
	if _, err := mgr.Authenticate(tok2); !errors.Is(err, ErrUnauthorized) {
		t.Fatalf("deleted user's session must be rejected, got %v", err)
	}
	if _, err := mgr.CreateUser("ops", "other-password-9", store.RoleAdmin); err != nil {
		t.Fatal(err)
	}
	if _, err := mgr.Authenticate(tok2); !errors.Is(err, ErrUnauthorized) {
		t.Fatalf("re-created user must not accept the old account's session, got %v", err)
	}
}

func TestLegacyHashIsUpgradedOnLogin(t *testing.T) {
	mgr, st := newTestManager(t)
	salt := []byte("0123456789abcdef")
	legacy := fmt.Sprintf("v1$10000$%x$%x", salt, legacyDerivePasswordKey("legacy-pass-1", salt, 10000))
	if err := st.SaveAdminUser(&store.AdminUser{Username: "old", PasswordHash: legacy, Role: store.RoleReadOnly}); err != nil {
		t.Fatal(err)
	}
	if _, _, _, err := mgr.Login("old", "legacy-pass-1"); err != nil {
		t.Fatalf("legacy hash must still verify: %v", err)
	}
	u, _ := st.GetAdminUser("old")
	if !strings.HasPrefix(u.PasswordHash, "v2$") {
		t.Fatalf("hash not upgraded: %s", u.PasswordHash)
	}
	if _, _, _, err := mgr.Login("old", "legacy-pass-1"); err != nil {
		t.Fatalf("upgraded hash must verify: %v", err)
	}
}
