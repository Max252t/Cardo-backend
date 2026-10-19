package httpapi

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"

	"github.com/Max252t/Cardo-backend/internal/auth"
	"github.com/Max252t/Cardo-backend/internal/domain"
)

type AuthService interface {
	Register(ctx context.Context, in auth.RegisterInput) (auth.Session, error)
	Login(ctx context.Context, in auth.LoginInput) (auth.Session, error)
	Refresh(ctx context.Context, refreshToken string) (auth.TokenPair, error)
	Logout(ctx context.Context, userID uuid.UUID, refreshToken string) error
}

type TokenVerifier interface {
	Verify(token string) (uuid.UUID, error)
}

type UserService interface {
	Get(ctx context.Context, id uuid.UUID) (domain.User, error)
	UpdateProfile(ctx context.Context, id uuid.UUID, upd domain.ProfileUpdate) (domain.User, error)
}

type BoardService interface {
	List(ctx context.Context, userID uuid.UUID) ([]domain.BoardWithRole, error)
	Create(ctx context.Context, userID uuid.UUID, title string) (domain.BoardWithRole, error)
	Rename(ctx context.Context, userID, boardID uuid.UUID, title string) (domain.BoardWithRole, domain.Event, error)
	Delete(ctx context.Context, userID, boardID uuid.UUID) (domain.Event, error)
}

type Checker interface {
	Ping(ctx context.Context) error
}

type CheckerFunc func(ctx context.Context) error

func (f CheckerFunc) Ping(ctx context.Context) error {
	return f(ctx)
}

type Deps struct {
	Logger         *slog.Logger
	Auth           AuthService
	Tokens         TokenVerifier
	Users          UserService
	Boards         BoardService
	Readiness      map[string]Checker
	TrustedProxies []string
	MaxBodyBytes   int64
}

type handlers struct {
	logger    *slog.Logger
	auth      AuthService
	tokens    TokenVerifier
	users     UserService
	boards    BoardService
	readiness map[string]Checker
}

const defaultMaxBodyBytes = 1 << 20

func NewRouter(d Deps) (*gin.Engine, error) {
	if d.Auth == nil || d.Tokens == nil || d.Users == nil || d.Boards == nil {
		return nil, errors.New("httpapi: auth, tokens, users and boards dependencies are required")
	}
	if d.Logger == nil {
		d.Logger = slog.New(slog.DiscardHandler)
	}
	if d.MaxBodyBytes <= 0 {
		d.MaxBodyBytes = defaultMaxBodyBytes
	}
	h := &handlers{
		logger:    d.Logger,
		auth:      d.Auth,
		tokens:    d.Tokens,
		users:     d.Users,
		boards:    d.Boards,
		readiness: d.Readiness,
	}

	r := gin.New()
	r.HandleMethodNotAllowed = true
	r.ContextWithFallback = true
	if err := r.SetTrustedProxies(d.TrustedProxies); err != nil {
		return nil, fmt.Errorf("httpapi: trusted proxies: %w", err)
	}
	r.Use(requestIDMiddleware(), h.logMiddleware(), h.recoveryMiddleware(), bodyLimitMiddleware(d.MaxBodyBytes))
	r.NoRoute(func(c *gin.Context) {
		abortWithError(c, http.StatusNotFound, CodeNotFound, "route not found")
	})
	r.NoMethod(func(c *gin.Context) {
		abortWithError(c, http.StatusMethodNotAllowed, CodeMethodNotAllowed, "method not allowed")
	})

	r.GET("/healthz", h.healthz)
	r.GET("/readyz", h.readyz)

	api := r.Group("/api")
	authGroup := api.Group("/auth")
	authGroup.POST("/register", h.register)
	authGroup.POST("/login", h.login)
	authGroup.POST("/refresh", h.refresh)

	user := api.Group("", h.authMiddleware())
	user.POST("/auth/logout", h.logout)
	user.GET("/me", h.getMe)
	user.PATCH("/me", h.patchMe)
	user.GET("/boards", h.listBoards)
	user.POST("/boards", h.createBoard)
	user.PATCH("/boards/:id", h.renameBoard)
	user.DELETE("/boards/:id", h.deleteBoard)

	return r, nil
}
