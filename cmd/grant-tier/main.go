// Command grant-tier sets an account's tier and records the audit row.
//
// Payments are not built yet, so this is how anyone becomes Premium — for
// testing, for support, and for the operator's own account. It writes both
// mm_users.tier and an mm_tier_grants row in one transaction: a grant with no
// audit trail is how "why is this account premium" becomes unanswerable.
//
//	go run ./cmd/grant-tier -uid <firebase-uid> -tier premium -by mitch -reason "founder"
//	go run ./cmd/grant-tier -uid <firebase-uid> -tier premium -expires 2026-12-31
//	go run ./cmd/grant-tier -uid <firebase-uid> -tier pro -reason "support"
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

	"github.com/mrrobotisreal/media_manipulator_api/internal/analyticsemit"
	"github.com/mrrobotisreal/media_manipulator_api/internal/config"
	"github.com/mrrobotisreal/media_manipulator_api/internal/db"
	"github.com/mrrobotisreal/media_manipulator_api/internal/logger"
	"github.com/mrrobotisreal/media_manipulator_api/internal/telemetry"
	"github.com/mrrobotisreal/media_manipulator_api/internal/tiers"
)

func main() {
	uid := flag.String("uid", "", "Firebase UID of the account (required)")
	tier := flag.String("tier", string(tiers.TierPremium), "tier to grant: free | premium | pro")
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
	// of an account, and 'operator' is derived per request from the allowlist,
	// so neither can be granted.
	if err := validateGrantTier(*tier); err != nil {
		fmt.Fprintln(os.Stderr, err)
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

	// The tier before the change, for server_tier_changed {from}. A failed lookup
	// only costs the event its "from"; it never blocks the grant.
	fromTier := "unknown"
	if prior, err := store.UserByFirebaseUID(ctx, *uid); err == nil && prior != nil {
		fromTier = string(tiers.Resolve(true, prior))
	}

	updated, err := store.SetUserTier(ctx, *uid, *tier, source, *grantedBy, *reason, expiresAt)
	if err != nil {
		fmt.Fprintf(os.Stderr, "grant failed: %v\n", err)
		fmt.Fprintln(os.Stderr, "(the account must already exist — it is created on first sign-in via POST /api/account/sync)")
		os.Exit(1)
	}

	emitTierChanged(cfg, logging, updated.FirebaseUID, fromTier, string(tiers.Resolve(true, updated)), source)

	expiry := "never"
	if updated.TierExpiresAt != nil {
		expiry = updated.TierExpiresAt.UTC().Format(time.RFC3339)
	}
	fmt.Printf("%s (%s)\n  tier:    %s\n  source:  %s\n  expires: %s\n  by:      %s\n",
		updated.FirebaseUID, updated.Email, updated.Tier, updated.TierSource, expiry, *grantedBy)
}

// validateGrantTier accepts exactly the storable tiers: free, premium, pro.
func validateGrantTier(t string) error {
	tier := tiers.Tier(t)
	if !tiers.Valid(tier) || tier == tiers.TierAnonymous || tier == tiers.TierOperator {
		return fmt.Errorf("-tier must be %q, %q or %q, got %q", tiers.TierFree, tiers.TierPremium, tiers.TierPro, t)
	}
	return nil
}

// emitTierChanged sends server_tier_changed through internal/analyticsemit. It is
// a no-op when ANALYTICS_SERVER_KEY / ANALYTICS_INGEST_URL are unset.
//
// KNOWN LIMIT: this process has no browser, hence no visitor/session UUIDs, and
// analyticsemit.Emit deliberately drops events without them (it refuses to mint
// synthetic visitors). So today this event is counted as skipped_no_identity and
// not delivered; mm_tier_grants remains the audit record. It is wired anyway so
// that if the analytics side ever accepts a server identity the call site is
// already correct. See the MM17 session log.
func emitTierChanged(cfg *config.Config, log *slog.Logger, firebaseUID, from, to, source string) {
	em := analyticsemit.New(cfg, log, nil)
	if !em.Enabled() {
		return
	}
	em.Emit(analyticsemit.Event{
		EventName:  analyticsemit.ServerTierChanged,
		EventTS:    time.Now().UTC(),
		PersonHint: &analyticsemit.PersonHint{FirebaseUID: firebaseUID, Tier: to},
		Context:    analyticsemit.Context("", "", "", to),
		Properties: map[string]any{"from": from, "to": to, "source": source},
	})
	em.Close(5 * time.Second)
}
