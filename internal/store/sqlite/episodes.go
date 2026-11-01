package sqlite

import (
	"context"
	"database/sql"
	"errors"

	"github.com/Dubjay18/reverie/internal/store"
	"github.com/oklog/ulid/v2"
)

type episodeRepo struct{ t *txn }

const episodeCols = `task_id, title, first_event, last_event, started_at, ended_at, closed, outcome,
	events, tool_calls, errors, feedback, implicit, importance, tier, consolidated, goal, last_error`

func (r episodeRepo) Get(ctx context.Context, taskID string) (store.Episode, error) {
	sc := r.t.sc
	row := r.t.tx.QueryRowContext(ctx, `SELECT `+episodeCols+` FROM episodes
		WHERE tenant = ? AND agent = ? AND project = ? AND usr = ? AND task_id = ?`,
		sc.Tenant, sc.Agent, sc.Project, sc.User, taskID)
	ep, err := scanEpisode(row)
	if errors.Is(err, sql.ErrNoRows) {
		return store.Episode{}, store.ErrNotFound
	}
	return ep, err
}

func (r episodeRepo) Save(ctx context.Context, ep store.Episode) error {
	sc := r.t.sc
	_, err := r.t.tx.ExecContext(ctx, `INSERT INTO episodes
		(tenant, agent, project, usr, `+episodeCols+`)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(tenant, agent, project, usr, task_id) DO UPDATE SET
			title = excluded.title, first_event = excluded.first_event, last_event = excluded.last_event,
			started_at = excluded.started_at, ended_at = excluded.ended_at, closed = excluded.closed,
			outcome = excluded.outcome, events = excluded.events, tool_calls = excluded.tool_calls,
			errors = excluded.errors, feedback = excluded.feedback, implicit = excluded.implicit,
			importance = excluded.importance, tier = excluded.tier, consolidated = excluded.consolidated,
			goal = excluded.goal, last_error = excluded.last_error`,
		sc.Tenant, sc.Agent, sc.Project, sc.User,
		ep.TaskID, ep.Title, ep.FirstEvent.String(), ep.LastEvent.String(),
		fmtTime(ep.StartedAt), fmtTime(ep.EndedAt), b2i(ep.Closed), string(ep.Outcome),
		ep.Events, ep.ToolCalls, ep.Errors, ep.Feedback, b2i(ep.Implicit),
		ep.Importance, string(ep.Tier), b2i(ep.Consolidated), ep.Goal, ep.LastError)
	return err
}

func (r episodeRepo) Open(ctx context.Context) ([]store.Episode, error) {
	return r.list(ctx, `closed = 0 ORDER BY ended_at`, 0)
}

func (r episodeRepo) Recent(ctx context.Context, limit int) ([]store.Episode, error) {
	if limit <= 0 {
		limit = store.DefaultLimit
	}
	limit = min(limit, store.MaxLimit)
	return r.list(ctx, `closed = 1 ORDER BY ended_at DESC, task_id LIMIT ?`, limit)
}

func (r episodeRepo) list(ctx context.Context, tail string, limit int) ([]store.Episode, error) {
	sc := r.t.sc
	args := []any{sc.Tenant, sc.Agent, sc.Project, sc.User}
	if limit > 0 {
		args = append(args, limit)
	}
	rows, err := r.t.tx.QueryContext(ctx, `SELECT `+episodeCols+` FROM episodes
		WHERE tenant = ? AND agent = ? AND project = ? AND usr = ? AND `+tail, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []store.Episode
	for rows.Next() {
		ep, err := scanEpisode(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, ep)
	}
	return out, rows.Err()
}

type scanner interface{ Scan(dest ...any) error }

func scanEpisode(s scanner) (store.Episode, error) {
	var (
		ep                          store.Episode
		first, last, started, ended string
		closed, implicit, consol    int
		outcome, tier               string
	)
	if err := s.Scan(&ep.TaskID, &ep.Title, &first, &last, &started, &ended, &closed, &outcome,
		&ep.Events, &ep.ToolCalls, &ep.Errors, &ep.Feedback, &implicit, &ep.Importance, &tier, &consol, &ep.Goal, &ep.LastError); err != nil {
		return store.Episode{}, err
	}
	var err error
	if ep.FirstEvent, err = ulid.ParseStrict(first); err != nil {
		return ep, err
	}
	if ep.LastEvent, err = ulid.ParseStrict(last); err != nil {
		return ep, err
	}
	if ep.StartedAt, err = parseTime(started); err != nil {
		return ep, err
	}
	if ep.EndedAt, err = parseTime(ended); err != nil {
		return ep, err
	}
	ep.Closed, ep.Implicit, ep.Consolidated = closed != 0, implicit != 0, consol != 0
	ep.Outcome, ep.Tier = store.Outcome(outcome), store.Tier(tier)
	return ep, nil
}

func b2i(b bool) int {
	if b {
		return 1
	}
	return 0
}
