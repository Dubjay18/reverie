package postgres

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"github.com/Dubjay18/reverie/internal/store"
	"github.com/jackc/pgx/v5"
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

// skillVisible: the project's shared skills and those narrowed to the
// handle's agent ($1 tenant, $2 project, $3 agent).
const skillVisible = `tenant = $1 AND project = $2 AND agent IN ('', $3)`

func (r skillRepo) args(extra ...any) []any {
	sc := r.t.sc
	return append([]any{sc.Tenant, sc.Project, sc.Agent}, extra...)
}

func (r skillRepo) Save(ctx context.Context, s store.Skill) error {
	if r.t.readOnly {
		return errReadOnly
	}
	sc := r.t.sc
	if s.Agent != "" && s.Agent != sc.Agent {
		return store.ErrScopeMismatch
	}
	tag, err := r.t.tx.Exec(ctx, `INSERT INTO skills (tenant, project, `+skillCols+`)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, $15)
		ON CONFLICT (tenant, project, id, version) DO UPDATE SET
			agent = EXCLUDED.agent, name = EXCLUDED.name, trigger = EXCLUDED.trigger, steps = EXCLUDED.steps,
			status = EXCLUDED.status, uses = EXCLUDED.uses, successes = EXCLUDED.successes,
			episode_ids = EXCLUDED.episode_ids, created_at = EXCLUDED.created_at,
			updated_at = EXCLUDED.updated_at, last_used = EXCLUDED.last_used
		WHERE skills.agent IN ('', $16)`,
		sc.Tenant, sc.Project, s.ID, s.Version, s.Agent, s.Name, s.Trigger, jsonList(s.Steps), string(s.Status),
		s.Uses, s.Successes, jsonList(s.EpisodeIDs), fmtTime(s.CreatedAt), fmtTime(s.UpdatedAt), fmtOpt(s.LastUsed),
		sc.Agent)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return store.ErrScopeMismatch // the row is another agent's narrowed skill
	}
	return nil
}

func (r skillRepo) Get(ctx context.Context, id string, version int) (store.Skill, error) {
	q := `SELECT ` + skillCols + ` FROM skills WHERE ` + skillVisible + ` AND id = $4`
	args := r.args(id)
	if version > 0 {
		q += ` AND version = $5`
		args = append(args, version)
	}
	s, err := scanSkill(r.t.tx.QueryRow(ctx, q+` ORDER BY version DESC LIMIT 1`, args...))
	if errors.Is(err, pgx.ErrNoRows) {
		return store.Skill{}, store.ErrNotFound
	}
	return s, err
}

func (r skillRepo) Versions(ctx context.Context, id string) ([]store.Skill, error) {
	return r.list(ctx, `SELECT `+skillCols+` FROM skills WHERE `+skillVisible+` AND id = $4 ORDER BY version LIMIT $5`,
		r.args(id, store.MaxLimit)...)
}

func (r skillRepo) List(ctx context.Context, q store.SkillQuery) ([]store.Skill, error) {
	if q.Status != "" {
		return r.list(ctx, `SELECT `+skillCols+` FROM skills WHERE `+skillVisible+` AND status = $4
			ORDER BY updated_at DESC, id, version DESC LIMIT $5`, r.args(string(q.Status), limitOf(q.Limit))...)
	}
	return r.list(ctx, `SELECT `+skillCols+` FROM skills WHERE `+skillVisible+`
		ORDER BY updated_at DESC, id, version DESC LIMIT $4`, r.args(limitOf(q.Limit))...)
}

func (r skillRepo) list(ctx context.Context, q string, args ...any) ([]store.Skill, error) {
	rows, err := r.t.tx.Query(ctx, q, args...)
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
	if r.t.readOnly {
		return errReadOnly
	}
	sc := r.t.sc
	_, err := r.t.tx.Exec(ctx, `INSERT INTO insights (tenant, agent, project, usr, `+insightCols+`)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13)
		ON CONFLICT (tenant, agent, project, usr, id) DO UPDATE SET
			body = EXCLUDED.body, entities = EXCLUDED.entities, confidence = EXCLUDED.confidence,
			status = EXCLUDED.status, episode_ids = EXCLUDED.episode_ids, confirmed_by = EXCLUDED.confirmed_by,
			created_at = EXCLUDED.created_at, confirmed_at = EXCLUDED.confirmed_at`,
		sc.Tenant, sc.Agent, sc.Project, sc.User, in.ID, in.Body, jsonList(in.Entities), in.Confidence, string(in.Status),
		jsonList(in.EpisodeIDs), jsonList(in.ConfirmedBy), fmtTime(in.CreatedAt), fmtOpt(in.ConfirmedAt))
	return err
}

func (r insightRepo) Get(ctx context.Context, id string) (store.Insight, error) {
	sc := r.t.sc
	in, err := scanInsight(r.t.tx.QueryRow(ctx, `SELECT `+insightCols+` FROM insights
		WHERE tenant = $1 AND agent = $2 AND project = $3 AND usr = $4 AND id = $5`,
		sc.Tenant, sc.Agent, sc.Project, sc.User, id))
	if errors.Is(err, pgx.ErrNoRows) {
		return store.Insight{}, store.ErrNotFound
	}
	return in, err
}

func (r insightRepo) List(ctx context.Context, q store.InsightQuery) ([]store.Insight, error) {
	sc := r.t.sc
	where := `tenant = $1 AND agent = $2 AND project = $3 AND usr = $4`
	args := []any{sc.Tenant, sc.Agent, sc.Project, sc.User}
	lim := `$5`
	if q.Status != "" {
		where += ` AND status = $5`
		args = append(args, string(q.Status))
		lim = `$6`
	}
	rows, err := r.t.tx.Query(ctx, `SELECT `+insightCols+` FROM insights WHERE `+where+
		` ORDER BY created_at DESC, id DESC LIMIT `+lim, append(args, limitOf(q.Limit))...)
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
