package flywheel

import (
	"context"
	"fmt"
	"regexp"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
)

// Source yields the message rows of a window. The production implementation is
// PGSource; tests pass a fake.
type Source interface {
	Messages(ctx context.Context, from, to time.Time) ([]MessageRow, error)
}

// Querier is the whole database surface the flywheel uses: it can only run a
// query that returns rows. pgx.Tx satisfies it. There is no Exec, so the CLI
// has no way to write (ReadOnlyTx also opens the transaction READ ONLY, so
// Postgres itself refuses a write).
type Querier interface {
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
}

// QueryMessages reads the window's rows. The digest (a document snapshot the
// editors send for debugging) is removed in SQL, so it never leaves the
// database. Conversations the user deleted are excluded. Messages after `to`
// (up to RephraseWindow) are included so a rephrase just past the edge is seen.
const QueryMessages = `
SELECT m.id::text, m.conversation_id::text, c.surface, m.seq, m.turn_id::text, m.role,
       (m.content - 'digest')::text, m.status, COALESCE(m.prompt_version, ''), m.created_at,
       COALESCE(f.rating, ''), COALESCE(f.reason, '')
  FROM assistant_messages m
  JOIN assistant_conversations c ON c.id = m.conversation_id
  LEFT JOIN assistant_message_feedback f ON f.message_id = m.id
 WHERE c.deleted_at IS NULL
   AND m.role IN ('user', 'assistant')
   AND m.created_at >= $1
   AND m.created_at < $2
 ORDER BY m.conversation_id, m.seq`

// Queries are every statement the CLI can issue.
var Queries = []string{QueryMessages}

var writeWord = regexp.MustCompile(`(?i)\b(insert|update|delete|drop|alter|create|truncate|grant|revoke|copy|merge|call|do|vacuum|reindex|lock|set|reset|comment|refresh)\b`)

// CheckReadOnly rejects any SQL that is not a single SELECT (or WITH ... SELECT)
// statement without a write keyword.
func CheckReadOnly(sql string) error {
	s := strings.TrimSpace(sql)
	l := strings.ToLower(s)
	if !strings.HasPrefix(l, "select") && !strings.HasPrefix(l, "with") {
		return fmt.Errorf("flywheel: not a SELECT: %.40q", s)
	}
	if strings.Contains(strings.TrimSuffix(s, ";"), ";") {
		return fmt.Errorf("flywheel: more than one statement")
	}
	if m := writeWord.FindString(s); m != "" {
		return fmt.Errorf("flywheel: write keyword %q in query", m)
	}
	return nil
}

// PGSource reads through a Querier. Every statement is checked by
// CheckReadOnly right before it runs.
type PGSource struct{ Q Querier }

// Messages implements Source.
func (p PGSource) Messages(ctx context.Context, from, to time.Time) ([]MessageRow, error) {
	if err := CheckReadOnly(QueryMessages); err != nil {
		return nil, err
	}
	rows, err := p.Q.Query(ctx, QueryMessages, from, to.Add(RephraseWindow))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []MessageRow
	for rows.Next() {
		var r MessageRow
		var content string
		if err := rows.Scan(&r.ID, &r.ConversationID, &r.Surface, &r.Seq, &r.TurnID, &r.Role,
			&content, &r.Status, &r.PromptVersion, &r.CreatedAt, &r.Rating, &r.Reason); err != nil {
			return nil, err
		}
		r.Content = []byte(content)
		out = append(out, r)
	}
	return out, rows.Err()
}

// ReadOnlyTx opens a READ ONLY, repeatable-read transaction on conn. Callers
// roll it back when done (there is nothing to commit).
func ReadOnlyTx(ctx context.Context, begin interface {
	BeginTx(context.Context, pgx.TxOptions) (pgx.Tx, error)
}) (pgx.Tx, error) {
	return begin.BeginTx(ctx, pgx.TxOptions{AccessMode: pgx.ReadOnly, IsoLevel: pgx.RepeatableRead})
}
