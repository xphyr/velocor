package auth

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/velocor/velocor/internal/store"
)

const (
	// TokenPrefix identifies Personal/Service Access Tokens issued by velod.
	TokenPrefix = "velo_pat_"
	// DefaultAdminUsername is the initial administrator username created on first boot.
	DefaultAdminUsername = "admin"
	// DefaultAdminPassword is the initial bootstrap password if VELOCOR_INIT_PASSWORD is unset.
	DefaultAdminPassword = "velocor-admin-changeme"
	// DefaultSessionTTL is the default validity window for human JWT sessions.
	DefaultSessionTTL = 12 * time.Hour

	pbkdfIterations = 10000
)

var (
	// ErrUnauthorized is returned when credentials or bearer tokens are missing, invalid, or expired.
	ErrUnauthorized = errors.New("unauthorized")
	// ErrForbidden is returned when the authenticated principal lacks the required RBAC role.
	ErrForbidden = errors.New("forbidden: insufficient role permissions")
)

// Claims represents the payload encoded inside a signed HS256 session JWT.
type Claims struct {
	Subject   string     `json:"sub"`
	Role      store.Role `json:"role"`
	IssuedAt  int64      `json:"iat"`
	ExpiresAt int64      `json:"exp"`
}

// Manager manages system cryptographic secrets, local admin credentials,
// JWT session signing, and scoped API tokens (velo_pat_...).
type Manager struct {
	store      store.Store
	jwtSecret  []byte
	sessionTTL time.Duration
}

// NewManager initializes the Auth Manager, bootstrapping SystemConfig and the default
// admin account in the Store if the node has not been initialized yet.
func NewManager(st store.Store, initPassword string) (*Manager, error) {
	cfg, err := st.GetSystemConfig()
	if errors.Is(err, store.ErrNotFound) {
		jwtSecret, err := randomBytes(32)
		if err != nil {
			return nil, fmt.Errorf("generate jwt secret: %w", err)
		}
		masterSalt, err := randomBytes(32)
		if err != nil {
			return nil, fmt.Errorf("generate master salt: %w", err)
		}
		nodeIDBytes, err := randomBytes(8)
		if err != nil {
			return nil, fmt.Errorf("generate node id: %w", err)
		}
		cfg = &store.SystemConfig{
			NodeID:        "velo-" + hex.EncodeToString(nodeIDBytes),
			JWTSecret:     jwtSecret,
			MasterKeySalt: masterSalt,
			InitializedAt: time.Now().UTC(),
		}
		if err := st.SaveSystemConfig(cfg); err != nil {
			return nil, fmt.Errorf("save initial system config: %w", err)
		}
	} else if err != nil {
		return nil, fmt.Errorf("load system config: %w", err)
	}

	if _, err := st.GetAdminUser(DefaultAdminUsername); errors.Is(err, store.ErrNotFound) {
		if initPassword == "" {
			initPassword = DefaultAdminPassword
		}
		hash, err := HashPassword(initPassword)
		if err != nil {
			return nil, fmt.Errorf("hash bootstrap password: %w", err)
		}
		now := time.Now().UTC()
		if err := st.SaveAdminUser(&store.AdminUser{
			Username:     DefaultAdminUsername,
			PasswordHash: hash,
			Role:         store.RoleAdmin,
			CreatedAt:    now,
			UpdatedAt:    now,
		}); err != nil {
			return nil, fmt.Errorf("save bootstrap admin user: %w", err)
		}
	} else if err != nil {
		return nil, fmt.Errorf("load admin user: %w", err)
	}

	return &Manager{
		store:      st,
		jwtSecret:  cfg.JWTSecret,
		sessionTTL: DefaultSessionTTL,
	}, nil
}

// Login verifies username/password credentials and issues a signed HS256 JWT token.
func (m *Manager) Login(username, password string) (string, time.Time, store.Role, error) {
	user, err := m.store.GetAdminUser(username)
	if err != nil {
		return "", time.Time{}, "", ErrUnauthorized
	}
	if !VerifyPassword(password, user.PasswordHash) {
		return "", time.Time{}, "", ErrUnauthorized
	}

	role := user.Role
	if role == "" {
		role = store.RoleAdmin
	}

	now := time.Now().UTC()
	expiresAt := now.Add(m.sessionTTL)
	claims := Claims{
		Subject:   user.Username,
		Role:      role,
		IssuedAt:  now.Unix(),
		ExpiresAt: expiresAt.Unix(),
	}
	token, err := m.signJWT(claims)
	if err != nil {
		return "", time.Time{}, "", err
	}
	return token, expiresAt, role, nil
}

// ChangePassword updates an admin user's password after verifying the current password.
func (m *Manager) ChangePassword(username, currentPassword, newPassword string) error {
	if len(newPassword) < 8 {
		return fmt.Errorf("%w: new password must be at least 8 characters", store.ErrInvalidArgument)
	}
	user, err := m.store.GetAdminUser(username)
	if err != nil {
		return ErrUnauthorized
	}
	if !VerifyPassword(currentPassword, user.PasswordHash) {
		return ErrUnauthorized
	}
	hash, err := HashPassword(newPassword)
	if err != nil {
		return err
	}
	user.PasswordHash = hash
	user.UpdatedAt = time.Now().UTC()
	return m.store.SaveAdminUser(user)
}

// ValidateRole verifies that role is a supported RBAC role.
func ValidateRole(role store.Role) error {
	switch role {
	case store.RoleAdmin, store.RoleCSIProvisioner, store.RoleReadOnly:
		return nil
	default:
		return fmt.Errorf("%w: invalid role %q", store.ErrInvalidArgument, role)
	}
}

// CreateUser provisions a new local user with the given role and password.
func (m *Manager) CreateUser(username, password string, role store.Role) (*store.AdminUser, error) {
	username = strings.TrimSpace(username)
	if username == "" || strings.ContainsAny(username, " \t\r\n:") {
		return nil, fmt.Errorf("%w: username must be non-empty without whitespace or colons", store.ErrInvalidArgument)
	}
	if len(password) < 8 {
		return nil, fmt.Errorf("%w: password must be at least 8 characters", store.ErrInvalidArgument)
	}
	if role == "" {
		role = store.RoleReadOnly
	}
	if err := ValidateRole(role); err != nil {
		return nil, err
	}
	if _, err := m.store.GetAdminUser(username); err == nil {
		return nil, fmt.Errorf("%w: user %q already exists", store.ErrAlreadyExists, username)
	} else if !errors.Is(err, store.ErrNotFound) {
		return nil, err
	}
	hash, err := HashPassword(password)
	if err != nil {
		return nil, err
	}
	now := time.Now().UTC()
	user := &store.AdminUser{
		Username:     username,
		PasswordHash: hash,
		Role:         role,
		CreatedAt:    now,
		UpdatedAt:    now,
	}
	if err := m.store.SaveAdminUser(user); err != nil {
		return nil, err
	}
	return user, nil
}

// UpdateUser updates an existing user's role and/or password.
func (m *Manager) UpdateUser(username string, role *store.Role, newPassword *string) (*store.AdminUser, error) {
	user, err := m.store.GetAdminUser(username)
	if err != nil {
		return nil, err
	}
	if role != nil {
		if err := ValidateRole(*role); err != nil {
			return nil, err
		}
		if user.Role == store.RoleAdmin && *role != store.RoleAdmin {
			if err := m.ensureOtherAdminExists(username); err != nil {
				return nil, err
			}
		}
		user.Role = *role
	}
	if newPassword != nil {
		if len(*newPassword) < 8 {
			return nil, fmt.Errorf("%w: new password must be at least 8 characters", store.ErrInvalidArgument)
		}
		hash, err := HashPassword(*newPassword)
		if err != nil {
			return nil, err
		}
		user.PasswordHash = hash
	}
	user.UpdatedAt = time.Now().UTC()
	if err := m.store.SaveAdminUser(user); err != nil {
		return nil, err
	}
	return user, nil
}

// DeleteUser removes a local user, preventing self-deletion and deletion of the last admin.
func (m *Manager) DeleteUser(username, actingUsername string) error {
	if username == actingUsername {
		return fmt.Errorf("%w: cannot delete the currently signed-in user", store.ErrInvalidArgument)
	}
	user, err := m.store.GetAdminUser(username)
	if err != nil {
		return err
	}
	if user.Role == store.RoleAdmin {
		if err := m.ensureOtherAdminExists(username); err != nil {
			return err
		}
	}
	return m.store.DeleteUser(username)
}

func (m *Manager) ensureOtherAdminExists(excludingUsername string) error {
	users, err := m.store.ListUsers()
	if err != nil {
		return err
	}
	for _, u := range users {
		if u.Username != excludingUsername && u.Role == store.RoleAdmin {
			return nil
		}
	}
	return fmt.Errorf("%w: at least one ADMIN user must remain", store.ErrInUse)
}

// CreateApiToken creates a new scoped API token (velo_pat_<id>_<secret>) and stores
// only its SHA-256 hash in the state store. Returns the metadata and plaintext token string.
func (m *Manager) CreateApiToken(name string, role store.Role, ttl time.Duration) (*store.ApiToken, string, error) {
	if strings.TrimSpace(name) == "" {
		return nil, "", fmt.Errorf("%w: token name is required", store.ErrInvalidArgument)
	}
	if err := ValidateRole(role); err != nil {
		return nil, "", err
	}

	idBytes, err := randomBytes(6)
	if err != nil {
		return nil, "", err
	}
	secretBytes, err := randomBytes(24)
	if err != nil {
		return nil, "", err
	}

	id := hex.EncodeToString(idBytes)
	prefix := TokenPrefix + id
	rawToken := prefix + "_" + hex.EncodeToString(secretBytes)
	hash := hashTokenString(rawToken)

	now := time.Now().UTC()
	var expiresAt *time.Time
	if ttl > 0 {
		exp := now.Add(ttl)
		expiresAt = &exp
	}

	apiTok := &store.ApiToken{
		ID:        id,
		Name:      name,
		Prefix:    prefix,
		TokenHash: hash,
		Role:      role,
		CreatedAt: now,
		ExpiresAt: expiresAt,
	}
	if err := m.store.CreateApiToken(apiTok); err != nil {
		return nil, "", err
	}
	return apiTok, rawToken, nil
}

// AuthenticateBearer validates either a Personal/Service Access Token (velo_pat_...)
// or an HS256 session JWT, returning the authenticated principal subject and RBAC Role.
func (m *Manager) AuthenticateBearer(rawToken string) (string, store.Role, error) {
	rawToken = strings.TrimSpace(rawToken)
	if rawToken == "" {
		return "", "", ErrUnauthorized
	}

	if strings.HasPrefix(rawToken, TokenPrefix) {
		return m.authenticatePAT(rawToken)
	}
	return m.authenticateJWT(rawToken)
}

// Authorize checks whether the granted role satisfies the minimum required role tier.
func Authorize(granted store.Role, required store.Role) error {
	if roleRank(granted) < roleRank(required) {
		return ErrForbidden
	}
	return nil
}

func roleRank(r store.Role) int {
	switch r {
	case store.RoleAdmin:
		return 3
	case store.RoleCSIProvisioner:
		return 2
	case store.RoleReadOnly:
		return 1
	default:
		return 0
	}
}

func (m *Manager) authenticatePAT(rawToken string) (string, store.Role, error) {
	// Format: velo_pat_<id>_<secret>
	rest := strings.TrimPrefix(rawToken, TokenPrefix)
	parts := strings.SplitN(rest, "_", 2)
	if len(parts) != 2 || parts[0] == "" || parts[1] == "" {
		return "", "", ErrUnauthorized
	}
	id := parts[0]

	tok, err := m.store.GetApiToken(id)
	if err != nil {
		return "", "", ErrUnauthorized
	}
	if tok.ExpiresAt != nil && time.Now().UTC().After(*tok.ExpiresAt) {
		return "", "", ErrUnauthorized
	}

	expectedHash := hashTokenString(rawToken)
	if subtle.ConstantTimeCompare([]byte(tok.TokenHash), []byte(expectedHash)) != 1 {
		return "", "", ErrUnauthorized
	}

	_ = m.store.UpdateApiTokenLastUsed(id, time.Now().UTC())
	return "token:" + tok.Name, tok.Role, nil
}

func (m *Manager) signJWT(claims Claims) (string, error) {
	headerJSON := `{"alg":"HS256","typ":"JWT"}`
	claimsJSON, err := json.Marshal(claims)
	if err != nil {
		return "", err
	}
	hEnc := base64.RawURLEncoding.EncodeToString([]byte(headerJSON))
	cEnc := base64.RawURLEncoding.EncodeToString(claimsJSON)
	signingInput := hEnc + "." + cEnc

	mac := hmac.New(sha256.New, m.jwtSecret)
	_, _ = mac.Write([]byte(signingInput))
	sig := base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
	return signingInput + "." + sig, nil
}

func (m *Manager) authenticateJWT(tokenStr string) (string, store.Role, error) {
	parts := strings.Split(tokenStr, ".")
	if len(parts) != 3 {
		return "", "", ErrUnauthorized
	}
	signingInput := parts[0] + "." + parts[1]
	mac := hmac.New(sha256.New, m.jwtSecret)
	_, _ = mac.Write([]byte(signingInput))
	expectedSig := mac.Sum(nil)

	gotSig, err := base64.RawURLEncoding.DecodeString(parts[2])
	if err != nil || !hmac.Equal(gotSig, expectedSig) {
		return "", "", ErrUnauthorized
	}

	claimsBytes, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return "", "", ErrUnauthorized
	}
	var claims Claims
	if err := json.Unmarshal(claimsBytes, &claims); err != nil {
		return "", "", ErrUnauthorized
	}

	if time.Now().UTC().Unix() > claims.ExpiresAt {
		return "", "", ErrUnauthorized
	}
	return claims.Subject, claims.Role, nil
}

// HashPassword computes a salted iterative SHA-256 password hash.
func HashPassword(password string) (string, error) {
	salt, err := randomBytes(16)
	if err != nil {
		return "", err
	}
	derived := derivePasswordKey(password, salt, pbkdfIterations)
	return fmt.Sprintf("v1$%d$%s$%s", pbkdfIterations, hex.EncodeToString(salt), hex.EncodeToString(derived)), nil
}

// VerifyPassword verifies a plaintext password against a stored HashPassword string.
func VerifyPassword(password, encodedHash string) bool {
	parts := strings.Split(encodedHash, "$")
	if len(parts) != 4 || parts[0] != "v1" {
		return false
	}
	var iterations int
	if _, err := fmt.Sscanf(parts[1], "%d", &iterations); err != nil || iterations <= 0 {
		return false
	}
	salt, err := hex.DecodeString(parts[2])
	if err != nil {
		return false
	}
	expected, err := hex.DecodeString(parts[3])
	if err != nil {
		return false
	}
	derived := derivePasswordKey(password, salt, iterations)
	return subtle.ConstantTimeCompare(derived, expected) == 1
}

func derivePasswordKey(password string, salt []byte, iterations int) []byte {
	mac := hmac.New(sha256.New, salt)
	_, _ = mac.Write([]byte(password))
	acc := mac.Sum(nil)
	for i := 1; i < iterations; i++ {
		h := sha256.New()
		_, _ = h.Write(acc)
		_, _ = h.Write(salt)
		acc = h.Sum(nil)
	}
	return acc
}

func hashTokenString(raw string) string {
	sum := sha256.Sum256([]byte(raw))
	return hex.EncodeToString(sum[:])
}

func randomBytes(n int) ([]byte, error) {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		return nil, err
	}
	return b, nil
}
