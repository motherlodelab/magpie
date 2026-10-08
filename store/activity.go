package store

import (
	"database/sql"
	"fmt"
	"time"
)

// ActivityRow is one day × command × status bucket of run_history.
type ActivityRow struct {
	Day              string // YYYY-MM-DD in the caller's zone
	Command          string
	Status           string
	Runs             int
	PagesOK          int
	PagesErr         int
	PromptTokens     int
	CompletionTokens int
	USDEstimate      float64
}

// Activity sums runs started at or after since, by day in the caller's
// zone (offset = its UTC offset) × command × status, oldest day first.
// started_at is RFC3339 UTC everywhere (BeginRun), so the since filter is
// a string compare. Watch checks are not runs and are skipped (QA ST8).
// Feeds the desktop Overview chart (D7).
// ponytail: one offset for the whole window — a run within an hour of a
// DST switch can land on the neighbouring day. Upgrade: per-day bounds.
func (d *DB) Activity(since time.Time, offset time.Duration) ([]ActivityRow, error) {
	mod := fmt.Sprintf("%+d minutes", int(offset/time.Minute))
	rows, err := d.db.Query(`SELECT date(started_at, ?) AS day, command, status, COUNT(*),
		SUM(pages_ok), SUM(pages_err), SUM(prompt_tokens), SUM(completion_tokens), SUM(usd_estimate)
		FROM run_history WHERE started_at >= ? AND command != 'watch'
		GROUP BY day, command, status ORDER BY day, command, status`,
		mod, since.UTC().Format(time.RFC3339))
	if err != nil {
		return nil, fmt.Errorf("store: activity: %w", err)
	}
	defer func() { _ = rows.Close() }() //nolint:errcheck // read-only; close error unactionable
	var out []ActivityRow
	for rows.Next() {
		var r ActivityRow
		var day sql.NullString
		if err := rows.Scan(&day, &r.Command, &r.Status, &r.Runs, &r.PagesOK, &r.PagesErr,
			&r.PromptTokens, &r.CompletionTokens, &r.USDEstimate); err != nil {
			return nil, fmt.Errorf("store: activity: %w", err)
		}
		if !day.Valid {
			continue // malformed started_at: skipped, not fatal (desktop Stats precedent)
		}
		r.Day = day.String
		out = append(out, r)
	}
	return out, rows.Err()
}

// ProviderSpend is LLM usage for one provider (llm_calls).
type ProviderSpend struct {
	Provider         string
	Calls            int
	PromptTokens     int
	CompletionTokens int
	USDEstimate      float64
}

// SpendByProvider sums llm_calls logged at or after since, biggest spend
// first (ties by provider name). ts is RFC3339 UTC (LogLLMCall), so the
// filter is a string compare on idx_llm_calls_ts — the desktop budget gate
// runs this before every metered call (D7).
func (d *DB) SpendByProvider(since time.Time) ([]ProviderSpend, error) {
	rows, err := d.db.Query(`SELECT provider, COUNT(*), SUM(prompt_tokens), SUM(completion_tokens), SUM(usd_estimate)
		FROM llm_calls WHERE ts >= ? GROUP BY provider ORDER BY 5 DESC, provider`,
		since.UTC().Format(time.RFC3339))
	if err != nil {
		return nil, fmt.Errorf("store: spend by provider: %w", err)
	}
	defer func() { _ = rows.Close() }() //nolint:errcheck // read-only; close error unactionable
	var out []ProviderSpend
	for rows.Next() {
		var p ProviderSpend
		if err := rows.Scan(&p.Provider, &p.Calls, &p.PromptTokens, &p.CompletionTokens, &p.USDEstimate); err != nil {
			return nil, fmt.Errorf("store: spend by provider: %w", err)
		}
		out = append(out, p)
	}
	return out, rows.Err()
}
