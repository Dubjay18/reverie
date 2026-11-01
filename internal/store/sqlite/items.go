package sqlite

import (
	"context"
	"encoding/binary"
	"fmt"
	"math"
	"regexp"
	"sort"
	"strings"

	"github.com/Dubjay18/reverie/internal/store"
)

type indexRepo struct{ t *txn }

func (r indexRepo) Upsert(ctx context.Context, it store.Item) error {
	sc := r.t.sc
	if _, err := r.t.tx.ExecContext(ctx, `INSERT INTO items (tenant, agent, project, usr, kind, ref_id, text, embedding)
		VALUES (?, ?, ?, ?, ?, ?, ?, NULL)
		ON CONFLICT(tenant, agent, project, usr, kind, ref_id) DO UPDATE SET text = excluded.text, embedding = NULL, embed_failures = 0`,
		sc.Tenant, sc.Agent, sc.Project, sc.User, it.Kind, it.RefID, it.Text); err != nil {
		return err
	}
	if _, err := r.t.tx.ExecContext(ctx, `DELETE FROM items_fts
		WHERE tenant = ? AND agent = ? AND project = ? AND usr = ? AND kind = ? AND ref_id = ?`,
		sc.Tenant, sc.Agent, sc.Project, sc.User, it.Kind, it.RefID); err != nil {
		return err
	}
	if _, err := r.t.tx.ExecContext(ctx, `INSERT INTO items_fts (text, tenant, agent, project, usr, kind, ref_id)
		VALUES (?, ?, ?, ?, ?, ?, ?)`,
		it.Text, sc.Tenant, sc.Agent, sc.Project, sc.User, it.Kind, it.RefID); err != nil {
		return err
	}
	if len(it.Embedding) > 0 {
		_, err := r.SetEmbedding(ctx, it.Kind, it.RefID, it.Text, it.Embedding)
		return err
	}
	return nil
}

func (r indexRepo) Delete(ctx context.Context, kind, refID string) error {
	sc := r.t.sc
	for _, q := range []string{
		`DELETE FROM items WHERE tenant = ? AND agent = ? AND project = ? AND usr = ? AND kind = ? AND ref_id = ?`,
		`DELETE FROM items_fts WHERE tenant = ? AND agent = ? AND project = ? AND usr = ? AND kind = ? AND ref_id = ?`,
	} {
		if _, err := r.t.tx.ExecContext(ctx, q, sc.Tenant, sc.Agent, sc.Project, sc.User, kind, refID); err != nil {
			return err
		}
	}
	return nil
}

func (r indexRepo) MarkEmbedFailed(ctx context.Context, kind, refID string) error {
	sc := r.t.sc
	_, err := r.t.tx.ExecContext(ctx, `UPDATE items SET embed_failures = embed_failures + 1
		WHERE tenant = ? AND agent = ? AND project = ? AND usr = ? AND kind = ? AND ref_id = ?`,
		sc.Tenant, sc.Agent, sc.Project, sc.User, kind, refID)
	return err
}

// Vectors are stored unit-length, so a search is a dot product per row.
func (r indexRepo) SetEmbedding(ctx context.Context, kind, refID, text string, vec []float32) (bool, error) {
	if r.t.dims > 0 && len(vec) != r.t.dims {
		return false, fmt.Errorf("sqlite: vector has %d dimensions, database is pinned to %d", len(vec), r.t.dims)
	}
	sc := r.t.sc
	res, err := r.t.tx.ExecContext(ctx, `UPDATE items SET embedding = ?
		WHERE tenant = ? AND agent = ? AND project = ? AND usr = ? AND kind = ? AND ref_id = ? AND text = ?`,
		encodeVec(normalized(vec)), sc.Tenant, sc.Agent, sc.Project, sc.User, kind, refID, text)
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	return n == 1, err
}

func (r indexRepo) Pending(ctx context.Context, limit int) ([]store.Item, error) {
	if limit <= 0 {
		limit = store.DefaultLimit
	}
	sc := r.t.sc
	rows, err := r.t.tx.QueryContext(ctx, `SELECT kind, ref_id, text FROM items
		WHERE tenant = ? AND agent = ? AND project = ? AND usr = ? AND embedding IS NULL AND embed_failures < ?
		ORDER BY rowid LIMIT ?`, sc.Tenant, sc.Agent, sc.Project, sc.User, store.MaxEmbedFailures, min(limit, store.MaxLimit))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []store.Item
	for rows.Next() {
		var it store.Item
		if err := rows.Scan(&it.Kind, &it.RefID, &it.Text); err != nil {
			return nil, err
		}
		out = append(out, it)
	}
	return out, rows.Err()
}

var wordRE = regexp.MustCompile(`[\p{L}\p{N}]+`)

// ftsQuery turns free text into an FTS5 expression that cannot be a syntax
// error: every word is quoted, joined with OR, and bm25 ranks the matches.
// Words are split the way the default tokenizer splits them, so "ERR_42"
// becomes "err" OR "42".
func ftsQuery(q string) string {
	words := wordRE.FindAllString(q, 64)
	if len(words) == 0 {
		return ""
	}
	for i, w := range words {
		words[i] = `"` + strings.ToLower(w) + `"`
	}
	return strings.Join(words, " OR ")
}

func (r indexRepo) SearchText(ctx context.Context, query string, limit int) ([]store.Hit, error) {
	match := ftsQuery(query)
	if match == "" {
		return nil, nil
	}
	if limit <= 0 {
		limit = 50
	}
	sc := r.t.sc
	rows, err := r.t.tx.QueryContext(ctx, `SELECT kind, ref_id FROM items_fts
		WHERE items_fts MATCH ? AND tenant = ? AND agent = ? AND project = ? AND usr = ?
		ORDER BY bm25(items_fts) LIMIT ?`,
		match, sc.Tenant, sc.Agent, sc.Project, sc.User, min(limit, store.MaxLimit))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []store.Hit
	for rows.Next() {
		var h store.Hit
		if err := rows.Scan(&h.Kind, &h.RefID); err != nil {
			return nil, err
		}
		out = append(out, h)
	}
	return out, rows.Err()
}

func (r indexRepo) SearchVector(ctx context.Context, vec []float32, limit int) ([]store.Hit, error) {
	if len(vec) == 0 {
		return nil, nil
	}
	if limit <= 0 {
		limit = 50
	}
	q := normalized(vec)
	sc := r.t.sc
	rows, err := r.t.tx.QueryContext(ctx, `SELECT kind, ref_id, embedding FROM items
		WHERE tenant = ? AND agent = ? AND project = ? AND usr = ? AND embedding IS NOT NULL`,
		sc.Tenant, sc.Agent, sc.Project, sc.User)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	type scored struct {
		h store.Hit
		s float32
	}
	var all []scored
	for rows.Next() {
		var (
			h    store.Hit
			blob []byte
		)
		if err := rows.Scan(&h.Kind, &h.RefID, &blob); err != nil {
			return nil, err
		}
		if len(blob) != 4*len(q) {
			continue // embedded by another model; never comparable
		}
		all = append(all, scored{h, dotBlob(q, blob)})
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	sort.SliceStable(all, func(i, j int) bool {
		if all[i].s != all[j].s {
			return all[i].s > all[j].s
		}
		return all[i].h.RefID < all[j].h.RefID
	})
	limit = min(limit, len(all))
	out := make([]store.Hit, limit)
	for i := range out {
		out[i] = all[i].h
	}
	return out, nil
}

func encodeVec(v []float32) []byte {
	b := make([]byte, 4*len(v))
	for i, f := range v {
		binary.LittleEndian.PutUint32(b[4*i:], math.Float32bits(f))
	}
	return b
}

// dotBlob is dot(q, decode(b)) without allocating the decoded vector, which
// matters when a search scans every row.
func dotBlob(q []float32, b []byte) (s float32) {
	for i, f := range q {
		s += f * math.Float32frombits(binary.LittleEndian.Uint32(b[4*i:]))
	}
	return s
}

func normalized(v []float32) []float32 {
	var n float64
	for _, f := range v {
		n += float64(f) * float64(f)
	}
	if n == 0 {
		return v
	}
	inv := float32(1 / math.Sqrt(n))
	out := make([]float32, len(v))
	for i, f := range v {
		out[i] = f * inv
	}
	return out
}
