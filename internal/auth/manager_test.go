package auth

import (
	"errors"
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
	if err := mgr.ChangePassword("admin", "initial-secret-123", "new-password-456"); err != nil {
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
