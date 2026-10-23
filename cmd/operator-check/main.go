// Command operator-check explains, per address on OPERATOR_EMAILS, whether
// Firebase will let that account resolve to the operator tier — and if not,
// why. It answers "I set the variable, why am I still free?" without reading
// a single production log.
//
// The operator tier (internal/tiers) requires two things of a Firebase ID
// token: the email claim is on the allowlist, and email_verified is true. The
// second is what this tool is usually for: an email/password account that
// never clicked its verification link carries email_verified=false and is an
// ordinary free account no matter what the allowlist says.
//
//	go run ./cmd/operator-check
//	go run ./cmd/operator-check -emails a@b.c,d@e.f
package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"strings"
	"time"

	firebase "firebase.google.com/go/v4"
	"firebase.google.com/go/v4/auth"
	"github.com/joho/godotenv"

	"github.com/mrrobotisreal/media_manipulator_api/internal/config"
)

func main() {
	emailsFlag := flag.String("emails", "", "comma-separated addresses to check (default: OPERATOR_EMAILS)")
	flag.Parse()

	_ = godotenv.Load(".env")
	_ = godotenv.Load(".env.local")
	cfg := config.Load()

	emails := cfg.OperatorEmails
	if strings.TrimSpace(*emailsFlag) != "" {
		emails = nil
		for _, e := range strings.Split(*emailsFlag, ",") {
			if e = strings.ToLower(strings.TrimSpace(e)); e != "" {
				emails = append(emails, e)
			}
		}
	}
	if len(emails) == 0 {
		fmt.Fprintln(os.Stderr, "OPERATOR_EMAILS is empty and -emails was not given; nothing to check")
		os.Exit(2)
	}
	if strings.TrimSpace(cfg.FirebaseProjectID) == "" {
		fmt.Fprintln(os.Stderr, "FIREBASE_PROJECT_ID is not set")
		os.Exit(2)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	app, err := firebase.NewApp(ctx, &firebase.Config{ProjectID: cfg.FirebaseProjectID})
	if err != nil {
		fmt.Fprintf(os.Stderr, "firebase init failed: %v\n", err)
		os.Exit(1)
	}
	client, err := app.Auth(ctx)
	if err != nil {
		fmt.Fprintf(os.Stderr, "firebase auth client failed: %v\n", err)
		os.Exit(1)
	}

	fmt.Printf("project %s — %d allowlisted address(es)\n\n", cfg.FirebaseProjectID, len(emails))
	problems := 0
	for _, email := range emails {
		u, err := client.GetUserByEmail(ctx, email)
		switch {
		case auth.IsUserNotFound(err):
			problems++
			fmt.Printf("  %-40s NO FIREBASE ACCOUNT — sign in once (Google, or email+password then verify)\n", email)
			continue
		case err != nil:
			problems++
			fmt.Printf("  %-40s lookup failed: %v\n", email, err)
			continue
		}
		providers := make([]string, 0, len(u.ProviderUserInfo))
		for _, p := range u.ProviderUserInfo {
			providers = append(providers, p.ProviderID)
		}
		status := "OPERATOR ✓"
		if !u.EmailVerified {
			problems++
			status = "NOT VERIFIED ✗ — resolves to free until the verification link is clicked"
		}
		if u.Disabled {
			problems++
			status = "DISABLED ✗"
		}
		fmt.Printf("  %-40s uid=%s verified=%-5v providers=%s\n      → %s\n",
			email, u.UID, u.EmailVerified, strings.Join(providers, ","), status)
	}
	fmt.Println()
	if problems > 0 {
		fmt.Printf("%d address(es) will NOT resolve to operator.\n", problems)
		os.Exit(1)
	}
	fmt.Println("every allowlisted address resolves to operator once signed in.")
}
