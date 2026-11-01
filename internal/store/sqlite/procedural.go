package sqlite

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"time"

	"github.com/Dubjay18/reverie/internal/store"
)

func (t *txn) Skills() store.SkillRepo     { return skillRepo{t} }
func (t *txn) Insights() store.InsightRepo { return insightRepo{t} }

func fmtOpt(t time.Time) string {
	if t.IsZero() {
		return ""
	}
	return fmtTime(t)
}

func parseOpt(s string) (time.Time, error) {
	if s == "" {
		return time.Time{}, nil
	}
	return parseTime(s)
}

func jsonList(v []string) string {
	if len(v) == 0 {
		return "[]"
	}
	b, _ := json.Marshal(v)
	return string(b)
}

type skillRepo struct{ t *txn }

const skillCols = `id, version, agent, name, trigger, steps, status, uses, successes, episode_ids, created_at, updated_at, last_used`

// visible is the skills a handle may see: its project's shared skills and
// those narrowed to its own agent.
const skillVisible = `tenant = ? AND project = ? AND agent IN ('', ?)`

func (r skillRepo) args(extra ...any) []any {
	sc := r.t.sc
	return append([]any{sc.Tenant, sc.Project, sc.Agent}, extra...)
}

func (r skillRepo) Save(ctx context.Context, s store.Skill) error {
	sc := r.t.sc
	if s.Agent != "" && s.Agent != sc.Agent {
		return store.ErrScopeMismatch
	}
	res, err := r.t.tx.ExecContext(ctx, `INSERT INTO skills (tenant, project, `+skillCols+`)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(tenant, project, id, version) DO UPDATE SET
			agent = excluded.agent, name = excluded.name, trigger = excluded.trigger, steps = excluded.steps,
			status = excluded.status, uses = excluded.uses, successes = excluded.successes,
			episode_ids = excluded.episode_ids, created_at = excluded.created_at,
			updated_at = excluded.updated_at, last_used = excluded.last_used
		WHERE skills.agent IN ('', ?)`,
		sc.Tenant, sc.Project, s.ID, s.Version, s.Agent, s.Name, s.Trigger, jsonList(s.Steps), string(s.Status),
		s.Uses, s.Successes, jsonList(s.EpisodeIDs), fmtTime(s.CreatedAt), fmtTime(s.UpdatedAt), fmtOpt(s.LastUsed),
		sc.Agent)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return store.ErrScopeMismatch // the row is another agent's narrowed skill
	}
	return nil
}

func (r skillRepo) Get(ctx context.Context, id string, version int) (store.Skill, error) {
	q := `SELECT ` + skillCols + ` FROM skills WHERE ` + skillVisible + ` AND id = ?`
	args := r.args(id)
	if version > 0 {
		q += ` AND version = ?`
		args = append(args, version)
	}
	s, err := scanSkill(r.t.tx.QueryRowContext(ctx, q+` ORDER BY version DESC LIMIT 1`, args...))
	if errors.Is(err, sql.ErrNoRows) {
		return store.Skill{}, store.ErrNotFound
	}
	return s, err
}

func (r skillRepo) Versions(ctx context.Context, id string) ([]store.Skill, error) {
	return r.list(ctx, `SELECT `+skillCols+` FROM skills WHERE `+skillVisible+` AND id = ? ORDER BY version LIMIT ?`,
		r.args(id, store.MaxLimit)...)
}

func (r skillRepo) List(ctx context.Context, q store.SkillQuery) ([]store.Skill, error) {
	where, args := skillVisible, r.args()
	if q.Status != "" {
		where += ` AND status = ?`
		args = append(args, string(q.Status))
	}
	return r.list(ctx, `SELECT `+skillCols+` FROM skills WHERE `+where+` ORDER BY updated_at DESC, id, version DESC LIMIT ?`,
		append(args, limitOf(q.Limit))...)
}

func (r skillRepo) list(ctx context.Context, q string, args ...any) ([]store.Skill, error) {
	rows, err := r.t.tx.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []store.Skill
	for rows.Next() {
		s, err := scanSkill(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, s)
	}
	return out, rows.Err()
}

func scanSkill(sc scanner) (store.Skill, error) {
	var (
		s                         store.Skill
		steps, eps, status        string
		created, updated, lastUse string
	)
	if err := sc.Scan(&s.ID, &s.Version, &s.Agent, &s.Name, &s.Trigger, &steps, &status, &s.Uses, &s.Successes,
		&eps, &created, &updated, &lastUse); err != nil {
		return s, err
	}
	s.Status = store.SkillStatus(status)
	_ = json.Unmarshal([]byte(steps), &s.Steps)
	_ = json.Unmarshal([]byte(eps), &s.EpisodeIDs)
	var err error
	if s.CreatedAt, err = parseTime(created); err != nil {
		return s, err
	}
	if s.UpdatedAt, err = parseTime(updated); err != nil {
		return s, err
	}
	s.LastUsed, err = parseOpt(lastUse)
	return s, err
}

type insightRepo struct{ t *txn }

const insightCols = `id, body, entities, confidence, status, episode_ids, confirmed_by, created_at, confirmed_at`

func (r insightRepo) Save(ctx context.Context, in store.Insight) error {
	sc := r.t.sc
	_, err := r.t.tx.ExecContext(ctx, `INSERT INTO insights (tenant, agent, project, usr, `+insightCols+`)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(tenant, agent, project, usr, id) DO UPDATE SET
			body = excluded.body, entities = excluded.entities, confidence = excluded.confidence,
			status = excluded.status, episode_ids = excluded.episode_ids, confirmed_by = excluded.confirmed_by,
			created_at = excluded.created_at, confirmed_at = excluded.confirmed_at`,
		sc.Tenant, sc.Agent, sc.Project, sc.User, in.ID, in.Body, jsonList(in.Entities), in.Confidence, string(in.Status),
		jsonList(in.EpisodeIDs), jsonList(in.ConfirmedBy), fmtTime(in.CreatedAt), fmtOpt(in.ConfirmedAt))
	return err
}

func (r insightRepo) Get(ctx context.Context, id string) (store.Insight, error) {
	sc := r.t.sc
	in, err := scanInsight(r.t.tx.QueryRowContext(ctx, `SELECT `+insightCols+` FROM insights
		WHERE tenant = ? AND agent = ? AND project = ? AND usr = ? AND id = ?`,
		sc.Tenant, sc.Agent, sc.Project, sc.User, id))
	if errors.Is(err, sql.ErrNoRows) {
		return store.Insight{}, store.ErrNotFound
	}
	return in, err
}

func (r insightRepo) List(ctx context.Context, q store.InsightQuery) ([]store.Insight, error) {
	sc := r.t.sc
	where := `tenant = ? AND agent = ? AND project = ? AND usr = ?`
	args := []any{sc.Tenant, sc.Agent, sc.Project, sc.User}
	if q.Status != "" {
		where += ` AND status = ?`
		args = append(args, string(q.Status))
	}
	rows, err := r.t.tx.QueryContext(ctx, `SELECT `+insightCols+` FROM insights WHERE `+where+
		` ORDER BY created_at DESC, id DESC LIMIT ?`, append(args, limitOf(q.Limit))...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []store.Insight
	for rows.Next() {
		in, err := scanInsight(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, in)
	}
	return out, rows.Err()
}

func scanInsight(sc scanner) (store.Insight, error) {
	var (
		in                    store.Insight
		ents, eps, by, status string
		created, confirmed    string
	)
	if err := sc.Scan(&in.ID, &in.Body, &ents, &in.Confidence, &status, &eps, &by, &created, &confirmed); err != nil {
		return in, err
	}
	in.Status = store.InsightStatus(status)
	_ = json.Unmarshal([]byte(ents), &in.Entities)
	_ = json.Unmarshal([]byte(eps), &in.EpisodeIDs)
	_ = json.Unmarshal([]byte(by), &in.ConfirmedBy)
	var err error
	if in.CreatedAt, err = parseTime(created); err != nil {
		return in, err
	}
	in.ConfirmedAt, err = parseOpt(confirmed)
	return in, err
}
