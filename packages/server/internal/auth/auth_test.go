package auth

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"

	"github.com/billstark001/latexmk/packages/server/internal/config"
	"github.com/billstark001/latexmk/packages/server/internal/store"
)

func TestAuthenticateModes(t *testing.T) {
	for _, test := range []struct {
		name, mode, header, wantID string
		wantError                  bool
	}{
		{"disabled", "none", "", "local", false},
		{"static", "token", "Bearer correct", "static", false},
		{"scheme case", "token", "bEaReR correct", "static", false},
		{"missing", "token", "", "", true},
		{"wrong scheme", "token", "Basic correct", "", true},
		{"wrong token", "token", "Bearer wrong", "", true},
		{"extra field", "token", "Bearer correct extra", "", true},
		{"postgres bootstrap", "postgres", "Bearer bootstrap", "bootstrap", false},
		{"database alias", "database", "Bearer bootstrap", "bootstrap", false},
		{"database unavailable", "postgres", "Bearer other", "", true},
		{"misconfigured", "unknown", "Bearer correct", "", true},
	} {
		t.Run(test.name, func(t *testing.T) {
			manager := New(config.Config{AuthMode: test.mode, APIToken: "correct", BootstrapToken: "bootstrap"}, nil)
			request := httptest.NewRequest(http.MethodGet, "/", nil)
			request.Header.Set("Authorization", test.header)
			principal, err := manager.Authenticate(request)
			if (err != nil) != test.wantError || principal.ID != test.wantID {
				t.Fatalf("principal=%+v, error=%v", principal, err)
			}
		})
	}
}

func TestRejectsAmbiguousAuthorization(t *testing.T) {
	manager := New(config.Config{AuthMode: "token", APIToken: "correct"}, nil)
	request := httptest.NewRequest(http.MethodGet, "/", nil)
	request.Header.Add("Authorization", "Bearer correct")
	request.Header.Add("Authorization", "Bearer wrong")
	if _, err := manager.Authenticate(request); err == nil {
		t.Fatal("accepted multiple Authorization values")
	}
}

func TestAuthenticationAdapters(t *testing.T) {
	gin.SetMode(gin.TestMode)
	for _, adapter := range []string{"http", "gin"} {
		for _, test := range []struct {
			name, header string
			admin        bool
			status       int
		}{
			{"accepted", "Bearer correct", false, http.StatusOK},
			{"administrator", "Bearer correct", true, http.StatusOK},
			{"unauthenticated", "Bearer submitted-secret", true, http.StatusUnauthorized},
		} {
			t.Run(adapter+"/"+test.name, func(t *testing.T) {
				manager := New(config.Config{AuthMode: "token", APIToken: "correct"}, nil)
				reached := false
				serve := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					principal, ok := FromContext(r.Context())
					if !ok || principal.ID != "static" {
						t.Errorf("missing principal: %+v", principal)
					}
					reached = true
					w.WriteHeader(http.StatusOK)
				})
				var handler http.Handler
				if adapter == "http" {
					if test.admin {
						handler = manager.RequireAdmin(serve)
					} else {
						handler = manager.Require(serve)
					}
				} else {
					router := gin.New()
					router.GET(
						"/",
						manager.Middleware(test.admin),
						func(c *gin.Context) { serve.ServeHTTP(c.Writer, c.Request) },
					)
					handler = router
				}
				request := httptest.NewRequest(http.MethodGet, "/", nil)
				request.Header.Set("Authorization", test.header)
				recorder := httptest.NewRecorder()
				handler.ServeHTTP(recorder, request)
				if recorder.Code != test.status || reached != (test.status == http.StatusOK) {
					t.Fatalf("status=%d reached=%v", recorder.Code, reached)
				}
				if test.status != http.StatusOK &&
					(strings.Contains(recorder.Body.String(), "submitted-secret") || !strings.HasPrefix(recorder.Header().Get("Content-Type"), "application/json")) {
					t.Fatalf("unsafe error: %s", recorder.Body.String())
				}
			})
		}
	}
	if _, ok := FromContext(context.Background()); ok {
		t.Fatal("empty context has a principal")
	}
}

type testAuthenticator struct {
	user store.User
	err  error
}

func (a testAuthenticator) AuthenticateToken(context.Context, string) (store.User, error) {
	return a.user, a.err
}

func TestDatabaseRolesAndErrors(t *testing.T) {
	gin.SetMode(gin.TestMode)
	for _, adapter := range []string{"http", "gin"} {
		for _, test := range []struct {
			name, role string
			dbError    error
			status     int
		}{
			{"admin", "admin", nil, http.StatusOK},
			{"member", "member", nil, http.StatusForbidden},
			{"disabled", "", errors.New("user is disabled"), http.StatusUnauthorized},
		} {
			t.Run(adapter+"/"+test.name, func(t *testing.T) {
				manager := &Manager{
					cfg: config.Config{AuthMode: "postgres"},
					db: testAuthenticator{
						user: store.User{ID: "usr-test", Name: "test", Role: test.role},
						err:  test.dbError,
					},
				}
				serve := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					principal, ok := FromContext(r.Context())
					if !ok || principal.ID != "usr-test" {
						t.Errorf("principal=%+v", principal)
					}
					w.WriteHeader(http.StatusOK)
				})
				handler := manager.RequireAdmin(serve)
				if adapter == "gin" {
					router := gin.New()
					router.GET(
						"/",
						manager.Middleware(true),
						func(c *gin.Context) { serve.ServeHTTP(c.Writer, c.Request) },
					)
					handler = router
				}
				request := httptest.NewRequest(http.MethodGet, "/", nil)
				request.Header.Set("Authorization", "Bearer database-token")
				recorder := httptest.NewRecorder()
				handler.ServeHTTP(recorder, request)
				if recorder.Code != test.status {
					t.Fatalf("status=%d body=%s", recorder.Code, recorder.Body.String())
				}
			})
		}
	}
}
