package sqlite

import (
	"context"
	"strings"
	"time"

	"github.com/Dubjay18/reverie"
	"github.com/Dubjay18/reverie/internal/store"
)

type factRepo struct{ t *txn }

const factCols = `id, entity, entity_type, relation, value, confidence, source, status, pinned,
	valid_from, valid_to, recorded_at, superseded_by`

const liveClause = `valid_to = '' AND status IN ('active', 'contested')`

func (r factRepo) Save(ctx context.Context, f reverie.Fact) error {
	sc := r.t.sc
	validTo := ""
	if !f.ValidTo.IsZero() {
		validTo = fmtTime(f.ValidTo)
	}
	if _, err := r.t.tx.ExecContext(ctx, `INSERT INTO facts
		(tenant, agent, project, usr, `+factCols+`)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(tenant, agent, project, usr, id) DO UPDATE SET
			entity = excluded.entity, entity_type = excluded.entity_type, relation = excluded.relation,
			value = excluded.value, confidence = excluded.confidence, source = excluded.source,
			status = excluded.status, pinned = excluded.pinned, valid_from = excluded.valid_from,
			valid_to = excluded.valid_to, recorded_at = excluded.recorded_at, superseded_by = excluded.superseded_by`,
		sc.Tenant, sc.Agent, sc.Project, sc.User,
		f.ID, f.Entity, f.EntityType, f.Relation, f.Value, f.Confidence, string(f.Source), string(f.Status), b2i(f.Pinned),
		fmtTime(f.ValidFrom), validTo, fmtTime(f.RecordedAt), f.SupersededBy); err != nil {
		return err
	}
	if _, err := r.t.tx.ExecContext(ctx, `DELETE FROM fact_sources
		WHERE tenant = ? AND agent = ? AND project = ? AND usr = ? AND fact_id = ?`,
		sc.Tenant, sc.Agent, sc.Project, sc.User, f.ID); err != nil {
		return err
	}
	for _, ep := range f.EpisodeIDs {
		if _, err := r.t.tx.ExecContext(ctx, `INSERT OR IGNORE INTO fact_sources
			(tenant, agent, project, usr, fact_id, episode_id) VALUES (?, ?, ?, ?, ?, ?)`,
			sc.Tenant, sc.Agent, sc.Project, sc.User, f.ID, ep); err != nil {
			return err
		}
	}
	return nil
}

func (r factRepo) Get(ctx context.Context, id string) (reverie.Fact, error) {
	facts, err := r.query(ctx, `id = ?`, []any{id}, ``, 1)
	if err != nil {
		return reverie.Fact{}, err
	}
	if len(facts) == 0 {
		return reverie.Fact{}, store.ErrNotFound
	}
	return facts[0], nil
}

func (r factRepo) Live(ctx context.Context, entity, relation string) ([]reverie.Fact, error) {
	return r.query(ctx, liveClause+` AND entity = ? AND relation = ?`, []any{entity, relation}, `ORDER BY valid_from, id`, store.MaxLimit)
}

func (r factRepo) Query(ctx context.Context, q store.FactQuery) ([]reverie.Fact, error) {
	where, args := filters(q)
	return r.query(ctx, liveClause+where, args, `ORDER BY confidence DESC, recorded_at DESC, id`, limitOf(q.Limit))
}

func (r factRepo) AsOf(ctx context.Context, q store.FactQuery, t time.Time) ([]reverie.Fact, error) {
	where, args := filters(q)
	at := fmtTime(t)
	return r.query(ctx, `status != 'tombstoned' AND valid_from <= ? AND (valid_to = '' OR valid_to > ?)`+where,
		append([]any{at, at}, args...), `ORDER BY valid_from, id`, limitOf(q.Limit))
}

func (r factRepo) Related(ctx context.Context, names []string, limit int) ([]reverie.Fact, error) {
	if len(names) == 0 {
		return nil, nil
	}
	if len(names) > 20 {
		names = names[:20]
	}
	marks := strings.TrimSuffix(strings.Repeat("?, ", len(names)), ", ")
	args := make([]any, 0, 2*len(names))
	lowered := make([]any, len(names))
	for i, n := range names {
		lowered[i] = strings.ToLower(strings.TrimSpace(n))
	}
	args = append(args, lowered...)
	args = append(args, lowered...)
	return r.query(ctx, liveClause+` AND (entity IN (`+marks+`) OR lower(value) IN (`+marks+`))`, args,
		`ORDER BY confidence DESC, recorded_at DESC, id`, limitOf(limit))
}

func filters(q store.FactQuery) (string, []any) {
	var where string
	var args []any
	if q.Entity != "" {
		where += ` AND entity = ?`
		args = append(args, strings.ToLower(strings.TrimSpace(q.Entity)))
	}
	if q.Relation != "" {
		where += ` AND relation = ?`
		args = append(args, strings.ToLower(strings.TrimSpace(q.Relation)))
	}
	if q.OnlyContested {
		where += ` AND status = 'contested'`
	}
	if q.OnlyPinned {
		where += ` AND pinned = 1`
	}
	return where, args
}

func limitOf(n int) int {
	if n <= 0 {
		return store.DefaultLimit
	}
	return min(n, store.MaxLimit)
}

// query runs a SELECT over this scope's facts. where and order are built only
// from constant fragments in this file; every value goes through args.
func (r factRepo) query(ctx context.Context, where string, args []any, order string, limit int) ([]reverie.Fact, error) {
	sc := r.t.sc
	full := append([]any{sc.Tenant, sc.Agent, sc.Project, sc.User}, args...)
	full = append(full, limit)
	rows, err := r.t.tx.QueryContext(ctx, `SELECT `+factCols+` FROM facts
		WHERE tenant = ? AND agent = ? AND project = ? AND usr = ? AND `+where+` `+order+` LIMIT ?`, full...)
	if err != nil {
		return nil, err
	}
	var out []reverie.Fact
	for rows.Next() {
		var (
			f                           reverie.Fact
			source, status, vf, vt, rec string
			pinned                      int
		)
		if err := rows.Scan(&f.ID, &f.Entity, &f.EntityType, &f.Relation, &f.Value, &f.Confidence, &source, &status,
			&pinned, &vf, &vt, &rec, &f.SupersededBy); err != nil {
			rows.Close()
			return nil, err
		}
		f.Source, f.Status, f.Pinned = reverie.FactSource(source), reverie.FactStatus(status), pinned != 0
		if f.ValidFrom, err = parseTime(vf); err != nil {
			rows.Close()
			return nil, err
		}
		if vt != "" {
			if f.ValidTo, err = parseTime(vt); err != nil {
				rows.Close()
				return nil, err
			}
		}
		if f.RecordedAt, err = parseTime(rec); err != nil {
			rows.Close()
			return nil, err
		}
		out = append(out, f)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return nil, err
	}
	rows.Close()

	// provenance, after the first cursor is closed (one connection per tx)
	for i := range out {
		eps, err := r.sources(ctx, out[i].ID)
		if err != nil {
			return nil, err
		}
		out[i].EpisodeIDs = eps
	}
	return out, nil
}

func (r factRepo) sources(ctx context.Context, factID string) ([]string, error) {
	sc := r.t.sc
	rows, err := r.t.tx.QueryContext(ctx, `SELECT episode_id FROM fact_sources
		WHERE tenant = ? AND agent = ? AND project = ? AND usr = ? AND fact_id = ? ORDER BY episode_id`,
		sc.Tenant, sc.Agent, sc.Project, sc.User, factID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		ids = append(ids, id)
	}
	return ids, rows.Err()
}
