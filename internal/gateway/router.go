package gateway

import (
	"context"
	"net/http"
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
	clients Clients
	jwtKey  []byte
	issuer  string
	api     *gin.Engine
}

func New(clients Clients, jwtKey []byte, issuer string, allowedOrigins []string) *Router {
	gin.SetMode(gin.ReleaseMode)
	router := &Router{clients: clients, jwtKey: jwtKey, issuer: issuer, api: gin.New()}
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
	v1.GET("/stocks", router.listStocks)
	v1.GET("/stocks/:symbol", router.getStock)
	v1.GET("/stocks/:symbol/candles", router.stockCandles)
	v1.GET("/indices", router.listIndices)
	v1.GET("/news", router.listNews)
	v1.GET("/news/:slug", router.getNews)
	v1.POST("/ai/chat", router.askAI)

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
		Symbol: strings.ToUpper(c.Query("symbol")), Category: c.Query("category"), Query: c.Query("q"), Limit: limit, Offset: offset,
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
	if authHeader := c.GetHeader("Authorization"); authHeader != "" {
		token, ok := security.BearerValue(authHeader)
		if !ok {
			writeError(c, http.StatusUnauthorized, "invalid bearer token")
			return
		}
		identity, err := security.Parse(r.jwtKey, r.issuer, token)
		if err != nil {
			writeError(c, http.StatusUnauthorized, "invalid access token")
			return
		}
		body.UserId = identity.AccountID
	}
	ctx, cancel := context.WithTimeout(c.Request.Context(), 30*time.Second)
	defer cancel()
	response, err := r.clients.AI.Ask(ctx, &body)
	if err != nil {
		writeRPCError(c, err)
		return
	}
	c.JSON(http.StatusOK, response)
}

func (r *Router) requireJWT() gin.HandlerFunc {
	return func(c *gin.Context) {
		token, ok := security.BearerValue(c.GetHeader("Authorization"))
		if !ok {
			writeError(c, http.StatusUnauthorized, "bearer token required")
			c.Abort()
			return
		}
		if _, err := security.Parse(r.jwtKey, r.issuer, token); err != nil {
			writeError(c, http.StatusUnauthorized, "invalid access token")
			c.Abort()
			return
		}
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
