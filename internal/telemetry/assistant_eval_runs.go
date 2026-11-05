package telemetry

import (
	"context"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"
)

// MM113: one assistant_eval_runs row per surface per recorded
// `assistant-eval --record` run (migration 20261105001_assistant_versions).

// AssistantEvalRun is one recorded eval result for a surface.
type AssistantEvalRun struct {
	ID              int64
	CreatedAt       time.Time
	PromptVersion   string
	GitSHA          string
	ModelGuide      string
	ModelCopilot    string
	Surface         string
	Cases           int
	Passed          int
	Failed          int
	Skipped         int
	CostUSD         float64
	MedianToolCalls float64
	EvidencePath    string
}

const (
	assistantEvalRunInsertSQL = `
INSERT INTO assistant_eval_runs
	(prompt_version, git_sha, model_guide, model_copilot, surface, cases, passed, failed, skipped, cost_usd, median_tool_calls, evidence_path)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12)`

	assistantEvalRunLatestSQL = `
SELECT id, created_at, prompt_version, git_sha, model_guide, model_copilot, surface, cases, passed, failed, skipped,
	cost_usd::float8, median_tool_calls::float8, evidence_path
FROM assistant_eval_runs WHERE surface = $1 ORDER BY created_at DESC, id DESC LIMIT 1`
)

// InsertAssistantEvalRun records one eval run.
func (s *Store) InsertAssistantEvalRun(ctx context.Context, r AssistantEvalRun) error {
	if !s.Enabled() {
		return ErrNoStore
	}
	ctx, cancel := s.ctx(ctx)
	defer cancel()
	_, err := s.Pool.Exec(ctx, assistantEvalRunInsertSQL, r.PromptVersion, r.GitSHA, r.ModelGuide, r.ModelCopilot,
		r.Surface, r.Cases, r.Passed, r.Failed, r.Skipped, r.CostUSD, r.MedianToolCalls, r.EvidencePath)
	return err
}

// LatestAssistantEvalRun returns the newest recorded run of a surface, or nil
// when none exists.
func (s *Store) LatestAssistantEvalRun(ctx context.Context, surface string) (*AssistantEvalRun, error) {
	if !s.Enabled() {
		return nil, ErrNoStore
	}
	ctx, cancel := s.ctx(ctx)
	defer cancel()
	var r AssistantEvalRun
	err := s.Pool.QueryRow(ctx, assistantEvalRunLatestSQL, surface).Scan(&r.ID, &r.CreatedAt, &r.PromptVersion, &r.GitSHA,
		&r.ModelGuide, &r.ModelCopilot, &r.Surface, &r.Cases, &r.Passed, &r.Failed, &r.Skipped, &r.CostUSD,
		&r.MedianToolCalls, &r.EvidencePath)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &r, nil
}
