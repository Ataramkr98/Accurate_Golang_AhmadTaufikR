package main

import (
	"context"
	"log"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/joho/godotenv"

	"tera/internal/http"
	"tera/internal/repository"
)

func main() {
	// Load environment variables from .env if present
	if err := godotenv.Load(); err != nil {
		log.Println("[Config] No .env file found or error loading, using system environment variables")
	}

	var store repository.Store
	dbURL := os.Getenv("DATABASE_URL")

	hosted := strings.EqualFold(strings.TrimSpace(os.Getenv("TERA_HOSTED")), "true")
	if dbURL != "" {
		log.Println("[Database] Connecting to PostgreSQL database...")
		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		pgStore, err := repository.NewPostgresStore(ctx, dbURL)
		cancel()

		if err != nil {
			if hosted {
				log.Fatalf("[Database] PostgreSQL is required in hosted mode: %v", err)
			}
			log.Printf("[Database] Failed to connect to PostgreSQL: %v", err)
			log.Println("[Database] Falling back to in-memory store...")
			store = repository.NewMemoryStore()
		} else {
			defer pgStore.Close()
			log.Println("[Database] Successfully connected to PostgreSQL!")
			store = pgStore

			// Seed demo data into PostgreSQL if not already present
			if err := repository.SeedDemoData(store); err != nil {
				log.Printf("[Database] Seed error: %v", err)
			}
		}
	} else {
		if hosted {
			log.Fatal("[Database] DATABASE_URL is required in hosted mode")
		}
		log.Println("[Database] DATABASE_URL not set. Running with in-memory store.")
		store = repository.NewMemoryStore()
	}

	server := httpapi.NewServer(store)
	handler := server.Router()

	// Optional same-origin SPA serving (used by the combined Render image).
	if dir := strings.TrimSpace(os.Getenv("TERA_STATIC_DIR")); dir != "" {
		log.Printf("[Static] Serving SPA from %s", dir)
		handler = httpapi.NewSPAHandler(handler, dir)
	}

	addr := strings.TrimSpace(os.Getenv("TERA_ADDR"))
	if port := strings.TrimSpace(os.Getenv("PORT")); port != "" {
		addr = ":" + port
	} else if addr == "" {
		addr = ":8080"
	}

	srv := &http.Server{
		Addr:         addr,
		Handler:      handler,
		ReadTimeout:  15 * time.Second,
		WriteTimeout: 30 * time.Second,
		IdleTimeout:  60 * time.Second,
	}

	// Channel to listen for errors coming from the listener.
	serverErrors := make(chan error, 1)

	// Start the service listening for requests.
	go func() {
		log.Printf("Tera backend listening on %s", addr)
		serverErrors <- srv.ListenAndServe()
	}()

	// Channel to listen for an interrupt or terminate signal from the OS.
	shutdown := make(chan os.Signal, 1)
	signal.Notify(shutdown, os.Interrupt, syscall.SIGTERM)

	// Blocking main and waiting for shutdown.
	select {
	case err := <-serverErrors:
		if err != nil && err != http.ErrServerClosed {
			log.Fatalf("server error: %v", err)
		}

	case sig := <-shutdown:
		log.Printf("main: %v : Start graceful shutdown", sig)

		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()

		if err := srv.Shutdown(ctx); err != nil {
			srv.Close()
			log.Fatalf("main: could not stop server gracefully: %v", err)
		}
	}
}
