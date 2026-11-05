package main

import (
	"context"
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
	"github.com/riverqueue/river"

	"github.com/bangbc-luna/lunain-travel/internal/booking"
	"github.com/bangbc-luna/lunain-travel/internal/catalog"
	"github.com/bangbc-luna/lunain-travel/internal/files"
	"github.com/bangbc-luna/lunain-travel/internal/geo"
	"github.com/bangbc-luna/lunain-travel/internal/identity"
	"github.com/bangbc-luna/lunain-travel/internal/ledger"
	"github.com/bangbc-luna/lunain-travel/internal/listing"
	"github.com/bangbc-luna/lunain-travel/internal/partner"
	"github.com/bangbc-luna/lunain-travel/internal/payment"
	"github.com/bangbc-luna/lunain-travel/internal/payout"
	"github.com/bangbc-luna/lunain-travel/internal/platform/auth"
	"github.com/bangbc-luna/lunain-travel/internal/platform/clock"
	"github.com/bangbc-luna/lunain-travel/internal/platform/config"
	"github.com/bangbc-luna/lunain-travel/internal/platform/db"
	"github.com/bangbc-luna/lunain-travel/internal/platform/httpx"
	"github.com/bangbc-luna/lunain-travel/internal/platform/jobs"
	"github.com/bangbc-luna/lunain-travel/internal/platform/mailer"
	"github.com/bangbc-luna/lunain-travel/internal/platform/storage"
	"github.com/bangbc-luna/lunain-travel/internal/reconcile"
	"github.com/bangbc-luna/lunain-travel/internal/settings"
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

// paymentProvider is the in-memory fake when PAYMENT_PROVIDER=fake (never in
// prod: config.Load refuses it), otherwise the PayOS REST client.
func paymentProvider(cfg config.Config) payment.Provider {
	if cfg.PaymentProvider == "fake" {
		return payment.NewFake()
	}
	return payment.NewPayOS(payment.PayOSConfig{
		ClientID: cfg.PayOSClientID, APIKey: cfg.PayOSAPIKey, ChecksumKey: cfg.PayOSChecksumKey,
		BaseURL: cfg.PayOSBaseURL, ReturnURL: cfg.PayOSReturnURL, CancelURL: cfg.PayOSCancelURL,
	}, nil)
}

func serve(log *slog.Logger) error {
	cfg, err := config.Load()
	if err != nil {
		return err
	}

	log.Info("config loaded", "env", cfg.AppEnv, "trusted_proxy_hops", cfg.TrustedProxyHops)
	for _, w := range cfg.PaymentWarnings() {
		log.Warn(w)
	}

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
	// The booking and reconcile workers get their Service once it is built
	// (it needs the job client for emails); the client is started only after
	// that.
	expireHolds, noShow, release := &booking.ExpireHoldsWorker{}, &booking.NoShowWorker{}, &booking.ReleaseWorker{}
	reconcileDaily, remindRefunds := &reconcile.DailyWorker{}, &reconcile.RemindRefundsWorker{}
	workers := river.NewWorkers()
	river.AddWorker(workers, &mailer.SendEmailWorker{Mailer: mail})
	river.AddWorker(workers, expireHolds)
	river.AddWorker(workers, noShow)
	river.AddWorker(workers, release)
	river.AddWorker(workers, reconcileDaily)
	river.AddWorker(workers, remindRefunds)
	periodic := append(booking.PeriodicJobs(), reconcile.PeriodicJobs()...)
	jobClient, err := jobs.NewClient(pool, workers, log, periodic...)
	if err != nil {
		return err
	}

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

	if cfg.PublicFileBaseURL == "" {
		return errors.New("PUBLIC_FILE_BASE_URL is not set (dev: docker compose up -d minio minio-init, see .env.example)")
	}
	objects, err := storage.NewS3(context.Background(), storage.S3Config{
		Endpoint: cfg.S3Endpoint, Region: cfg.S3Region, Bucket: cfg.S3Bucket,
		AccessKey: cfg.S3AccessKey, SecretKey: cfg.S3SecretKey, UsePathStyle: cfg.S3UsePathStyle,
	})
	if err != nil {
		return err
	}

	fileSvc := files.NewService(pool, objects, clock.Real{}, cfg.PublicFileBaseURL)
	geoSvc := geo.NewService(pool)
	catSvc := catalog.NewService(pool, clock.Real{})
	partnerSvc := partner.NewService(partner.Deps{
		Pool: pool, Clock: clock.Real{}, Users: idSvc, Catalog: catSvc, Geo: geoSvc, Files: fileSvc,
	})

	listingSvc := listing.NewService(listing.Deps{
		Pool: pool, Clock: clock.Real{}, Partners: partnerSvc, Catalog: catSvc, Geo: geoSvc, Files: fileSvc,
		BookingGuard: booking.Guard{},
	})

	settingsSvc := settings.NewService(pool, clock.Real{})
	paymentSvc := payment.NewService(pool, clock.Real{}, paymentProvider(cfg), log)
	paymentSvc.SetOps(payment.OpsDeps{Jobs: jobClient, Users: idSvc, Files: fileSvc})
	ledgerSvc := ledger.NewService(pool, clock.Real{})
	bookingSvc := booking.NewService(booking.Deps{
		Pool: pool, Clock: clock.Real{}, Listings: listingSvc, Catalog: catSvc, Settings: settingsSvc,
		Identity: idSvc, Payments: paymentSvc, Ledger: ledgerSvc, Jobs: jobClient, Log: log,
	})
	expireHolds.Service, noShow.Service, release.Service = bookingSvc, bookingSvc, bookingSvc
	payoutSvc := payout.NewService(payout.Deps{
		Pool: pool, Clock: clock.Real{}, Ledger: ledgerSvc, Partners: partnerSvc, Settings: settingsSvc,
		Files: fileSvc, Jobs: jobClient, Log: log,
	})
	reconcileSvc := reconcile.NewService(reconcile.Deps{
		Pool: pool, Clock: clock.Real{}, Payments: paymentSvc, Ledger: ledgerSvc, Handler: bookingSvc,
		Identity: idSvc, Jobs: jobClient, Log: log,
	})
	reconcileDaily.Service, remindRefunds.Payments = reconcileSvc, paymentSvc

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

	mux, _ := buildMux(muxDeps{
		Pool: pool, Issuer: issuer, ClientIP: httpx.ClientIPFunc(cfg.TrustedProxyHops),
		Identity: idSvc, Settings: settingsSvc, Files: fileSvc,
		Geo: geoSvc, Catalog: catSvc, Partner: partnerSvc, Listing: listingSvc,
		Ledger: ledgerSvc, Payment: paymentSvc, Booking: bookingSvc, Payout: payoutSvc, Reconcile: reconcileSvc,
		AppEnv: cfg.AppEnv,
	})

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
