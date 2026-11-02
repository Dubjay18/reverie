package sqlite

import (
	"context"
	"database/sql"
	"errors"
	"time"

	"github.com/Dubjay18/reverie/internal/store"
)

type dreamRepo struct{ t *txn }

func (r dreamRepo) Acquire(ctx context.Context, runID string, ttl time.Duration, now time.Time) (bool, error) {
	sc := r.t.sc
	var holder, exp string
	err := r.t.tx.QueryRowContext(ctx, `SELECT run_id, expires_at FROM dream_leases
		WHERE tenant = ? AND agent = ? AND project = ? AND usr = ?`,
		sc.Tenant, sc.Agent, sc.Project, sc.User).Scan(&holder, &exp)
	switch {
	case err == nil:
		expAt, err := parseTime(exp)
		if err != nil {
			return false, err
		}
		if holder != runID && expAt.After(now) {
			return false, nil
		}
	case errors.Is(err, sql.ErrNoRows):
	default:
		return false, err
	}
	if _, err := r.t.tx.ExecContext(ctx, `INSERT INTO dream_leases (tenant, agent, project, usr, run_id, expires_at)
		VALUES (?, ?, ?, ?, ?, ?)
		ON CONFLICT(tenant, agent, project, usr) DO UPDATE SET run_id = excluded.run_id, expires_at = excluded.expires_at`,
		sc.Tenant, sc.Agent, sc.Project, sc.User, runID, fmtTime(now.Add(ttl))); err != nil {
		return false, err
	}
	// Any run still marked running from an expired lease is now aborted.
	_, err = r.t.tx.ExecContext(ctx, `UPDATE dream_runs SET status = ?, ended_at = ?
		WHERE tenant = ? AND agent = ? AND project = ? AND usr = ? AND status = ? AND id != ?`,
		store.RunAborted, fmtTime(now), sc.Tenant, sc.Agent, sc.Project, sc.User, store.RunRunning, runID)
	return err == nil, err
}

func (r dreamRepo) Renew(ctx context.Context, runID string, ttl time.Duration, now time.Time) (bool, error) {
	sc := r.t.sc
	res, err := r.t.tx.ExecContext(ctx, `UPDATE dream_leases SET expires_at = ?
		WHERE tenant = ? AND agent = ? AND project = ? AND usr = ? AND run_id = ?`,
		fmtTime(now.Add(ttl)), sc.Tenant, sc.Agent, sc.Project, sc.User, runID)
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	return n == 1, err
}

func (r dreamRepo) Release(ctx context.Context, runID string) error {
	sc := r.t.sc
	_, err := r.t.tx.ExecContext(ctx, `DELETE FROM dream_leases
		WHERE tenant = ? AND agent = ? AND project = ? AND usr = ? AND run_id = ?`,
		sc.Tenant, sc.Agent, sc.Project, sc.User, runID)
	return err
}

func (r dreamRepo) StartRun(ctx context.Context, d store.DreamRun) error {
	sc := r.t.sc
	_, err := r.t.tx.ExecContext(ctx, `INSERT INTO dream_runs
		(tenant, agent, project, usr, id, trigger, status, started_at, ended_at, budget, spent, report)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, '', ?, ?, ?)`,
		sc.Tenant, sc.Agent, sc.Project, sc.User, d.ID, d.Trigger, d.Status, fmtTime(d.StartedAt), d.Budget, d.Spent, []byte(d.Report))
	return err
}

func (r dreamRepo) FinishRun(ctx context.Context, d store.DreamRun) error {
	sc := r.t.sc
	res, err := r.t.tx.ExecContext(ctx, `UPDATE dream_runs
		SET status = ?, ended_at = ?, spent = ?, report = ?
		WHERE tenant = ? AND agent = ? AND project = ? AND usr = ? AND id = ?`,
		d.Status, fmtTime(d.EndedAt), d.Spent, []byte(d.Report),
		sc.Tenant, sc.Agent, sc.Project, sc.User, d.ID)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return store.ErrNotFound
	}
	return nil
}

func (r dreamRepo) GetRun(ctx context.Context, id string) (store.DreamRun, error) {
	sc := r.t.sc
	row := r.t.tx.QueryRowContext(ctx, `SELECT id, trigger, status, started_at, ended_at, budget, spent, report
		FROM dream_runs WHERE tenant = ? AND agent = ? AND project = ? AND usr = ? AND id = ?`,
		sc.Tenant, sc.Agent, sc.Project, sc.User, id)
	d, err := scanDreamRun(row)
	if errors.Is(err, sql.ErrNoRows) {
		return store.DreamRun{}, store.ErrNotFound
	}
	return d, err
}

func (r dreamRepo) Runs(ctx context.Context, limit int) ([]store.DreamRun, error) {
	if limit <= 0 {
		limit = 50
	}
	sc := r.t.sc
	rows, err := r.t.tx.QueryContext(ctx, `SELECT id, trigger, status, started_at, ended_at, budget, spent, report
		FROM dream_runs WHERE tenant = ? AND agent = ? AND project = ? AND usr = ?
		ORDER BY started_at DESC, id DESC LIMIT ?`,
		sc.Tenant, sc.Agent, sc.Project, sc.User, min(limit, store.MaxLimit))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []store.DreamRun
	for rows.Next() {
		d, err := scanDreamRun(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, d)
	}
	return out, rows.Err()
}

func scanDreamRun(s scanner) (store.DreamRun, error) {
	var (
		d              store.DreamRun
		started, ended string
		report         []byte
	)
	if err := s.Scan(&d.ID, &d.Trigger, &d.Status, &started, &ended, &d.Budget, &d.Spent, &report); err != nil {
		return store.DreamRun{}, err
	}
	var err error
	if d.StartedAt, err = parseTime(started); err != nil {
		return d, err
	}
	if ended != "" {
		if d.EndedAt, err = parseTime(ended); err != nil {
			return d, err
		}
	}
	if len(report) > 0 {
		d.Report = append(d.Report[:0], report...)
	}
	return d, nil
}
