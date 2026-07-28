// Command grant-tier sets an account's tier and records the audit row.
//
// Payments are not built yet, so this is how anyone becomes Premium — for
// testing, for support, and for the operator's own account. It writes both
// mm_users.tier and an mm_tier_grants row in one transaction: a grant with no
// audit trail is how "why is this account premium" becomes unanswerable.
//
//	go run ./cmd/grant-tier -uid <firebase-uid> -tier premium -by mitch -reason "founder"
//	go run ./cmd/grant-tier -uid <firebase-uid> -tier premium -expires 2026-12-31
//	go run ./cmd/grant-tier -uid <firebase-uid> -tier free -reason "grant revoked"
package main

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/user"
	"strings"
	"time"

	"github.com/joho/godotenv"

	"github.com/mrrobotisreal/media_manipulator_api/internal/config"
	"github.com/mrrobotisreal/media_manipulator_api/internal/db"
	"github.com/mrrobotisreal/media_manipulator_api/internal/logger"
	"github.com/mrrobotisreal/media_manipulator_api/internal/telemetry"
	"github.com/mrrobotisreal/media_manipulator_api/internal/tiers"
)

func main() {
	uid := flag.String("uid", "", "Firebase UID of the account (required)")
	tier := flag.String("tier", string(tiers.TierPremium), "tier to grant: free | premium")
	grantedBy := flag.String("by", "", "who is granting this (defaults to the OS user)")
	reason := flag.String("reason", "", "why, for the audit trail")
	expires := flag.String("expires", "", "optional expiry, YYYY-MM-DD (UTC). Omit for no expiry")
	flag.Parse()

	if strings.TrimSpace(*uid) == "" {
		fmt.Fprintln(os.Stderr, "-uid is required (the Firebase UID, visible in the Firebase console or in mm_users)")
		flag.Usage()
		os.Exit(2)
	}

	// Only tiers that can be stored on a user row. 'anonymous' is the absence
	// of an account, not something that can be granted.
	if *tier != string(tiers.TierFree) && *tier != string(tiers.TierPremium) {
		fmt.Fprintf(os.Stderr, "-tier must be %q or %q, got %q\n", tiers.TierFree, tiers.TierPremium, *tier)
		os.Exit(2)
	}

	var expiresAt *time.Time
	if strings.TrimSpace(*expires) != "" {
		parsed, err := time.ParseInLocation("2006-01-02", strings.TrimSpace(*expires), time.UTC)
		if err != nil {
			fmt.Fprintf(os.Stderr, "-expires must be YYYY-MM-DD: %v\n", err)
			os.Exit(2)
		}
		expiresAt = &parsed
	}

	if strings.TrimSpace(*grantedBy) == "" {
		if u, err := user.Current(); err == nil {
			*grantedBy = u.Username
		} else {
			*grantedBy = "unknown"
		}
	}

	_ = godotenv.Load(".env")
	_ = godotenv.Load(".env.local")
	cfg := config.Load()
	logging := logger.New(cfg)
	slog.SetDefault(logging)

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	pool, err := db.New(ctx, cfg)
	if err != nil || pool == nil {
		fmt.Fprintf(os.Stderr, "database unavailable: %v\n", err)
		os.Exit(1)
	}
	defer pool.Close()

	store := telemetry.NewStore(pool, cfg, logging)

	// A grant is always manual here; the 'subscription' source is reserved for
	// the billing webhook that does not exist yet.
	source := "manual_grant"
	if *tier == string(tiers.TierFree) {
		// Returning someone to free is a revocation, not a grant. Recording it
		// as 'default' keeps "was this ever paid for" answerable from the row.
		source = "default"
	}

	updated, err := store.SetUserTier(ctx, *uid, *tier, source, *grantedBy, *reason, expiresAt)
	if err != nil {
		fmt.Fprintf(os.Stderr, "grant failed: %v\n", err)
		fmt.Fprintln(os.Stderr, "(the account must already exist — it is created on first sign-in via POST /api/account/sync)")
		os.Exit(1)
	}

	expiry := "never"
	if updated.TierExpiresAt != nil {
		expiry = updated.TierExpiresAt.UTC().Format(time.RFC3339)
	}
	fmt.Printf("%s (%s)\n  tier:    %s\n  source:  %s\n  expires: %s\n  by:      %s\n",
		updated.FirebaseUID, updated.Email, updated.Tier, updated.TierSource, expiry, *grantedBy)
}
