// Command reseed wipes the demo tenant and re-runs SeedDemoData against the
// configured database, so an existing deployment picks up the current seed
// dataset instead of the stale one it was first initialised with.
//
// Usage: go run ./cmd/reseed
package main

import (
	"context"
	"log"
	"time"

	"github.com/joho/godotenv"

	"tera/internal/repository"
)

func main() {
	if err := godotenv.Load(); err != nil {
		log.Println("[Config] No .env file found, using system environment variables")
	}

	dbURL := repository.DatabaseURLFromEnv()
	if dbURL == "" {
		log.Fatal("[Reseed] DATABASE_URL is not set; nothing to reseed.")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	store, err := repository.NewPostgresStore(ctx, dbURL)
	if err != nil {
		log.Fatalf("[Reseed] could not connect: %v", err)
	}
	defer store.Close()

	if err := store.WipeDemoTenant(ctx); err != nil {
		log.Fatalf("[Reseed] wipe failed: %v", err)
	}
	log.Println("[Reseed] demo tenant removed.")

	if err := repository.SeedDemoData(store); err != nil {
		log.Fatalf("[Reseed] seed failed: %v", err)
	}
	log.Println("[Reseed] demo tenant reseeded with the current dataset.")
}
