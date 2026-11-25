package gateway

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/minhphu05/finsights-backend/internal/security"
)

// The AI service serves its session routes at the root, without a /v1 prefix.
func TestAIChatRoutesMatchAIService(t *testing.T) {
	key := []byte("01234567890123456789012345678901")
	token, _, err := security.Sign(key, "finsights-auth", "user-1", "user@example.com", time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	var seen []string
	aiService := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		seen = append(seen, request.Method+" "+request.URL.RequestURI())
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"session_uid":"s-1","answer":"ok"}`))
	}))
	defer aiService.Close()
	router := New(Clients{}, key, "finsights-auth", nil, aiService.URL)

	for _, test := range []struct {
		name, method, path, body, wantCalls string
	}{
		{name: "new chat", method: http.MethodPost, path: "/api/v1/ai/chat", body: `{"message":"hello"}`, wantCalls: "POST /sessions; POST /sessions/s-1/messages?profile=deepseek"},
		{name: "list", method: http.MethodGet, path: "/api/v1/ai/sessions", wantCalls: "GET /sessions?limit=20"},
		{name: "read", method: http.MethodGet, path: "/api/v1/ai/sessions/s-1/messages", wantCalls: "GET /sessions/s-1"},
		{name: "delete", method: http.MethodDelete, path: "/api/v1/ai/sessions/s-1", wantCalls: "DELETE /sessions/s-1"},
	} {
		t.Run(test.name, func(t *testing.T) {
			seen = nil
			request := httptest.NewRequest(test.method, test.path, strings.NewReader(test.body))
			request.Header.Set("Authorization", "Bearer "+token)
			response := httptest.NewRecorder()
			router.Handler().ServeHTTP(response, request)
			if response.Code != http.StatusOK {
				t.Fatalf("status = %d; body=%s", response.Code, response.Body.String())
			}
			if got := strings.Join(seen, "; "); got != test.wantCalls {
				t.Fatalf("AI service calls = %q, want %q", got, test.wantCalls)
			}
		})
	}
}
