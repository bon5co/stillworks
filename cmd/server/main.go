package main

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/bon5co/godjango/auth"
	"github.com/bon5co/godjango/database"
	"github.com/bon5co/godjango/web"
	configuredproject "stillworks/internal/project"
)

func main() {
	address := "127.0.0.1:8000"
	if len(os.Args) > 1 {
		address = os.Args[1]
	}
	settings, err := configuredproject.LoadRuntimeSettings()
	if err != nil {
		exit(err)
	}
	db, err := database.Open(
		context.Background(),
		database.DefaultConfig(settings.DatabaseURL.Reveal()),
	)
	if err != nil {
		exit(err)
	}
	defer db.Close()
	configured, err := configuredproject.Configure()
	if err != nil {
		exit(err)
	}
	store := auth.NewBunStore(db)
	manager := auth.NewManager(store, auth.NewPasswordHasher())
	sessionStore, err := web.NewSessionStore(store)
	if err != nil {
		exit(err)
	}
	sessions, err := web.NewSessions(web.SessionConfig{
		CookieName:  "godjango_session",
		Lifetime:    24 * time.Hour,
		IdleTimeout: 30 * time.Minute,
		Secure:      !settings.Debug,
	}, sessionStore)
	if err != nil {
		exit(err)
	}
	csrf, err := web.NewCSRF(web.CSRFConfig{
		CookieName: "godjango_csrf",
		Secure:     !settings.Debug,
	})
	if err != nil {
		exit(err)
	}
	sessionSecret := derive(settings.SessionSecret.Reveal(), "session")
	resetSecret := derive(settings.SessionSecret.Reveal(), "password-reset")
	router, err := web.NewRouter(web.RouterConfig{
		Project: configured,
		Services: web.RuntimeServices{
			Database:  db,
			Users:     manager,
			AuthStore: store,
			Login: func(request *http.Request, user *auth.User) error {
				return auth.Login(web.SessionFromRequest(request), user, sessionSecret)
			},
		},
		Middleware: []web.Middleware{
			web.RequestID(),
			web.Recover(),
			web.SecurityHeaders(web.SecurityHeadersConfig{HTTPS: !settings.Debug}),
			web.BodyLimit(1 << 20),
			sessions.Middleware,
			csrf.Middleware,
			web.Authentication(manager, sessionSecret),
		},
	})
	if err != nil {
		exit(err)
	}
	authHandlers, err := web.NewAuthHandlers(web.AuthHandlerConfig{
		Backend:       manager,
		SessionSecret: sessionSecret,
		ResetTokens: auth.ResetTokenGenerator{
			Secret:  resetSecret,
			Timeout: 24 * time.Hour,
		},
		SendReset: func(context.Context, web.ResetMessage) error {
			return errors.New("password reset delivery is not configured")
		},
	})
	if err != nil {
		exit(err)
	}
	authHandlers.Routes(router)
	handler := http.NewServeMux()
	handler.HandleFunc("GET /healthz", func(response http.ResponseWriter, _ *http.Request) {
		response.WriteHeader(http.StatusNoContent)
	})
	handler.Handle("/", router)
	listener, err := net.Listen("tcp", address)
	if err != nil {
		exit(err)
	}
	fmt.Fprintf(os.Stdout, "Starting development server at http://%s/\n", address)
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	server := web.Server{
		Handler:           handler,
		ShutdownTimeout:   10 * time.Second,
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       30 * time.Second,
		WriteTimeout:      30 * time.Second,
		IdleTimeout:       2 * time.Minute,
	}
	if err := server.Serve(ctx, listener); err != nil {
		exit(err)
	}
}

func derive(secret string, purpose string) []byte {
	sum := sha256.Sum256([]byte(purpose + "\x00" + secret))
	return sum[:]
}

func exit(err error) {
	fmt.Fprintln(os.Stderr, err)
	os.Exit(1)
}
