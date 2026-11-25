package gateway

import (
	"context"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	finsightsv1 "github.com/minhphu05/finsights-backend/gen/go/finsights/v1"
	"github.com/minhphu05/finsights-backend/internal/security"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

type Clients struct {
	Auth   finsightsv1.AuthServiceClient
	User   finsightsv1.UserServiceClient
	Market finsightsv1.MarketServiceClient
	AI     finsightsv1.AIServiceClient
}

type Router struct {
	clients       Clients
	jwtKey        []byte
	issuer        string
	aiHTTPBaseURL string
	aiHTTPClient  *http.Client
	api           *gin.Engine
}

func New(clients Clients, jwtKey []byte, issuer string, allowedOrigins []string, aiHTTPBaseURL string) *Router {
	gin.SetMode(gin.ReleaseMode)
	router := &Router{
		clients: clients, jwtKey: jwtKey, issuer: issuer, api: gin.New(),
		aiHTTPBaseURL: strings.TrimRight(aiHTTPBaseURL, "/"),
		aiHTTPClient:  &http.Client{Timeout: 15 * time.Second},
	}
	router.api.Use(gin.Recovery())
	router.api.Use(corsMiddleware(allowedOrigins))
	router.api.Use(func(c *gin.Context) {
		c.Request.Body = http.MaxBytesReader(c.Writer, c.Request.Body, 1<<20)
		c.Next()
	})
	router.api.GET("/livez", func(c *gin.Context) { c.JSON(http.StatusOK, gin.H{"status": "alive"}) })
	router.api.GET("/readyz", func(c *gin.Context) { c.JSON(http.StatusOK, gin.H{"status": "ready"}) })

	v1 := router.api.Group("/api/v1")
	v1.POST("/auth/register", router.register)
	v1.POST("/auth/login", router.login)
	v1.POST("/auth/google", router.googleLogin)
	v1.POST("/admin/auth/login", router.adminLogin)
	admin := v1.Group("/admin", router.requireAdminJWT())
	admin.GET("/auth/me", router.adminMe)
	admin.GET("/admins", router.listAdmins)
	admin.POST("/admins", router.createAdmin)
	v1.GET("/stocks", router.listStocks)
	v1.GET("/stocks/:symbol", router.getStock)
	v1.GET("/stocks/:symbol/candles", router.stockCandles)
	v1.GET("/indices/:symbol/candles", router.indexCandles)
	v1.GET("/stocks/:symbol/financials", router.getStockFinancials)
	v1.GET("/stocks/:symbol/disclosures", router.listStockDisclosures)
	v1.GET("/financial-reports", router.listFinancialDocuments)
	v1.GET("/indices", router.listIndices)
	v1.GET("/news", router.listNews)
	v1.GET("/news/:slug", router.getNews)
	ai := v1.Group("/ai", router.requireJWT())
	ai.POST("/chat", router.askAI)
	ai.GET("/sessions", router.listAISessions)
	ai.GET("/sessions/:session_id/messages", router.getAISessionMessages)
	ai.DELETE("/sessions/:session_id", router.deleteAISession)

	private := v1.Group("/users/me", router.requireJWT())
	private.GET("", router.getProfile)
	private.PATCH("", router.updateProfile)
	private.GET("/watchlist", router.listWatchlist)
	private.POST("/watchlist", router.addWatchlist)
	private.DELETE("/watchlist/:symbol", router.removeWatchlist)
	return router
}

func (r *Router) Handler() http.Handler { return r.api }

func (r *Router) register(c *gin.Context) {
	var body finsightsv1.RegisterRequest
	if err := c.ShouldBindJSON(&body); err != nil {
		writeError(c, http.StatusBadRequest, "invalid request body")
		return
	}
	ctx, cancel := context.WithTimeout(c.Request.Context(), 5*time.Second)
	defer cancel()
	response, err := r.clients.Auth.Register(ctx, &body)
	if err != nil {
		writeRPCError(c, err)
		return
	}
	c.JSON(http.StatusAccepted, response)
}

func (r *Router) login(c *gin.Context) {
	var body finsightsv1.LoginRequest
	if err := c.ShouldBindJSON(&body); err != nil {
		writeError(c, http.StatusBadRequest, "invalid request body")
		return
	}
	ctx, cancel := context.WithTimeout(c.Request.Context(), 5*time.Second)
	defer cancel()
	response, err := r.clients.Auth.Login(ctx, &body)
	if err != nil {
		writeRPCError(c, err)
		return
	}
	c.JSON(http.StatusOK, response)
}

func (r *Router) googleLogin(c *gin.Context) {
	var body struct {
		IDToken string `json:"id_token" binding:"required"`
	}
	if err := c.ShouldBindJSON(&body); err != nil {
		writeError(c, http.StatusBadRequest, "Google ID token is required")
		return
	}
	ctx, cancel := context.WithTimeout(c.Request.Context(), 8*time.Second)
	defer cancel()
	response, err := r.clients.Auth.GoogleLogin(ctx, &finsightsv1.GoogleLoginRequest{IdToken: body.IDToken})
	if err != nil {
		writeRPCError(c, err)
		return
	}
	c.JSON(http.StatusOK, response)
}

func (r *Router) adminLogin(c *gin.Context) {
	var body finsightsv1.AdminLoginRequest
	if err := c.ShouldBindJSON(&body); err != nil {
		writeError(c, http.StatusBadRequest, "username and password are required")
		return
	}
	ctx, cancel := context.WithTimeout(c.Request.Context(), 8*time.Second)
	defer cancel()
	response, err := r.clients.Auth.AdminLogin(ctx, &body)
	if err != nil {
		writeRPCError(c, err)
		return
	}
	c.JSON(http.StatusOK, response)
}

func (r *Router) adminMe(c *gin.Context) {
	identity, ok := c.Get("admin_identity")
	if !ok {
		writeError(c, http.StatusUnauthorized, "admin identity is required")
		return
	}
	admin, ok := identity.(security.Identity)
	if !ok {
		writeError(c, http.StatusUnauthorized, "admin identity is invalid")
		return
	}
	c.JSON(http.StatusOK, gin.H{"admin_id": admin.AccountID, "username": admin.Email})
}

func (r *Router) listAdmins(c *gin.Context) {
	ctx := security.ForwardBearer(c.Request.Context(), c.GetHeader("Authorization"))
	ctx, cancel := context.WithTimeout(ctx, 8*time.Second)
	defer cancel()
	response, err := r.clients.Auth.ListAdmins(ctx, &finsightsv1.ListAdminsRequest{})
	if err != nil {
		writeRPCError(c, err)
		return
	}
	c.JSON(http.StatusOK, response)
}

func (r *Router) createAdmin(c *gin.Context) {
	var body finsightsv1.CreateAdminRequest
	if err := c.ShouldBindJSON(&body); err != nil {
		writeError(c, http.StatusBadRequest, "username and password are required")
		return
	}
	identity, ok := c.Get("admin_identity")
	if !ok {
		writeError(c, http.StatusUnauthorized, "admin identity is required")
		return
	}
	admin := identity.(security.Identity)
	body.ActorAdminId = admin.AccountID
	ctx := security.ForwardBearer(c.Request.Context(), c.GetHeader("Authorization"))
	ctx, cancel := context.WithTimeout(ctx, 8*time.Second)
	defer cancel()
	response, err := r.clients.Auth.CreateAdmin(ctx, &body)
	if err != nil {
		writeRPCError(c, err)
		return
	}
	c.JSON(http.StatusCreated, response)
}

func (r *Router) listStocks(c *gin.Context) {
	limit, offset, ok := pagination(c)
	if !ok {
		return
	}
	ctx, cancel := context.WithTimeout(c.Request.Context(), 5*time.Second)
	defer cancel()
	response, err := r.clients.Market.ListStocks(ctx, &finsightsv1.ListStocksRequest{
		Query: c.Query("q"), Exchange: c.Query("exchange"), Sector: c.Query("sector"), Limit: limit, Offset: offset,
	})
	if err != nil {
		writeRPCError(c, err)
		return
	}
	c.JSON(http.StatusOK, response)
}

func (r *Router) getStock(c *gin.Context) {
	ctx, cancel := context.WithTimeout(c.Request.Context(), 5*time.Second)
	defer cancel()
	response, err := r.clients.Market.GetStock(ctx, &finsightsv1.GetStockRequest{Symbol: strings.ToUpper(c.Param("symbol"))})
	if err != nil {
		writeRPCError(c, err)
		return
	}
	c.JSON(http.StatusOK, response)
}

func (r *Router) stockCandles(c *gin.Context) {
	limit, err := parseUint(c.DefaultQuery("limit", "90"), 1000)
	if err != nil {
		writeError(c, http.StatusBadRequest, "invalid limit")
		return
	}
	ctx, cancel := context.WithTimeout(c.Request.Context(), 5*time.Second)
	defer cancel()
	response, err := r.clients.Market.GetStockCandles(ctx, &finsightsv1.GetStockCandlesRequest{
		Symbol: strings.ToUpper(c.Param("symbol")), Range: c.DefaultQuery("range", "1M"), Limit: limit,
	})
	if err != nil {
		writeRPCError(c, err)
		return
	}
	c.JSON(http.StatusOK, response)
}

func (r *Router) indexCandles(c *gin.Context) {
	limit, err := parseUint(c.DefaultQuery("limit", "90"), 1000)
	if err != nil {
		writeError(c, http.StatusBadRequest, "invalid limit")
		return
	}
	ctx, cancel := context.WithTimeout(c.Request.Context(), 5*time.Second)
	defer cancel()
	response, err := r.clients.Market.GetIndexCandles(ctx, &finsightsv1.GetIndexCandlesRequest{
		Symbol: strings.ToUpper(c.Param("symbol")), Range: c.DefaultQuery("range", "1M"), Limit: limit,
	})
	if err != nil {
		writeRPCError(c, err)
		return
	}
	c.JSON(http.StatusOK, response)
}

func (r *Router) getStockFinancials(c *gin.Context) {
	limit, err := parseUint(c.DefaultQuery("limit", "24"), 100)
	if err != nil {
		writeError(c, http.StatusBadRequest, "invalid limit")
		return
	}
	ctx, cancel := context.WithTimeout(c.Request.Context(), 8*time.Second)
	defer cancel()
	response, err := r.clients.Market.GetStockFinancials(ctx, &finsightsv1.GetStockFinancialsRequest{
		Symbol: strings.ToUpper(c.Param("symbol")), PeriodType: c.Query("period_type"), Limit: limit,
	})
	if err != nil {
		writeRPCError(c, err)
		return
	}
	c.JSON(http.StatusOK, response)
}

func (r *Router) listFinancialDocuments(c *gin.Context) {
	limit, offset, ok := pagination(c)
	if !ok {
		return
	}
	ctx, cancel := context.WithTimeout(c.Request.Context(), 8*time.Second)
	defer cancel()
	response, err := r.clients.Market.ListFinancialDocuments(ctx, &finsightsv1.ListFinancialDocumentsRequest{
		Symbol: strings.ToUpper(c.Query("symbol")), Kind: c.Query("kind"), Limit: limit, Offset: offset,
	})
	if err != nil {
		writeRPCError(c, err)
		return
	}
	c.JSON(http.StatusOK, response)
}

func (r *Router) listStockDisclosures(c *gin.Context) {
	limit, offset, ok := pagination(c)
	if !ok {
		return
	}
	ctx, cancel := context.WithTimeout(c.Request.Context(), 8*time.Second)
	defer cancel()
	response, err := r.clients.Market.ListFinancialDocuments(ctx, &finsightsv1.ListFinancialDocumentsRequest{
		Symbol: strings.ToUpper(c.Param("symbol")), Kind: c.Query("kind"), Limit: limit, Offset: offset,
	})
	if err != nil {
		writeRPCError(c, err)
		return
	}
	c.JSON(http.StatusOK, response)
}

func (r *Router) listIndices(c *gin.Context) {
	ctx, cancel := context.WithTimeout(c.Request.Context(), 5*time.Second)
	defer cancel()
	response, err := r.clients.Market.ListIndices(ctx, &finsightsv1.ListIndicesRequest{})
	if err != nil {
		writeRPCError(c, err)
		return
	}
	c.JSON(http.StatusOK, response)
}

func (r *Router) listNews(c *gin.Context) {
	limit, offset, ok := pagination(c)
	if !ok {
		return
	}
	ctx, cancel := context.WithTimeout(c.Request.Context(), 5*time.Second)
	defer cancel()
	response, err := r.clients.Market.ListNews(ctx, &finsightsv1.ListNewsRequest{
		Symbol: strings.ToUpper(c.Query("symbol")), Category: c.Query("category"), Query: c.Query("q"),
		Limit: limit, Offset: offset, Relevance: c.Query("relevance"),
	})
	if err != nil {
		writeRPCError(c, err)
		return
	}
	c.JSON(http.StatusOK, response)
}

func (r *Router) getNews(c *gin.Context) {
	ctx, cancel := context.WithTimeout(c.Request.Context(), 5*time.Second)
	defer cancel()
	response, err := r.clients.Market.GetNews(ctx, &finsightsv1.GetNewsRequest{Slug: c.Param("slug")})
	if err != nil {
		writeRPCError(c, err)
		return
	}
	c.JSON(http.StatusOK, response)
}

func (r *Router) getProfile(c *gin.Context) {
	ctx := security.ForwardBearer(c.Request.Context(), c.GetHeader("Authorization"))
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	response, err := r.clients.User.GetProfile(ctx, &finsightsv1.GetProfileRequest{})
	if err != nil {
		writeRPCError(c, err)
		return
	}
	c.JSON(http.StatusOK, response)
}

func (r *Router) updateProfile(c *gin.Context) {
	var body finsightsv1.UpdateProfileRequest
	if err := c.ShouldBindJSON(&body); err != nil {
		writeError(c, http.StatusBadRequest, "invalid request body")
		return
	}
	ctx := security.ForwardBearer(c.Request.Context(), c.GetHeader("Authorization"))
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	response, err := r.clients.User.UpdateProfile(ctx, &body)
	if err != nil {
		writeRPCError(c, err)
		return
	}
	c.JSON(http.StatusOK, response)
}

func (r *Router) listWatchlist(c *gin.Context) {
	ctx := security.ForwardBearer(c.Request.Context(), c.GetHeader("Authorization"))
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	response, err := r.clients.User.ListWatchlist(ctx, &finsightsv1.ListWatchlistRequest{})
	if err != nil {
		writeRPCError(c, err)
		return
	}
	c.JSON(http.StatusOK, response)
}

func (r *Router) addWatchlist(c *gin.Context) {
	var body finsightsv1.ChangeWatchlistRequest
	if err := c.ShouldBindJSON(&body); err != nil {
		writeError(c, http.StatusBadRequest, "invalid request body")
		return
	}
	ctx := security.ForwardBearer(c.Request.Context(), c.GetHeader("Authorization"))
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	response, err := r.clients.User.AddWatchlistSymbol(ctx, &body)
	if err != nil {
		writeRPCError(c, err)
		return
	}
	c.JSON(http.StatusOK, response)
}

func (r *Router) removeWatchlist(c *gin.Context) {
	ctx := security.ForwardBearer(c.Request.Context(), c.GetHeader("Authorization"))
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	response, err := r.clients.User.RemoveWatchlistSymbol(ctx, &finsightsv1.ChangeWatchlistRequest{Symbol: c.Param("symbol")})
	if err != nil {
		writeRPCError(c, err)
		return
	}
	c.JSON(http.StatusOK, response)
}

func (r *Router) askAI(c *gin.Context) {
	var body finsightsv1.AIAskRequest
	if err := c.ShouldBindJSON(&body); err != nil {
		writeError(c, http.StatusBadRequest, "invalid request body")
		return
	}
	token, ok := security.BearerValue(c.GetHeader("Authorization"))
	if !ok {
		writeError(c, http.StatusUnauthorized, "bearer token required")
		return
	}
	identity, err := security.Parse(r.jwtKey, r.issuer, token)
	if err != nil {
		writeError(c, http.StatusUnauthorized, "invalid access token")
		return
	}
	body.UserId = identity.AccountID
	ctx := security.ForwardBearer(c.Request.Context(), c.GetHeader("Authorization"))
	ctx, cancel := context.WithTimeout(ctx, 60*time.Second)
	defer cancel()
	response, err := r.clients.AI.Ask(ctx, &body)
	if err != nil {
		writeRPCError(c, err)
		return
	}
	c.JSON(http.StatusOK, response)
}

func (r *Router) listAISessions(c *gin.Context) {
	limit, err := parseUint(c.DefaultQuery("limit", "20"), 50)
	if err != nil {
		writeError(c, http.StatusBadRequest, "invalid limit")
		return
	}
	offset, err := parseUint(c.DefaultQuery("offset", "0"), 1000)
	if err != nil {
		writeError(c, http.StatusBadRequest, "invalid offset")
		return
	}
	r.proxyAIHistory(c, http.MethodGet, "/v1/sessions?limit="+strconv.FormatUint(uint64(limit), 10)+"&offset="+strconv.FormatUint(uint64(offset), 10))
}

func (r *Router) getAISessionMessages(c *gin.Context) {
	sessionID := url.PathEscape(strings.TrimSpace(c.Param("session_id")))
	if sessionID == "" {
		writeError(c, http.StatusBadRequest, "session_id is required")
		return
	}
	r.proxyAIHistory(c, http.MethodGet, "/v1/sessions/"+sessionID+"/messages?limit=100")
}

func (r *Router) deleteAISession(c *gin.Context) {
	sessionID := url.PathEscape(strings.TrimSpace(c.Param("session_id")))
	if sessionID == "" {
		writeError(c, http.StatusBadRequest, "session_id is required")
		return
	}
	r.proxyAIHistory(c, http.MethodDelete, "/v1/sessions/"+sessionID)
}

func (r *Router) proxyAIHistory(c *gin.Context, method, path string) {
	if r.aiHTTPBaseURL == "" {
		writeError(c, http.StatusServiceUnavailable, "AI history service is unavailable")
		return
	}
	request, err := http.NewRequestWithContext(c.Request.Context(), method, r.aiHTTPBaseURL+path, nil)
	if err != nil {
		writeError(c, http.StatusInternalServerError, "could not create AI request")
		return
	}
	request.Header.Set("Authorization", c.GetHeader("Authorization"))
	response, err := r.aiHTTPClient.Do(request)
	if err != nil {
		writeError(c, http.StatusServiceUnavailable, "AI history service is unavailable")
		return
	}
	defer response.Body.Close()
	body, err := io.ReadAll(io.LimitReader(response.Body, 1<<20))
	if err != nil {
		writeError(c, http.StatusBadGateway, "could not read AI response")
		return
	}
	contentType := response.Header.Get("Content-Type")
	if contentType == "" {
		contentType = "application/json"
	}
	c.Data(response.StatusCode, contentType, body)
}

func (r *Router) requireJWT() gin.HandlerFunc {
	return func(c *gin.Context) {
		token, ok := security.BearerValue(c.GetHeader("Authorization"))
		if !ok {
			writeError(c, http.StatusUnauthorized, "bearer token required")
			c.Abort()
			return
		}
		identity, err := security.Parse(r.jwtKey, r.issuer, token)
		if err != nil || identity.TokenUse == "admin" {
			writeError(c, http.StatusUnauthorized, "invalid access token")
			c.Abort()
			return
		}
		c.Next()
	}
}

func (r *Router) requireAdminJWT() gin.HandlerFunc {
	return func(c *gin.Context) {
		token, ok := security.BearerValue(c.GetHeader("Authorization"))
		if !ok {
			writeError(c, http.StatusUnauthorized, "admin bearer token required")
			c.Abort()
			return
		}
		identity, err := security.Parse(r.jwtKey, r.issuer, token)
		if err != nil || !security.IsAdmin(identity) {
			writeError(c, http.StatusForbidden, "admin access required")
			c.Abort()
			return
		}
		c.Set("admin_identity", identity)
		c.Next()
	}
}

func pagination(c *gin.Context) (uint32, uint32, bool) {
	limit, err := parseUint(c.DefaultQuery("limit", "30"), 100)
	if err != nil {
		writeError(c, http.StatusBadRequest, "invalid limit")
		return 0, 0, false
	}
	offset, err := parseUint(c.DefaultQuery("offset", "0"), 1_000_000)
	if err != nil {
		writeError(c, http.StatusBadRequest, "invalid offset")
		return 0, 0, false
	}
	return limit, offset, true
}

func parseUint(raw string, max uint32) (uint32, error) {
	value, err := strconv.ParseUint(raw, 10, 32)
	if err != nil || value > uint64(max) {
		return 0, status.Error(codes.InvalidArgument, "value out of range")
	}
	return uint32(value), nil
}

func writeRPCError(c *gin.Context, err error) {
	code := status.Code(err)
	statusCode := http.StatusInternalServerError
	switch code {
	case codes.InvalidArgument:
		statusCode = http.StatusBadRequest
	case codes.Unauthenticated:
		statusCode = http.StatusUnauthorized
	case codes.PermissionDenied:
		statusCode = http.StatusForbidden
	case codes.NotFound:
		statusCode = http.StatusNotFound
	case codes.AlreadyExists:
		statusCode = http.StatusConflict
	case codes.DeadlineExceeded:
		statusCode = http.StatusGatewayTimeout
	case codes.Unavailable, codes.FailedPrecondition:
		statusCode = http.StatusServiceUnavailable
	case codes.ResourceExhausted:
		statusCode = http.StatusTooManyRequests
	case codes.Unimplemented:
		statusCode = http.StatusNotImplemented
	}
	message := status.Convert(err).Message()
	if statusCode >= 500 {
		message = "backend service is temporarily unavailable"
	}
	writeError(c, statusCode, message)
}

func writeError(c *gin.Context, code int, message string) {
	c.JSON(code, gin.H{"error": message})
}
