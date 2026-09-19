// BitDX Prop Firm backend — see PROP_FIRM_PLAN.md at the workspace root
// for the full design. This binary owns: the package/phase catalog
// (GET /packages), provisioning (POST /internal/provision), the prop
// firm's own login (POST /auth/login), and the simulated trading engine
// for evaluation-stage accounts (POST /trading/*).
package main

import (
	"context"
	"log"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/google/uuid"
	"github.com/joho/godotenv"

	"github.com/dex/propfirm-backend/internal/api"
	"github.com/dex/propfirm-backend/internal/auth"
	"github.com/dex/propfirm-backend/internal/db"
	"github.com/dex/propfirm-backend/internal/priceclient"
	"github.com/dex/propfirm-backend/internal/repo"
	"github.com/dex/propfirm-backend/internal/simengine"
)

func newID() string { return uuid.NewString() }

func mustEnv(key string) string {
	v := os.Getenv(key)
	if v == "" {
		log.Fatalf("missing required env var %s", key)
	}
	return v
}

func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

func main() {
	_ = godotenv.Load() // fine if absent (e.g. real deploy sets env directly)

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	pool, err := db.Connect(ctx, mustEnv("POSTGRES_SERVICE_URI"))
	if err != nil {
		log.Fatalf("connect postgres: %v", err)
	}
	defer pool.Close()

	if err := db.Migrate(ctx, pool); err != nil {
		log.Fatalf("migrate: %v", err)
	}
	if err := db.SeedCatalog(ctx, pool); err != nil {
		log.Fatalf("seed catalog: %v", err)
	}
	log.Println("BitDX Prop Firm catalog ready (15 packages seeded/verified)")

	packagesRepo := repo.NewPackageRepo(pool)
	purchasesRepo := repo.NewPurchaseRepo(pool)
	usersRepo := repo.NewUserRepo(pool)
	accountsRepo := repo.NewAccountRepo(pool)
	tradesRepo := repo.NewTradeRepo(pool)
	_ = repo.NewProfitCreditRepo(pool) // wired in once the live-account credit path (section 10/13) is built

	priceClient := priceclient.NewClient(envOr("PROPFIRM_ENGINE_URL", "http://localhost:8080"))
	engine := simengine.New(accountsRepo, packagesRepo, tradesRepo, priceClient, newID)

	tickInterval := 5 * time.Second
	scheduler := simengine.NewScheduler(accountsRepo, engine, tickInterval)
	go scheduler.Run(ctx)

	tokenIssuer := auth.NewTokenIssuer(mustEnv("PROPFIRM_JWT_SECRET"), 24*time.Hour)
	internalSecret := mustEnv("PROPFIRM_INTERNAL_SECRET")

	packagesHandler := api.NewPackagesHandler(packagesRepo)
	marketsHandler := api.NewMarketsHandler(priceClient)
	depthHandler := api.NewDepthHandler(priceClient)
	provisionHandler := api.NewProvisionHandler(purchasesRepo, packagesRepo, usersRepo, accountsRepo, newID)
	authHandler := api.NewAuthHandler(usersRepo, tokenIssuer)
	accountsHandler := api.NewAccountsHandler(accountsRepo)
	tradingHandler := api.NewTradingHandler(accountsRepo, tradesRepo, engine)

	mux := http.NewServeMux()
	mux.HandleFunc("/health", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok"))
	})

	// Public — the exchange's purchase page reads this directly.
	mux.HandleFunc("/packages", packagesHandler.List)

	// Public — BitDX Prop Firm's own trade screen reads the real, currently
	// registered market list + live prices through here (never calls the
	// exchange's matching-engine directly from the browser).
	mux.HandleFunc("/markets", marketsHandler.List)

	// Public — real order-book depth and recent trades from the exchange's
	// own matching-engine, proxied for reference display on the trade
	// screen. Simulated (evaluation-stage) orders never execute against
	// this book — it's shown for market context only (see PROP_FIRM_PLAN.md
	// section 10).
	mux.HandleFunc("/depth", depthHandler.Depth)
	mux.HandleFunc("/trades", depthHandler.Trades)

	// Server-to-server only — Dex-Backend calls this after a successful
	// BI2XUSD debit (PROP_FIRM_PLAN.md section 3/13).
	mux.HandleFunc("/internal/provision", api.RequireInternalSecret(internalSecret, provisionHandler.Provision))

	// BitDX Prop Firm's own login (section 2).
	mux.HandleFunc("/auth/login", authHandler.Login)

	// Authenticated trader endpoints.
	mux.HandleFunc("/accounts", api.RequireAuth(tokenIssuer, accountsHandler.List))
	mux.HandleFunc("/trading/orders", api.RequireAuth(tokenIssuer, tradingHandler.OpenOrder))
	mux.HandleFunc("/trading/close", api.RequireAuth(tokenIssuer, tradingHandler.CloseOrder))
	mux.HandleFunc("/trading/cancel", api.RequireAuth(tokenIssuer, tradingHandler.CancelOrder))
	mux.HandleFunc("/trading/positions", api.RequireAuth(tokenIssuer, tradingHandler.Positions))
	mux.HandleFunc("/trading/history", api.RequireAuth(tokenIssuer, tradingHandler.History))

	frontendOrigin := envOr("PROPFIRM_FRONTEND_ORIGIN", "http://localhost:3001")
	handler := api.CORS(frontendOrigin, mux)

	addr := ":" + envOr("PORT", "8090")
	srv := &http.Server{Addr: addr, Handler: handler}

	go func() {
		log.Printf("BitDX Prop Firm backend listening on %s", addr)
		if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			log.Fatalf("server error: %v", err)
		}
	}()

	<-ctx.Done()
	log.Println("shutting down...")
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	_ = srv.Shutdown(shutdownCtx)
}
