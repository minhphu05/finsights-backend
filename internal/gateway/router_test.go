package gateway

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestHealthAndGoogleLoginValidation(t *testing.T) {
	router := New(Clients{}, []byte("01234567890123456789012345678901"), "finsights-auth", []string{"https://site.example"}, "")

	for _, test := range []struct {
		name       string
		method     string
		path       string
		body       string
		origin     string
		wantStatus int
		wantCORS   string
	}{
		{name: "liveness", method: http.MethodGet, path: "/livez", wantStatus: http.StatusOK},
		{name: "readiness", method: http.MethodGet, path: "/readyz", wantStatus: http.StatusOK},
		{name: "Google login requires an ID token", method: http.MethodPost, path: "/api/v1/auth/google", body: `{}`, wantStatus: http.StatusBadRequest},
		{name: "CORS preflight permits configured frontend", method: http.MethodOptions, path: "/api/v1/stocks", origin: "https://site.example", wantStatus: http.StatusNoContent, wantCORS: "https://site.example"},
		{name: "CORS rejects unknown browser origins", method: http.MethodOptions, path: "/api/v1/stocks", origin: "https://unknown.example", wantStatus: http.StatusForbidden},
	} {
		t.Run(test.name, func(t *testing.T) {
			request := httptest.NewRequest(test.method, test.path, http.NoBody)
			if test.body != "" {
				request = httptest.NewRequest(test.method, test.path, strings.NewReader(test.body))
			}
			if test.origin != "" {
				request.Header.Set("Origin", test.origin)
			}
			response := httptest.NewRecorder()
			router.Handler().ServeHTTP(response, request)
			if response.Code != test.wantStatus {
				t.Fatalf("status = %d, want %d; body=%s", response.Code, test.wantStatus, response.Body.String())
			}
			if got := response.Header().Get("Access-Control-Allow-Origin"); got != test.wantCORS {
				t.Fatalf("CORS origin = %q, want %q", got, test.wantCORS)
			}
		})
	}
}
