package sqlite

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"

	"github.com/Dubjay18/reverie/internal/store"
)

type journalRepo struct{ t *txn }

func (r journalRepo) Append(ctx context.Context, e store.JournalEntry) (int64, error) {
	sc := r.t.sc
	eps, _ := json.Marshal(e.SourceEpisodes)
	if e.SourceEpisodes == nil {
		eps = []byte("[]")
	}
	res, err := r.t.tx.ExecContext(ctx, `INSERT INTO journal
		(tenant, agent, project, usr, run_id, phase, op, target_kind, target_id, before, after, rationale, source_episodes, tokens, at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		sc.Tenant, sc.Agent, sc.Project, sc.User, e.RunID, e.Phase, e.Op, e.TargetKind, e.TargetID,
		[]byte(e.Before), []byte(e.After), e.Rationale, string(eps), e.Tokens, fmtTime(e.At))
	if err != nil {
		return 0, err
	}
	return res.LastInsertId()
}

const journalCols = `id, run_id, phase, op, target_kind, target_id, before, after, rationale, source_episodes, tokens, at, reverted`

func (r journalRepo) Get(ctx context.Context, id int64) (store.JournalEntry, error) {
	sc := r.t.sc
	row := r.t.tx.QueryRowContext(ctx, `SELECT `+journalCols+` FROM journal
		WHERE tenant = ? AND agent = ? AND project = ? AND usr = ? AND id = ?`, sc.Tenant, sc.Agent, sc.Project, sc.User, id)
	e, err := scanJournal(row)
	if errors.Is(err, sql.ErrNoRows) {
		return store.JournalEntry{}, store.ErrNotFound
	}
	return e, err
}

func (r journalRepo) ByRun(ctx context.Context, runID string) ([]store.JournalEntry, error) {
	sc := r.t.sc
	rows, err := r.t.tx.QueryContext(ctx, `SELECT `+journalCols+` FROM journal
		WHERE tenant = ? AND agent = ? AND project = ? AND usr = ? AND run_id = ? ORDER BY id LIMIT ?`,
		sc.Tenant, sc.Agent, sc.Project, sc.User, runID, store.MaxLimit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []store.JournalEntry
	for rows.Next() {
		e, err := scanJournal(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, e)
	}
	return out, rows.Err()
}

func (r journalRepo) Recent(ctx context.Context, limit int) ([]store.JournalEntry, error) {
	sc := r.t.sc
	if limit <= 0 {
		limit = store.DefaultLimit
	}
	rows, err := r.t.tx.QueryContext(ctx, `SELECT `+journalCols+` FROM journal
		WHERE tenant = ? AND agent = ? AND project = ? AND usr = ? ORDER BY id DESC LIMIT ?`,
		sc.Tenant, sc.Agent, sc.Project, sc.User, min(limit, store.MaxLimit))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []store.JournalEntry
	for rows.Next() {
		e, err := scanJournal(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, e)
	}
	return out, rows.Err()
}

func (r journalRepo) MarkReverted(ctx context.Context, id int64) error {
	sc := r.t.sc
	res, err := r.t.tx.ExecContext(ctx, `UPDATE journal SET reverted = 1
		WHERE tenant = ? AND agent = ? AND project = ? AND usr = ? AND id = ?`, sc.Tenant, sc.Agent, sc.Project, sc.User, id)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return store.ErrNotFound
	}
	return nil
}

func scanJournal(s scanner) (store.JournalEntry, error) {
	var (
		e        store.JournalEntry
		before   []byte
		after    []byte
		eps, at  string
		reverted int
	)
	if err := s.Scan(&e.ID, &e.RunID, &e.Phase, &e.Op, &e.TargetKind, &e.TargetID, &before, &after, &e.Rationale, &eps, &e.Tokens, &at, &reverted); err != nil {
		return e, err
	}
	if len(before) > 0 {
		e.Before = json.RawMessage(before)
	}
	if len(after) > 0 {
		e.After = json.RawMessage(after)
	}
	_ = json.Unmarshal([]byte(eps), &e.SourceEpisodes)
	var err error
	if e.At, err = parseTime(at); err != nil {
		return e, err
	}
	e.Reverted = reverted != 0
	return e, nil
}
