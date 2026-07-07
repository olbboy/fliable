package engine

import (
	"time"

	"github.com/olbboy/fliable/store"
)

// HousekeepingPolicy controls retention of finished instances and their
// history. Unbounded history is a production liability (disk, query
// latency); this bounds it from day one.
type HousekeepingPolicy struct {
	// TTL is how long a completed or terminated instance (and all its
	// records) is retained after it ends. Zero disables purging.
	TTL time.Duration
	// Interval is how often the background sweep runs. Defaults to 1h.
	Interval time.Duration
	// BatchSize caps how many instances one sweep purges (0 = 500).
	BatchSize int
}

// WithHousekeeping enables periodic retention sweeps of finished instances.
func WithHousekeeping(p HousekeepingPolicy) Option {
	return func(e *Engine) {
		if p.Interval <= 0 {
			p.Interval = time.Hour
		}
		if p.BatchSize <= 0 {
			p.BatchSize = 500
		}
		e.housekeeping = &p
	}
}

// RunHousekeeping purges finished instances whose end time is older than
// the retention TTL, returning how many were purged. Safe to call
// directly (tests, cron, ops endpoints) regardless of the background
// sweep.
func (e *Engine) RunHousekeeping(now time.Time) int {
	if e.housekeeping == nil || e.housekeeping.TTL <= 0 {
		return 0
	}
	cutoff := now.Add(-e.housekeeping.TTL)
	purged := 0
	for _, state := range []store.InstanceState{store.InstanceCompleted, store.InstanceTerminated} {
		insts, err := e.st.ListInstances(store.InstanceFilter{
			State:       state,
			EndedBefore: cutoff,
			Limit:       e.housekeeping.BatchSize,
		})
		if err != nil {
			e.log.Error("housekeeping list failed", "state", state, "error", err)
			continue
		}
		for _, inst := range insts {
			// Never purge an instance a live parent still waits on.
			if inst.ParentID != "" {
				if parent, err := e.st.GetInstance(inst.ParentID); err == nil && parent.State == store.InstanceActive {
					continue
				}
			}
			if err := e.st.PurgeInstance(inst.ID); err != nil {
				e.log.Error("housekeeping purge failed", "instance", inst.ID, "error", err)
				continue
			}
			purged++
		}
	}
	if purged > 0 {
		e.log.Info("housekeeping purged finished instances", "count", purged, "olderThan", e.housekeeping.TTL.String())
	}
	return purged
}

// housekeepingLoop runs retention sweeps until the engine stops.
func (e *Engine) housekeepingLoop() {
	ticker := newTicker(e.housekeeping.Interval)
	defer ticker.Stop()
	for {
		select {
		case <-e.stop:
			return
		case <-ticker.C:
			e.RunHousekeeping(e.now())
		}
	}
}
