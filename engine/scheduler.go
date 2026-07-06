package engine

import (
	"errors"
	"fmt"
	"strings"

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
			e.requeueFailedJob(job, err)
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
			e.requeueFailedJob(job, err)
		}
	}
}

// requeueFailedJob keeps at-least-once semantics: DueJobs removed the job
// from the store before execution, so a failed execution must put it back
// (with a fresh due time) or surface an incident — never drop it silently.
func (e *Engine) requeueFailedJob(job *store.Job, cause error) {
	msg := cause.Error()
	if errors.Is(cause, store.ErrNotFound) ||
		strings.Contains(msg, "is completed") || strings.Contains(msg, "is terminated") {
		// The instance is gone; the job is genuinely obsolete.
		e.log.Debug("job dropped, instance ended", "job", job.ID, "error", cause)
		return
	}
	if job.Retries > 1 {
		retry := *job
		retry.Retries--
		retry.DueAt = e.now().Add(e.retryBackoff)
		if err := e.st.PutJob(&retry); err == nil {
			e.log.Warn("job execution failed, requeued", "job", job.ID, "error", cause)
			return
		}
	}
	inc := &store.Incident{
		ID:         e.newID("incd"),
		InstanceID: job.InstanceID,
		TokenID:    job.TokenID,
		ElementID:  job.ElementID,
		Message:    fmt.Sprintf("job %s (%s) failed permanently: %v", job.ID, job.Kind, cause),
		CreatedAt:  e.now(),
	}
	if err := e.st.PutIncident(inc); err != nil {
		e.log.Error("job failed and incident write failed", "job", job.ID, "cause", cause, "error", err)
		return
	}
	e.metrics.IncidentsCreated.Add(1)
	e.log.Error("job failed permanently, incident raised", "job", job.ID, "incident", inc.ID, "error", cause)
}
