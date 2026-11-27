package gateway

import (
	"context"
	"io"
	"net/http"
	"regexp"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/minhphu05/finsights-backend/internal/security"
)

// Provider key ids of the AI service, e.g. "deepseek" or "cloudflare_account_id". Anything
// else is refused, so a path parameter cannot reach another AI service route.
var aiProviderID = regexp.MustCompile(`^[a-z0-9_]{1,64}$`)

// Ids of the AI service's evaluation runs, checked for the same reason.
var aiEvalRunID = regexp.MustCompile(`^[0-9a-f]{32}$`)

// registerAIAdmin exposes the AI service's key management, provider health and retrieval
// benchmark to admins. The AI service has no authentication of its own: these routes must stay
// behind the admin JWT.
func (r *Router) registerAIAdmin(admin *gin.RouterGroup) {
	ai := admin.Group("/ai")
	ai.GET("/admin/keys", r.listAIKeys)
	ai.PUT("/admin/keys/:provider", r.changeAIKey)
	ai.DELETE("/admin/keys/:provider", r.changeAIKey)
	ai.GET("/health/providers", r.aiProvidersHealth)
	ai.GET("/admin/evals/options", r.aiEvalOptions)
	ai.GET("/admin/evals/retrieval", r.listAIEvalRuns)
	ai.POST("/admin/evals/retrieval", r.startAIEvalRun)
	ai.GET("/admin/evals/retrieval/:run_id", r.getAIEvalRun)
}

func (r *Router) aiEvalOptions(c *gin.Context) {
	r.proxyAIAdmin(c, "/admin/evals/options", nil)
}

func (r *Router) listAIEvalRuns(c *gin.Context) {
	r.proxyAIAdmin(c, "/admin/evals/retrieval", nil)
}

// The AI service answers at once with the run id; the run itself continues there.
func (r *Router) startAIEvalRun(c *gin.Context) {
	body, err := io.ReadAll(c.Request.Body)
	if err != nil {
		writeError(c, http.StatusBadRequest, "invalid request body")
		return
	}
	r.proxyAIAdmin(c, "/admin/evals/retrieval", body)
}

func (r *Router) getAIEvalRun(c *gin.Context) {
	runID := c.Param("run_id")
	if !aiEvalRunID.MatchString(runID) {
		writeError(c, http.StatusNotFound, "unknown run")
		return
	}
	r.proxyAIAdmin(c, "/admin/evals/retrieval/"+runID, nil)
}

func (r *Router) listAIKeys(c *gin.Context) {
	r.proxyAIAdmin(c, "/admin/keys", nil)
}

// The query string is not forwarded: `live=true` would call every LLM provider.
func (r *Router) aiProvidersHealth(c *gin.Context) {
	r.proxyAIAdmin(c, "/health/providers", nil)
}

func (r *Router) changeAIKey(c *gin.Context) {
	provider := c.Param("provider")
	if !aiProviderID.MatchString(provider) {
		writeError(c, http.StatusNotFound, "unknown provider")
		return
	}
	var body []byte
	if c.Request.Method == http.MethodPut {
		var err error
		if body, err = io.ReadAll(c.Request.Body); err != nil {
			writeError(c, http.StatusBadRequest, "invalid request body")
			return
		}
	}
	r.proxyAIAdmin(c, "/admin/keys/"+provider, body)
}

// proxyAIAdmin relays the AI service's status and body. The admin token stays in the gateway,
// and request bodies (they carry keys) are never logged.
func (r *Router) proxyAIAdmin(c *gin.Context, path string, body []byte) {
	if r.aiHTTPBaseURL == "" {
		writeError(c, http.StatusServiceUnavailable, "AI service is unavailable")
		return
	}
	identity, _ := c.Get("admin_identity")
	admin, ok := identity.(security.Identity)
	if !ok {
		writeError(c, http.StatusUnauthorized, "admin identity is required")
		return
	}
	// Provider health calls each provider with a 15 second limit.
	ctx, cancel := context.WithTimeout(c.Request.Context(), 25*time.Second)
	defer cancel()
	status, contentType, response, err := r.doAIRequest(ctx, c.Request.Method, path, body, admin.AccountID)
	if err != nil {
		writeError(c, http.StatusServiceUnavailable, "AI service is unavailable")
		return
	}
	if contentType == "" {
		contentType = "application/json"
	}
	c.Data(status, contentType, response)
}
