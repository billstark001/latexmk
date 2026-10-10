// Package auth authenticates requests and manages service users and tokens.
package auth

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"net/http"
	"strings"

	"github.com/gin-gonic/gin"

	"github.com/billstark001/latexmk/packages/server/internal/config"
	"github.com/billstark001/latexmk/packages/server/internal/store"
	"github.com/billstark001/latexmk/packages/shared/protocol"
)

// Principal is the authenticated identity and role exposed to request handlers.
// It never contains the submitted credential.
type Principal struct {
	ID   string
	Name string
	Role string
}

type contextKey struct{}

type tokenAuthenticator interface {
	AuthenticateToken(context.Context, string) (store.User, error)
}

type Manager struct {
	cfg config.Config
	db  tokenAuthenticator
}

// New configures authentication. A nil database still permits static and
// bootstrap tokens; other database credentials fail until a store is available.
func New(cfg config.Config, db *store.Postgres) *Manager {
	m := &Manager{cfg: cfg}
	if db != nil {
		m.db = db
	}
	return m
}

// Middleware is the Gin equivalent of Require/RequireAdmin. Keeping the
// principal in request context lets compile workers and handlers share the
// same typed accessor without exposing a raw bearer token.
func (m *Manager) Middleware(admin bool) gin.HandlerFunc {
	return func(c *gin.Context) {
		principal, err := m.Authenticate(c.Request)
		if err != nil {
			c.AbortWithStatusJSON(http.StatusUnauthorized, gin.H{"error": err.Error()})
			return
		}
		if admin && principal.Role != "admin" {
			c.AbortWithStatusJSON(http.StatusForbidden, gin.H{"error": "administrator role required"})
			return
		}
		c.Request = c.Request.WithContext(context.WithValue(c.Request.Context(), contextKey{}, principal))
		c.Next()
	}
}

// Require authenticates each request and adds its Principal to request context.
func (m *Manager) Require(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		principal, err := m.Authenticate(r)
		if err != nil {
			writeAuthError(w, http.StatusUnauthorized, err.Error())
			return
		}
		next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), contextKey{}, principal)))
	})
}

// RequireAdmin additionally rejects authenticated identities without admin role.
func (m *Manager) RequireAdmin(next http.Handler) http.Handler {
	return m.Require(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		principal, _ := FromContext(r.Context())
		if principal.Role != "admin" {
			writeAuthError(w, http.StatusForbidden, "administrator role required")
			return
		}
		next.ServeHTTP(w, r)
	}))
}

// Authenticate resolves the configured identity without modifying the request.
// Credential-bearing modes require exactly one Authorization header value.
func (m *Manager) Authenticate(r *http.Request) (Principal, error) {
	if m.cfg.AuthMode == "none" {
		return Principal{ID: "local", Name: "local", Role: "admin"}, nil
	}
	values := r.Header.Values("Authorization")
	if len(values) != 1 {
		return Principal{}, errors.New("exactly one bearer token is required")
	}
	token, err := bearerToken(values[0])
	if err != nil {
		return Principal{}, err
	}
	switch m.cfg.AuthMode {
	case "token":
		if !constantEqual(token, m.cfg.APIToken) {
			return Principal{}, errors.New("invalid bearer token")
		}
		return Principal{ID: "static", Name: "static-token", Role: "admin"}, nil
	case "postgres", "database":
		if constantEqual(token, m.cfg.BootstrapToken) {
			return Principal{ID: "bootstrap", Name: "bootstrap", Role: "admin"}, nil
		}
		if m.db == nil {
			return Principal{}, errors.New("database is unavailable")
		}
		user, err := m.db.AuthenticateToken(r.Context(), token)
		if err != nil {
			return Principal{}, err
		}
		return Principal{ID: user.ID, Name: user.Name, Role: user.Role}, nil
	default:
		return Principal{}, errors.New("authentication is misconfigured")
	}
}

// FromContext returns the identity inserted by either authentication adapter.
func FromContext(ctx context.Context) (Principal, bool) {
	principal, ok := ctx.Value(contextKey{}).(Principal)
	return principal, ok
}

func bearerToken(header string) (string, error) {
	parts := strings.Fields(header)
	if len(parts) != 2 || !strings.EqualFold(parts[0], "Bearer") || !protocol.ValidBearerToken(parts[1]) {
		return "", errors.New("missing bearer token")
	}
	return parts[1], nil
}

func constantEqual(a, b string) bool {
	ha := sha256.Sum256([]byte(a))
	hb := sha256.Sum256([]byte(b))
	return subtle.ConstantTimeCompare(ha[:], hb[:]) == 1
}

func writeAuthError(w http.ResponseWriter, status int, message string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]string{"error": message})
}
