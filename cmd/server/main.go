package main

import (
	"context"
	"errors"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/joho/godotenv"

	"secondbrain-server/internal/api"
	"secondbrain-server/internal/index"
	"secondbrain-server/internal/ingest"
	"secondbrain-server/internal/llm"
	"secondbrain-server/internal/store"
	"secondbrain-server/internal/vault"
)

func main() {
	// Log to stdout, not the default stderr. Platforms like Railway classify
	// every stderr line as "error" severity, which would paint normal startup
	// logs red. Fatal errors still surface via the non-zero exit + healthcheck.
	log.SetOutput(os.Stdout)

	// Load a local .env if present. No-ops in production (Railway) where the
	// env vars are set directly and no .env file exists — hence the ignored error.
	_ = godotenv.Load()

	dbPath := envOrDefault("DB_PATH", "/data/jobs.db")

	// Bootstrap helper: `server mint "my phone"` creates a token without going
	// through the auth-protected HTTP route. Solves the first-token chicken-and-egg.
	if len(os.Args) > 1 && os.Args[1] == "mint" {
		runMint(dbPath)
		return
	}

	vaultPath := envOrDefault("VAULT_PATH", "/data/vault")
	backendURL := resolveBackendURL() // used to build join links
	port := envOrDefault("PORT", "8080")

	if err := store.Init(dbPath); err != nil {
		log.Fatalf("failed to init db: %v", err)
	}

	// One-click bootstrap: if BOOTSTRAP_ADMIN_LABEL is set and no admin exists
	// yet, mint one and print it to the logs so a template deployer gets a
	// working admin token without a manual `mint` step. Runs once — subsequent
	// boots see the existing admin and skip it.
	bootstrapAdmin()

	vault.Start(vaultPath) // boots the single-writer channel

	// Semantic search index: embeddings go through the same OpenRouter key.
	// When no key is set, index.Search/Related degrade to keyword search.
	index.Init(llm.NewFromEnv())

	// Root context is cancelled on SIGINT/SIGTERM so workers drain cleanly on
	// a Railway redeploy instead of dying mid-write.
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	// Re-embed after every vault commit, and once on boot to backfill/catch up.
	// Both run in the background so they never block writes or startup.
	if index.Enabled() {
		vault.SetOnCommit(func() {
			if err := index.Reconcile(ctx); err != nil {
				log.Printf("index: reconcile failed: %v", err)
			}
		})
		go func() {
			if err := index.Reconcile(ctx); err != nil {
				log.Printf("index: initial reconcile failed: %v", err)
			}
		}()
	}

	jobQueue := store.StartWorkers(ctx, 4, ingest.ProcessJob) // 4 concurrent extraction/chunking workers

	srv := &http.Server{
		Addr:         ":" + port,
		Handler:      api.NewRouter(jobQueue, backendURL),
		ReadTimeout:  15 * time.Second,
		WriteTimeout: 30 * time.Second,
		IdleTimeout:  60 * time.Second,
	}

	go func() {
		log.Printf("secondbrain server listening on :%s", port)
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Fatalf("server failed: %v", err)
		}
	}()

	<-ctx.Done()
	log.Println("shutdown signal received, draining...")

	shutdownCtx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	if err := srv.Shutdown(shutdownCtx); err != nil {
		log.Printf("graceful shutdown failed: %v", err)
	}
}

// runMint bootstraps a token from the CLI (bypassing HTTP auth). Defaults to an
// admin token — the deployer needs admin to invite members afterward.
//
//	server mint "my phone"          -> admin
//	server mint "teammate" member   -> member
func runMint(dbPath string) {
	label := "default"
	role := store.RoleAdmin
	if len(os.Args) > 2 {
		label = os.Args[2]
	}
	if len(os.Args) > 3 {
		role = os.Args[3]
	}
	if err := store.Init(dbPath); err != nil {
		log.Fatalf("failed to init db: %v", err)
	}
	_, raw, err := store.CreateToken(label, role)
	if err != nil {
		log.Fatalf("failed to mint token: %v", err)
	}
	fmt.Printf("token (%s, %s): %s\n", label, role, raw)
	fmt.Println("store this now — it is not recoverable.")
}

// bootstrapAdmin provisions a first admin token from env so a template deployer
// doesn't need a manual `mint`. Two modes:
//
//   - BOOTSTRAP_ADMIN_TOKEN (preferred): install this exact secret as an admin.
//     Idempotent, works even if another admin exists, and needs no log-scraping
//     since the operator already knows the value.
//   - BOOTSTRAP_ADMIN_LABEL (fallback): mint a random admin the FIRST time only
//     (when no admin exists) and print it once to the logs.
//
// No-op if neither is set.
func bootstrapAdmin() {
	label := os.Getenv("BOOTSTRAP_ADMIN_LABEL")

	if raw := os.Getenv("BOOTSTRAP_ADMIN_TOKEN"); raw != "" {
		l := label
		if l == "" {
			l = "bootstrap-admin"
		}
		created, err := store.EnsureAdminToken(l, raw)
		if err != nil {
			log.Printf("bootstrap: ensure admin token failed: %v", err)
			return
		}
		if created {
			log.Printf("bootstrap: installed admin token from BOOTSTRAP_ADMIN_TOKEN (label %q)", l)
		} else {
			log.Printf("bootstrap: BOOTSTRAP_ADMIN_TOKEN already installed — using existing")
		}
		return
	}

	if label == "" {
		return
	}
	exists, err := store.HasAdminToken()
	if err != nil {
		log.Printf("bootstrap: admin check failed: %v", err)
		return
	}
	if exists {
		log.Printf("bootstrap: an admin token already exists — skipping mint (set BOOTSTRAP_ADMIN_TOKEN to install a known one)")
		return
	}
	_, raw, err := store.CreateToken(label, store.RoleAdmin)
	if err != nil {
		log.Printf("bootstrap: mint admin failed: %v", err)
		return
	}
	log.Printf("bootstrap: minted admin token (%s): %s", label, raw)
	log.Printf("bootstrap: store this now — it is not shown again.")
}

// resolveBackendURL uses BACKEND_URL if set, otherwise Railway's auto-provided
// public domain, so a one-click deploy needs no manual URL config. It's only
// used to build invite join-links, so a missing value is a warning, not fatal —
// a fresh Railway deploy has no domain until one is generated, and the server
// must still boot (and pass its healthcheck) so a domain can be attached.
func resolveBackendURL() string {
	if v := os.Getenv("BACKEND_URL"); v != "" {
		return v
	}
	if d := os.Getenv("RAILWAY_PUBLIC_DOMAIN"); d != "" {
		return "https://" + d
	}
	log.Println("warning: no BACKEND_URL or RAILWAY_PUBLIC_DOMAIN set — invite join-links will be incomplete until a public domain is configured")
	return ""
}

func envOrDefault(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}
