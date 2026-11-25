package gateway

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/minhphu05/finsights-backend/internal/security"
)

func TestAIAdminRoutes(t *testing.T) {
	key := []byte("01234567890123456789012345678901")
	adminToken, _, err := security.SignAdmin(key, "finsights-auth", "admin-1", "adminbase", time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	userToken, _, err := security.Sign(key, "finsights-auth", "user-1", "user@example.com", time.Minute)
	if err != nil {
		t.Fatal(err)
	}

	var seen []string
	aiService := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		if request.Header.Get("Authorization") != "" {
			t.Error("the admin token must not reach the AI service")
		}
		body, _ := io.ReadAll(request.Body)
		seen = append(seen, request.Method+" "+request.URL.RequestURI()+" "+string(body))
		if request.Method == http.MethodDelete {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	defer aiService.Close()
	router := New(Clients{}, key, "finsights-auth", nil, aiService.URL)

	for _, test := range []struct {
		name       string
		method     string
		path       string
		body       string
		token      string
		wantStatus int
		wantCall   string
	}{
		{name: "no token", method: http.MethodGet, path: "/api/v1/admin/ai/admin/keys", wantStatus: http.StatusUnauthorized},
		{name: "user token", method: http.MethodPut, path: "/api/v1/admin/ai/admin/keys/deepseek", body: `{"key":"x"}`, token: userToken, wantStatus: http.StatusForbidden},
		{name: "list keys", method: http.MethodGet, path: "/api/v1/admin/ai/admin/keys", token: adminToken, wantStatus: http.StatusOK, wantCall: "GET /admin/keys "},
		{name: "save key", method: http.MethodPut, path: "/api/v1/admin/ai/admin/keys/deepseek", body: `{"key":"sk-test"}`, token: adminToken, wantStatus: http.StatusOK, wantCall: `PUT /admin/keys/deepseek {"key":"sk-test"}`},
		{name: "delete key", method: http.MethodDelete, path: "/api/v1/admin/ai/admin/keys/cloudflare_account_id", token: adminToken, wantStatus: http.StatusNoContent, wantCall: "DELETE /admin/keys/cloudflare_account_id "},
		{name: "health drops the query", method: http.MethodGet, path: "/api/v1/admin/ai/health/providers?live=true", token: adminToken, wantStatus: http.StatusOK, wantCall: "GET /health/providers "},
		{name: "bad provider id", method: http.MethodDelete, path: "/api/v1/admin/ai/admin/keys/a.b", token: adminToken, wantStatus: http.StatusNotFound},
	} {
		t.Run(test.name, func(t *testing.T) {
			seen = nil
			request := httptest.NewRequest(test.method, test.path, strings.NewReader(test.body))
			if test.token != "" {
				request.Header.Set("Authorization", "Bearer "+test.token)
			}
			response := httptest.NewRecorder()
			router.Handler().ServeHTTP(response, request)
			if response.Code != test.wantStatus {
				t.Fatalf("status = %d, want %d; body=%s", response.Code, test.wantStatus, response.Body.String())
			}
			if got := strings.Join(seen, "; "); got != test.wantCall {
				t.Fatalf("AI service call = %q, want %q", got, test.wantCall)
			}
		})
	}
}
