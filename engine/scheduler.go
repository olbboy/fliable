package engine

import (
	"github.com/olbboy/fliable/store"
)

// schedulerLoop claims due jobs and executes them until Stop is called.
// It is the only background goroutine the engine runs: timers, async
// continuations, retries and timer-start events all flow through here.
func (e *Engine) schedulerLoop() {
	defer close(e.stopped)
	ticker := newTicker(e.pollInterval)
	defer ticker.Stop()
	for {
		select {
		case <-e.stop:
			return
		case <-ticker.C:
			e.RunDueJobs(100)
		}
	}
}

// RunDueJobs claims and executes up to limit due jobs immediately. The
// scheduler calls this continuously; tests and embedders with virtual
// clocks call it directly for deterministic time control.
func (e *Engine) RunDueJobs(limit int) int {
	jobs, err := e.st.DueJobs(e.now(), limit)
	if err != nil {
		e.log.Error("due job claim failed", "error", err)
		return 0
	}
	for _, job := range jobs {
		e.executeJob(job)
	}
	return len(jobs)
}

func (e *Engine) executeJob(job *store.Job) {
	e.metrics.JobsExecuted.Add(1)

	// Timer-start jobs create a fresh instance.
	if job.InstanceID == "" && job.DefinitionKey != "" {
		def, err := e.st.LatestDefinition(job.DefinitionKey)
		if err != nil {
			e.log.Error("timer start: definition gone", "key", job.DefinitionKey)
			return
		}
		if _, err := e.startInstanceAt(def.ID, "", nil, job.ElementID, "", ""); err != nil {
			e.log.Error("timer start failed", "key", job.DefinitionKey, "error", err)
		}
		// Reschedule cycles.
		if job.Interval > 0 && (job.Repeats > 1 || job.Repeats == -1) {
			next := *job
			next.ID = e.newID("job")
			next.DueAt = job.DueAt.Add(job.Interval)
			if next.Repeats > 0 {
				next.Repeats--
			}
			if err := e.st.PutJob(&next); err != nil {
				e.log.Error("timer start reschedule failed", "error", err)
			}
		}
		return
	}

	switch job.Kind {
	case store.JobTimer:
		err := e.resume(job.InstanceID, func(rt *runtime) (bool, error) {
			return rt.timerFired(job)
		})
		if err != nil {
			e.log.Warn("timer job skipped", "job", job.ID, "instance", job.InstanceID, "error", err)
		}

	case store.JobAsync, store.JobRetry:
		err := e.resume(job.InstanceID, func(rt *runtime) (bool, error) {
			tok := rt.inst.Tokens[job.TokenID]
			if tok == nil || tok.State != store.TokenWaitRetry || tok.WaitRef != job.ID {
				return false, nil
			}
			tok.State = store.TokenActive
			tok.WaitRef = ""
			return true, nil
		})
		if err != nil {
			e.log.Warn("continuation job skipped", "job", job.ID, "instance", job.InstanceID, "error", err)
		}
	}
}
