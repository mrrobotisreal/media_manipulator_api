// Package main implements the media_manipulator_api migration runner.
//
// Run via: go run ./internal/migrations <command>.
//
// Commands: up, down, steps <n>, reset, version, force <version>, create <name>.
//
// Migrations live next to this file under ./migrations and follow the
// YYYYMMDDJJJ naming convention (UTC date + 3-digit daily job number).
package main

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/golang-migrate/migrate/v4"
	_ "github.com/golang-migrate/migrate/v4/database/postgres"
	_ "github.com/golang-migrate/migrate/v4/source/file"
	"github.com/jackc/pgx/v5"
	"github.com/joho/godotenv"
)

const (
	defaultDatabaseURL = "postgres://postgres:postgres@localhost:5432/media_manipulator?sslmode=disable"
	envDatabaseURL     = "DATABASE_URL"
	envAdminURL        = "POSTGRES_ADMIN_DATABASE_URL"
	envMigrationsPath  = "MIGRATIONS_PATH"
	envCreateDB        = "CREATE_DATABASE_IF_NOT_EXISTS"
)

func main() {
	loadDotEnv()

	if len(os.Args) < 2 {
		printUsage()
		os.Exit(2)
	}
	command := strings.ToLower(strings.TrimSpace(os.Args[1]))

	// `create` runs without needing a DB connection.
	if command == "create" {
		if len(os.Args) < 3 {
			fatalf("create requires a migration name: go run ./internal/migrations create <name>")
		}
		path, err := resolveMigrationsPath()
		if err != nil {
			fatalf("resolve migrations path: %v", err)
		}
		upPath, downPath, err := createMigrationFiles(path, os.Args[2], time.Now().UTC())
		if err != nil {
			fatalf("create migration: %v", err)
		}
		fmt.Printf("Created:\n  %s\n  %s\n", upPath, downPath)
		return
	}

	databaseURL := getEnv(envDatabaseURL, defaultDatabaseURL)
	if strings.TrimSpace(os.Getenv(envDatabaseURL)) == "" {
		fmt.Fprintf(os.Stderr, "DATABASE_URL not set, defaulting to %s\n", databaseURL)
	}

	if shouldCreateDB() {
		if err := ensureDatabaseExists(context.Background(), databaseURL); err != nil {
			fatalf("ensure database exists: %v", err)
		}
	}

	migrationsPath, err := resolveMigrationsPath()
	if err != nil {
		fatalf("resolve migrations path: %v", err)
	}
	source := "file://" + filepath.ToSlash(migrationsPath)

	m, err := migrate.New(source, databaseURL)
	if err != nil {
		fatalf("init migrate: %v", err)
	}
	defer m.Close()

	switch command {
	case "up":
		if err := m.Up(); err != nil && !errors.Is(err, migrate.ErrNoChange) {
			fatalf("up: %v", err)
		}
		fmt.Println("migrations: up complete")
	case "down":
		// `down` rolls back exactly one step to be safe.
		if err := m.Steps(-1); err != nil && !errors.Is(err, migrate.ErrNoChange) {
			fatalf("down: %v", err)
		}
		fmt.Println("migrations: down (1 step)")
	case "steps":
		if len(os.Args) < 3 {
			fatalf("steps requires an integer argument")
		}
		n, err := strconv.Atoi(os.Args[2])
		if err != nil {
			fatalf("steps: %v", err)
		}
		if err := m.Steps(n); err != nil && !errors.Is(err, migrate.ErrNoChange) {
			fatalf("steps %d: %v", n, err)
		}
		fmt.Printf("migrations: steps %d complete\n", n)
	case "reset":
		if err := m.Down(); err != nil && !errors.Is(err, migrate.ErrNoChange) {
			fatalf("reset: %v", err)
		}
		fmt.Println("migrations: reset complete")
	case "version":
		ver, dirty, err := m.Version()
		if err != nil && !errors.Is(err, migrate.ErrNilVersion) {
			fatalf("version: %v", err)
		}
		if errors.Is(err, migrate.ErrNilVersion) {
			fmt.Println("version: <none>")
		} else {
			fmt.Printf("version: %d dirty=%v\n", ver, dirty)
		}
	case "force":
		if len(os.Args) < 3 {
			fatalf("force requires a version argument")
		}
		n, err := strconv.Atoi(os.Args[2])
		if err != nil {
			fatalf("force: %v", err)
		}
		if err := m.Force(n); err != nil {
			fatalf("force %d: %v", n, err)
		}
		fmt.Printf("forced version to %d\n", n)
	default:
		printUsage()
		os.Exit(2)
	}
}

func loadDotEnv() {
	_ = godotenv.Load(".env")
	_ = godotenv.Load(".env.local")
}

func getEnv(key, fallback string) string {
	if v := strings.TrimSpace(os.Getenv(key)); v != "" {
		return v
	}
	return fallback
}

func shouldCreateDB() bool {
	raw := strings.ToLower(strings.TrimSpace(os.Getenv(envCreateDB)))
	if raw == "" {
		return true
	}
	switch raw {
	case "0", "false", "no", "off":
		return false
	}
	return true
}

func fatalf(format string, args ...any) {
	fmt.Fprintf(os.Stderr, "migrate: "+format+"\n", args...)
	os.Exit(1)
}

func printUsage() {
	fmt.Print(`media_manipulator_api migration runner

Usage:
  go run ./internal/migrations <command> [args]

Commands:
  up                    Apply all pending migrations.
  down                  Roll back one migration.
  steps <n>             Apply (n>0) or roll back (n<0) n migrations.
  reset                 Roll back every migration.
  version               Print the current migration version.
  force <version>       Force the schema to a specific version (clears dirty).
  create <name>         Create a new migration pair (YYYYMMDDJJJ_<name>.{up,down}.sql).

Environment:
  DATABASE_URL                   Postgres URL (default: ` + defaultDatabaseURL + `)
  POSTGRES_ADMIN_DATABASE_URL    Optional URL for CREATE DATABASE (defaults to the target server's "postgres" db)
  MIGRATIONS_PATH                Override migrations directory
  CREATE_DATABASE_IF_NOT_EXISTS  Create the target database when missing (default true)
`)
}

// resolveMigrationsPath finds the migrations directory.
//
// Search order:
//  1. $MIGRATIONS_PATH if set.
//  2. ./internal/migrations/migrations (relative to repo root).
//  3. ../../internal/migrations/migrations (when invoked from cmd/api).
//  4. ./migrations (when invoked from inside internal/migrations).
func resolveMigrationsPath() (string, error) {
	if override := strings.TrimSpace(os.Getenv(envMigrationsPath)); override != "" {
		if _, err := os.Stat(override); err == nil {
			abs, err := filepath.Abs(override)
			if err == nil {
				return abs, nil
			}
			return override, nil
		}
		return "", fmt.Errorf("MIGRATIONS_PATH %q does not exist", override)
	}
	candidates := []string{
		"internal/migrations/migrations",
		"../internal/migrations/migrations",
		"../../internal/migrations/migrations",
		"migrations",
	}
	for _, c := range candidates {
		if _, err := os.Stat(c); err == nil {
			abs, err := filepath.Abs(c)
			if err == nil {
				return abs, nil
			}
			return c, nil
		}
	}
	return "", fmt.Errorf("could not locate migrations dir (set MIGRATIONS_PATH)")
}

// ensureDatabaseExists connects to the maintenance database and creates the
// target database when missing. Idempotent — safe to call on every boot.
//
// We must connect to a database OTHER than the target because CREATE DATABASE
// cannot run inside a transaction and cannot be issued while connected to the
// database being created.
func ensureDatabaseExists(ctx context.Context, databaseURL string) error {
	target, host, err := parseTargetDB(databaseURL)
	if err != nil {
		return err
	}
	if target == "" {
		return errors.New("no database name in DATABASE_URL")
	}
	adminURL := strings.TrimSpace(os.Getenv(envAdminURL))
	if adminURL == "" {
		adminURL, err = deriveAdminURL(databaseURL)
		if err != nil {
			return err
		}
	}

	connCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	conn, err := pgx.Connect(connCtx, adminURL)
	if err != nil {
		return fmt.Errorf("connect admin db on %s: %w", host, err)
	}
	defer conn.Close(connCtx)

	var exists bool
	row := conn.QueryRow(connCtx, "SELECT EXISTS(SELECT 1 FROM pg_database WHERE datname = $1)", target)
	if err := row.Scan(&exists); err != nil {
		return fmt.Errorf("check database existence: %w", err)
	}
	if exists {
		return nil
	}

	// CREATE DATABASE cannot be parameterized — identifier-quote the name.
	stmt := fmt.Sprintf("CREATE DATABASE %s", quoteIdentifier(target))
	if _, err := conn.Exec(connCtx, stmt); err != nil {
		return fmt.Errorf("create database %s: %w", target, err)
	}
	fmt.Printf("created database %q on %s\n", target, host)
	return nil
}

func parseTargetDB(databaseURL string) (string, string, error) {
	u, err := url.Parse(databaseURL)
	if err != nil {
		return "", "", fmt.Errorf("parse DATABASE_URL: %w", err)
	}
	name := strings.TrimPrefix(u.Path, "/")
	host := u.Host
	if name == "" {
		return "", host, errors.New("DATABASE_URL is missing a database name")
	}
	return name, host, nil
}

func deriveAdminURL(databaseURL string) (string, error) {
	u, err := url.Parse(databaseURL)
	if err != nil {
		return "", err
	}
	u.Path = "/postgres"
	return u.String(), nil
}

// quoteIdentifier safely quotes a Postgres identifier (table/db/etc.) by
// wrapping in double-quotes and doubling any embedded quotes.
func quoteIdentifier(name string) string {
	return `"` + strings.ReplaceAll(name, `"`, `""`) + `"`
}

// createMigrationFiles writes the empty .up.sql and .down.sql files for a new
// migration. Daily counter is the next 3-digit number for today's UTC date.
func createMigrationFiles(dir, rawName string, now time.Time) (string, string, error) {
	name := normalizeMigrationName(rawName)
	if name == "" {
		return "", "", errors.New("empty migration name")
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", "", err
	}
	stamp := now.Format("20060102")
	seq, err := nextDailySequence(dir, stamp)
	if err != nil {
		return "", "", err
	}
	prefix := fmt.Sprintf("%s%03d", stamp, seq)
	upPath := filepath.Join(dir, fmt.Sprintf("%s_%s.up.sql", prefix, name))
	downPath := filepath.Join(dir, fmt.Sprintf("%s_%s.down.sql", prefix, name))
	for _, p := range []string{upPath, downPath} {
		if _, err := os.Stat(p); err == nil {
			return "", "", fmt.Errorf("migration file already exists: %s", p)
		}
	}
	if err := os.WriteFile(upPath, []byte("-- "+filepath.Base(upPath)+"\n"), 0o644); err != nil {
		return "", "", err
	}
	if err := os.WriteFile(downPath, []byte("-- "+filepath.Base(downPath)+"\n"), 0o644); err != nil {
		return "", "", err
	}
	return upPath, downPath, nil
}

func normalizeMigrationName(name string) string {
	name = strings.TrimSpace(name)
	if name == "" {
		return ""
	}
	var b strings.Builder
	for _, r := range strings.ToLower(name) {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9':
			b.WriteRune(r)
		case r == '_' || r == '-':
			b.WriteRune('_')
		case r == ' ':
			b.WriteRune('_')
		}
	}
	out := strings.Trim(b.String(), "_")
	for strings.Contains(out, "__") {
		out = strings.ReplaceAll(out, "__", "_")
	}
	return out
}

// nextDailySequence inspects existing migrations and returns the next 3-digit
// counter for the given YYYYMMDD stamp. Counter starts at 1.
func nextDailySequence(dir, stamp string) (int, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return 1, nil
		}
		return 0, err
	}
	highest := 0
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		name := e.Name()
		if len(name) < len(stamp)+3 {
			continue
		}
		if !strings.HasPrefix(name, stamp) {
			continue
		}
		seqStr := name[len(stamp) : len(stamp)+3]
		if seq, err := strconv.Atoi(seqStr); err == nil && seq > highest {
			highest = seq
		}
	}
	return highest + 1, nil
}
