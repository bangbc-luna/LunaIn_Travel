package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/riverqueue/river"

	"github.com/bangbc-luna/lunain-travel/internal/identity"
	"github.com/bangbc-luna/lunain-travel/internal/platform/auth"
	"github.com/bangbc-luna/lunain-travel/internal/platform/clock"
	"github.com/bangbc-luna/lunain-travel/internal/platform/config"
	"github.com/bangbc-luna/lunain-travel/internal/platform/db"
	"github.com/bangbc-luna/lunain-travel/internal/platform/httpx"
	"github.com/bangbc-luna/lunain-travel/internal/platform/jobs"
	"github.com/bangbc-luna/lunain-travel/internal/platform/mailer"
)

// migrateTimeout bounds db.Migrate, including waiting for the migration lock
// held by another replica.
const migrateTimeout = 5 * time.Minute

func main() {
	log := slog.New(slog.NewJSONHandler(os.Stdout, nil))
	slog.SetDefault(log)

	if err := dispatch(log, os.Args[1:]); err != nil {
		log.Error("fatal", "err", err)
		os.Exit(1)
	}
}

// dispatch runs the subcommand named by args[0]; no subcommand means serve.
func dispatch(log *slog.Logger, args []string) error {
	if len(args) == 0 {
		return serve(log)
	}
	switch args[0] {
	case "serve":
		return serve(log)
	case "create-admin":
		return createAdmin(args[1:])
	default:
		return fmt.Errorf("unknown command %q (want serve or create-admin)", args[0])
	}
}

// createAdmin creates a verified super_admin account and prints its id.
func createAdmin(args []string) error {
	fs := flag.NewFlagSet("create-admin", flag.ContinueOnError)
	email := fs.String("email", "", "admin email")
	password := fs.String("password", "", "admin password")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *email == "" || *password == "" {
		return errors.New("create-admin: --email and --password are required")
	}
	cfg, err := config.Load()
	if err != nil {
		return err
	}
	connectCtx, cancelConnect := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancelConnect()
	pool, err := db.Connect(connectCtx, cfg.DatabaseURL)
	if err != nil {
		return err
	}
	defer pool.Close()
	migCtx, cancelMig := context.WithTimeout(context.Background(), migrateTimeout)
	defer cancelMig()
	if err := db.Migrate(migCtx, pool); err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	svc := identity.NewService(identity.Deps{Pool: pool, Clock: clock.Real{}, CodePepper: cfg.CodePepper})
	var id uuid.UUID
	err = db.InTx(ctx, pool, func(tx pgx.Tx) error {
		var err error
		id, err = svc.CreateInternalUser(ctx, tx, *email, *password, auth.RoleSuperAdmin)
		return err
	})
	if err != nil {
		return err
	}
	fmt.Println(id)
	return nil
}

// rejectingGoogle is used when GOOGLE_CLIENT_ID is not configured.
type rejectingGoogle struct{}

func (rejectingGoogle) Verify(context.Context, string) (identity.GoogleIdentity, error) {
	return identity.GoogleIdentity{}, identity.ErrGoogleTokenInvalid
}

func serve(log *slog.Logger) error {
	cfg, err := config.Load()
	if err != nil {
		return err
	}

	log.Info("config loaded", "env", cfg.AppEnv, "trusted_proxy_hops", cfg.TrustedProxyHops)

	connectCtx, cancelConnect := context.WithTimeout(context.Background(), 30*time.Second)
	pool, err := db.Connect(connectCtx, cfg.DatabaseURL)
	cancelConnect()
	if err != nil {
		return err
	}
	defer pool.Close()
	migCtx, cancelMig := context.WithTimeout(context.Background(), migrateTimeout)
	err = db.Migrate(migCtx, pool)
	cancelMig()
	if err != nil {
		return err
	}

	var mail mailer.Mailer = mailer.LogMailer{Logger: log}
	if cfg.AppEnv == "prod" {
		mail = mailer.SMTPMailer{
			Host: cfg.SMTPHost, Port: cfg.SMTPPort,
			User: cfg.SMTPUser, Password: cfg.SMTPPassword, From: cfg.MailFrom,
		}
	}
	workers := river.NewWorkers()
	river.AddWorker(workers, &mailer.SendEmailWorker{Mailer: mail})
	jobClient, err := jobs.NewClient(pool, workers, log)
	if err != nil {
		return err
	}
	if err := jobClient.Start(context.Background()); err != nil {
		return err
	}
	defer func() {
		stopCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if err := jobClient.Stop(stopCtx); err != nil {
			log.Error("stop job client", "err", err)
		}
	}()

	var google identity.GoogleVerifier = rejectingGoogle{}
	if cfg.GoogleClientID == "" {
		log.Warn("GOOGLE_CLIENT_ID is empty: Google sign-in is disabled")
	} else {
		google, err = identity.NewGoogleVerifier(context.Background(), cfg.GoogleClientID)
		if err != nil {
			return err
		}
	}
	issuer := auth.NewIssuer(cfg.JWTSecret, 15*time.Minute, clock.Real{})
	idSvc := identity.NewService(identity.Deps{
		Pool: pool, Jobs: jobClient, Issuer: issuer, Clock: clock.Real{},
		CodePepper: cfg.CodePepper, Google: google, RefreshTTL: 30 * 24 * time.Hour,
	})

	mux := http.NewServeMux()
	idSvc.Routes(mux, issuer, httpx.ClientIPFunc(cfg.TrustedProxyHops))
	mux.HandleFunc("GET /healthz", healthz)
	mux.HandleFunc("GET /readyz", readyz(pool))
	mux.HandleFunc("/", httpx.NotFound)

	srv := &http.Server{
		Addr:              cfg.HTTPAddr,
		Handler:           httpx.Chain(mux, httpx.RequestID, httpx.Logger(log), httpx.Recover),
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       15 * time.Second,
		WriteTimeout:      30 * time.Second,
		IdleTimeout:       60 * time.Second,
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	errCh := make(chan error, 1)
	go func() {
		log.Info("http server listening", "addr", cfg.HTTPAddr, "env", cfg.AppEnv)
		errCh <- srv.ListenAndServe()
	}()

	select {
	case err := <-errCh:
		return err
	case <-ctx.Done():
	}

	log.Info("shutting down")
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := srv.Shutdown(shutdownCtx); err != nil {
		return err
	}
	if err := <-errCh; !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return nil
}

func healthz(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]string{"status": "ok"})
}

func readyz(pool *pgxpool.Pool) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		ctx, cancel := context.WithTimeout(r.Context(), 2*time.Second)
		defer cancel()
		w.Header().Set("Content-Type", "application/json")
		if err := pool.Ping(ctx); err != nil {
			w.WriteHeader(http.StatusServiceUnavailable)
			_ = json.NewEncoder(w).Encode(map[string]string{"status": "unavailable"})
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]string{"status": "ready"})
	}
}
