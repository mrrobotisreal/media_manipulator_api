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
//
// An account normally gets its mm_users row on first sign-in (POST
// /api/account/sync). For a Firebase user that has never signed in (e.g. a test
// account added in the Firebase console), grant-tier creates the row from the
// Firebase user record first (MA01). Like sync, that never sends the welcome
// email; -no-create turns it off.
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

	firebase "firebase.google.com/go/v4"
	"firebase.google.com/go/v4/auth"
	"github.com/joho/godotenv"

	"github.com/mrrobotisreal/media_manipulator_api/internal/analyticsemit"
	"github.com/mrrobotisreal/media_manipulator_api/internal/config"
	"github.com/mrrobotisreal/media_manipulator_api/internal/db"
	"github.com/mrrobotisreal/media_manipulator_api/internal/logger"
	"github.com/mrrobotisreal/media_manipulator_api/internal/models"
	"github.com/mrrobotisreal/media_manipulator_api/internal/telemetry"
	"github.com/mrrobotisreal/media_manipulator_api/internal/tiers"
)

func main() {
	uid := flag.String("uid", "", "Firebase UID of the account (required)")
	tier := flag.String("tier", string(tiers.TierPremium), "tier to grant: free | premium | pro")
	grantedBy := flag.String("by", "", "who is granting this (defaults to the OS user)")
	reason := flag.String("reason", "", "why, for the audit trail")
	expires := flag.String("expires", "", "optional expiry, YYYY-MM-DD (UTC). Omit for no expiry")
	noCreate := flag.Bool("no-create", false, "fail instead of creating a missing account from its Firebase user record")
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
	prior, err := store.UserByFirebaseUID(ctx, *uid)
	switch {
	case err == nil && prior != nil:
		fromTier = string(tiers.Resolve(true, prior))
	case err == nil && prior == nil && !*noCreate:
		created, cerr := createFromFirebase(ctx, cfg, store, *uid)
		if cerr != nil {
			fmt.Fprintf(os.Stderr, "account %s is not in mm_users and could not be created: %v\n", *uid, cerr)
			os.Exit(1)
		}
		fmt.Printf("created mm_users row for %s (%s) from Firebase\n", created.FirebaseUID, created.Email)
		fromTier = string(tiers.Resolve(true, created))
	}

	updated, err := store.SetUserTier(ctx, *uid, *tier, source, *grantedBy, *reason, expiresAt)
	if err != nil {
		fmt.Fprintf(os.Stderr, "grant failed: %v\n", err)
		if *noCreate {
			fmt.Fprintln(os.Stderr, "(-no-create: the account must already exist; it is created on first sign-in via POST /api/account/sync)")
		}
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

// createFromFirebase inserts the mm_users row for a Firebase user that has never
// called /api/account/sync, using the same upsert sync uses (it never touches
// tier). It needs the Firebase Admin credentials the API already runs with
// (GOOGLE_APPLICATION_CREDENTIALS + FIREBASE_PROJECT_ID), and refuses a UID that
// Firebase does not know, so a typo cannot mint a phantom account.
func createFromFirebase(ctx context.Context, cfg *config.Config, store *telemetry.Store, uid string) (*models.User, error) {
	if strings.TrimSpace(cfg.FirebaseProjectID) == "" {
		return nil, fmt.Errorf("FIREBASE_PROJECT_ID is not set")
	}
	app, err := firebase.NewApp(ctx, &firebase.Config{ProjectID: cfg.FirebaseProjectID})
	if err != nil {
		return nil, fmt.Errorf("firebase init: %w", err)
	}
	client, err := app.Auth(ctx)
	if err != nil {
		return nil, fmt.Errorf("firebase auth client: %w", err)
	}
	fu, err := client.GetUser(ctx, uid)
	if auth.IsUserNotFound(err) {
		return nil, fmt.Errorf("no Firebase user with UID %q in project %s", uid, cfg.FirebaseProjectID)
	}
	if err != nil {
		return nil, fmt.Errorf("firebase lookup: %w", err)
	}
	user, _, err := store.UpsertUser(ctx, syncInputFromFirebase(fu.UserInfo, fu.EmailVerified, providerIDs(fu.ProviderUserInfo)))
	return user, err
}

func providerIDs(infos []*auth.UserInfo) []string {
	out := make([]string, 0, len(infos))
	for _, p := range infos {
		if p != nil {
			out = append(out, p.ProviderID)
		}
	}
	return out
}

// syncInputFromFirebase maps a Firebase user record to the sync input, with
// signup_source from the first linked provider (password -> email, google.com ->
// google, apple.com -> apple), the vocabulary /api/account/sync stores.
func syncInputFromFirebase(info *auth.UserInfo, emailVerified bool, providers []string) telemetry.UserSyncInput {
	in := telemetry.UserSyncInput{EmailVerified: emailVerified}
	if info != nil {
		in.FirebaseUID = info.UID
		in.Email = info.Email
		in.DisplayName = info.DisplayName
	}
	for _, p := range providers {
		switch p {
		case "password":
			in.SignupSource = "email"
		case "google.com":
			in.SignupSource = "google"
		case "apple.com":
			in.SignupSource = "apple"
		default:
			continue
		}
		break
	}
	return in
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
