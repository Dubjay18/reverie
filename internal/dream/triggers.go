package dream

import (
	"context"
	"time"

	"github.com/Dubjay18/reverie/internal/store"
)

// Trigger constants recorded on DreamRun.Trigger.
const (
	TriggerManual   = "manual"
	TriggerPressure = "pressure"
	TriggerIdle     = "idle"
	TriggerHook     = "hook"
	TriggerCron     = "cron"
)

// TriggerPolicy defines thresholds for automatic dreaming.
type TriggerPolicy struct {
	PressureEpisodes int           // dream when unconsolidated closed episodes >= this (default 5)
	IdleAfter        time.Duration // dream when newest unconsolidated episode ended >= IdleAfter ago (default 30m)
	CronInterval     time.Duration // dream when last finished dream ended >= CronInterval ago (default 24h)
}

func (p TriggerPolicy) defaults() TriggerPolicy {
	if p.PressureEpisodes <= 0 {
		p.PressureEpisodes = 5
	}
	if p.IdleAfter <= 0 {
		p.IdleAfter = 30 * time.Minute
	}
	if p.CronInterval <= 0 {
		p.CronInterval = 24 * time.Hour
	}
	return p
}

// ShouldTrigger inspects the scope's read-only view and reports which trigger
// condition (pressure, idle, or cron) is satisfied at now, or "" if none is.
func ShouldTrigger(ctx context.Context, sc store.Scoped, pol TriggerPolicy, now time.Time) (string, error) {
	pol = pol.defaults()
	view := NewReadView(sc)
	u, err := view.Unconsolidated(ctx, pol.PressureEpisodes+1)
	if err != nil || len(u) == 0 {
		return "", err
	}
	if len(u) >= pol.PressureEpisodes {
		return TriggerPressure, nil
	}
	var newestEnd time.Time
	for _, ep := range u {
		if ep.EndedAt.After(newestEnd) {
			newestEnd = ep.EndedAt
		}
	}
	if !newestEnd.IsZero() && now.Sub(newestEnd) >= pol.IdleAfter {
		return TriggerIdle, nil
	}
	var lastDream time.Time
	_ = sc.View(ctx, func(tx store.Tx) error {
		runs, err := tx.Dreams().Runs(ctx, 1)
		if err == nil && len(runs) > 0 {
			lastDream = runs[0].StartedAt
		}
		return nil
	})
	if lastDream.IsZero() || now.Sub(lastDream) >= pol.CronInterval {
		return TriggerCron, nil
	}
	return "", nil
}
