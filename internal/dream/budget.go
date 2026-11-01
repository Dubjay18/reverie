// Package dream implements offline, budgeted, transactional memory consolidation.
package dream

import "errors"

var ErrBudgetExhausted = errors.New("dream: token budget exhausted")

// Budget tracks token spending across a dream run and enforces priority-ordered
// skipping: Validate (and deterministic phases) always run; Distill is the
// first LLM phase skipped when the budget is exhausted.
type Budget struct {
	limit int
	spent int
}

// NewBudget returns a tracker capped at limit tokens (<= 0 means 0 budget).
func NewBudget(limit int) *Budget {
	if limit < 0 {
		limit = 0
	}
	return &Budget{limit: limit}
}

func (b *Budget) Limit() int     { return b.limit }
func (b *Budget) Spent() int     { return b.spent }
func (b *Budget) Remaining() int { return max(b.limit-b.spent, 0) }

// Spend records n used tokens. It returns ErrBudgetExhausted if spending n
// exceeds the remaining budget, while still recording the spend so accounting
// reflects actual model usage.
func (b *Budget) Spend(n int) error {
	if n <= 0 {
		return nil
	}
	b.spent += n
	if b.spent > b.limit {
		return ErrBudgetExhausted
	}
	return nil
}

// Allow reports whether a phase with cost estimate est (min 1 for LLM phases,
// 0 for deterministic phases) may run. Deterministic phases (est == 0) always
// run; LLM phases require Remaining() >= est.
func (b *Budget) Allow(est int) bool {
	if est <= 0 {
		return true
	}
	return b.Remaining() >= est
}
