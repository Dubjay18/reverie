package sqlite

import (
	"context"
	"encoding/json"

	"github.com/Dubjay18/reverie"
	"github.com/Dubjay18/reverie/internal/store"
	"github.com/oklog/ulid/v2"
)

type eventRepo struct{ t *txn }

func (r eventRepo) Append(ctx context.Context, ev reverie.Event) (bool, error) {
	if ev.Scope != r.t.sc {
		return false, store.ErrScopeMismatch
	}
	if err := ev.Validate(); err != nil {
		return false, err
	}
	sc := r.t.sc
	res, err := r.t.tx.ExecContext(ctx, `INSERT INTO events
		(id, tenant, agent, project, usr, task_id, kind, origin, at, payload, token_cost)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(tenant, agent, project, usr, id) DO NOTHING`,
		ev.ID.String(), sc.Tenant, sc.Agent, sc.Project, sc.User, ev.TaskID,
		string(ev.Kind), string(ev.Origin), fmtTime(ev.At), []byte(ev.Payload), ev.TokenCost)
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	return n == 1, err
}

func (r eventRepo) List(ctx context.Context, q store.EventQuery) ([]reverie.Event, error) {
	limit := q.Limit
	if limit <= 0 {
		limit = store.DefaultLimit
	}
	limit = min(limit, store.MaxLimit)
	sc := r.t.sc
	query := `SELECT id, task_id, kind, origin, at, payload, token_cost FROM events
		WHERE tenant = ? AND agent = ? AND project = ? AND usr = ?`
	args := []any{sc.Tenant, sc.Agent, sc.Project, sc.User}
	if q.TaskID != "" {
		query += ` AND task_id = ?`
		args = append(args, q.TaskID)
	}
	if q.After != (ulid.ULID{}) {
		query += ` AND id > ?`
		args = append(args, q.After.String())
	}
	if q.From != (ulid.ULID{}) {
		query += ` AND id >= ?`
		args = append(args, q.From.String())
	}
	if q.To != (ulid.ULID{}) {
		query += ` AND id <= ?`
		args = append(args, q.To.String())
	}
	query += ` ORDER BY id LIMIT ?`
	args = append(args, limit)

	rows, err := r.t.tx.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []reverie.Event
	for rows.Next() {
		var (
			id, kind, origin, at string
			ev                   reverie.Event
			payload              []byte
		)
		if err := rows.Scan(&id, &ev.TaskID, &kind, &origin, &at, &payload, &ev.TokenCost); err != nil {
			return nil, err
		}
		if ev.ID, err = ulid.ParseStrict(id); err != nil {
			return nil, err
		}
		if ev.At, err = parseTime(at); err != nil {
			return nil, err
		}
		ev.Scope, ev.Kind, ev.Origin = sc, reverie.EventKind(kind), reverie.Origin(origin)
		if len(payload) > 0 {
			ev.Payload = json.RawMessage(payload)
		}
		out = append(out, ev)
	}
	return out, rows.Err()
}

func (r eventRepo) UpdatePayload(ctx context.Context, id ulid.ULID, payload json.RawMessage) error {
	sc := r.t.sc
	res, err := r.t.tx.ExecContext(ctx, `UPDATE events SET payload = ?
		WHERE tenant = ? AND agent = ? AND project = ? AND usr = ? AND id = ?`,
		[]byte(payload), sc.Tenant, sc.Agent, sc.Project, sc.User, id.String())
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return store.ErrNotFound
	}
	return nil
}
