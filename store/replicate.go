package store

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
)

// Replication primitives on the journal store. A leader journal streams
// its append-only entries to a warm standby (see package replicate); the
// standby applies each entry to its own memory image and journal file,
// so promotion is just "open an engine on the follower's store". This is
// asynchronous replication: RPO is the in-flight batch, RTO is engine
// start time (~milliseconds).

// OnAppend registers a callback invoked with a copy of every journal
// line after it is locally durable, in write order. Callbacks run under
// the journal lock: hand the line to a channel or buffer, never block.
func (j *Journal) OnAppend(fn func(line []byte)) {
	j.mu.Lock()
	defer j.mu.Unlock()
	j.onAppend = append(j.onAppend, fn)
}

// StateSnapshot serializes the journal's full current state, suitable
// for bootstrapping a follower via ResetFromSnapshot.
func (j *Journal) StateSnapshot() ([]byte, error) {
	j.mu.Lock()
	defer j.mu.Unlock()
	return j.snapshotData()
}

// ResetFromSnapshot replaces the journal's entire state with a snapshot
// produced by StateSnapshot: the in-memory image is rebuilt, the
// snapshot is written to disk and the journal file truncated. Follower
// bootstrap / full resync.
func (j *Journal) ResetFromSnapshot(data []byte) error {
	var snap snapshotFile
	if err := json.Unmarshal(data, &snap); err != nil {
		return fmt.Errorf("store: bad snapshot: %w", err)
	}
	j.mu.Lock()
	defer j.mu.Unlock()

	// Rebuild the state inside the existing Memory under its write lock:
	// the j.mem pointer never changes, so concurrent readers stay safe.
	m := j.mem
	m.mu.Lock()
	m.definitions = map[string]*Definition{}
	m.instances = map[string]*Instance{}
	m.tasks = map[string]*Task{}
	m.jobs = map[string]*Job{}
	m.externals = map[string]*ExternalTask{}
	m.agentJobs = map[string]*AgentJob{}
	m.subs = map[string]*Subscription{}
	m.incidents = map[string]*Incident{}
	m.blobs = map[string]*Blob{}
	m.history = map[string][]*HistoryEvent{}
	m.histSeq = map[string]int64{}
	for _, d := range snap.Definitions {
		m.definitions[d.ID] = d
	}
	for _, in := range snap.Instances {
		m.instances[in.ID] = in
	}
	for _, t := range snap.Tasks {
		m.tasks[t.ID] = t
	}
	for _, jb := range snap.Jobs {
		m.jobs[jb.ID] = jb
	}
	for _, e := range snap.Externals {
		m.externals[e.ID] = e
	}
	for _, a := range snap.AgentJobs {
		m.agentJobs[a.ID] = a
	}
	for _, s := range snap.Subs {
		m.subs[s.ID] = s
	}
	for _, i := range snap.Incidents {
		m.incidents[i.ID] = i
	}
	for _, b := range snap.Blobs {
		m.blobs[blobKey(b.Kind, b.Key)] = b
	}
	for id, evs := range snap.History {
		m.history[id] = evs
		var maxSeq int64
		for _, ev := range evs {
			if ev.Seq > maxSeq {
				maxSeq = ev.Seq
			}
		}
		m.histSeq[id] = maxSeq
	}
	m.mu.Unlock()

	// Persist: snapshot file + empty journal, atomically enough for a
	// standby (a crash mid-reset re-syncs from the leader anyway).
	tmp := j.snapshotPath() + ".tmp"
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		return fmt.Errorf("store: write snapshot: %w", err)
	}
	if err := os.Rename(tmp, j.snapshotPath()); err != nil {
		return fmt.Errorf("store: rename snapshot: %w", err)
	}
	if err := j.f.Close(); err != nil {
		return err
	}
	f, err := os.OpenFile(j.journalPath(), os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o644)
	if err != nil {
		return fmt.Errorf("store: reset journal: %w", err)
	}
	j.f = f
	j.w = bufio.NewWriter(f)
	j.n = 0
	return nil
}

// ApplyReplicated applies one journal line received from a leader: the
// entry mutates the in-memory image and is appended verbatim to the
// local journal. History entries older than the local high-water mark
// are skipped, so a resync overlap never duplicates the audit trail.
func (j *Journal) ApplyReplicated(line []byte) error {
	var e journalEntry
	if err := json.Unmarshal(line, &e); err != nil {
		return fmt.Errorf("store: bad replicated entry: %w", err)
	}
	j.mu.Lock()
	defer j.mu.Unlock()

	if e.Op == "hist" {
		var ev HistoryEvent
		if err := json.Unmarshal(e.D, &ev); err != nil {
			return err
		}
		j.mem.mu.RLock()
		seen := ev.Seq != 0 && ev.Seq <= j.mem.histSeq[ev.InstanceID]
		j.mem.mu.RUnlock()
		if seen {
			return nil
		}
	}
	if err := j.apply(e); err != nil {
		return err
	}
	if _, err := j.w.Write(append(line, '\n')); err != nil {
		return fmt.Errorf("store: journal write: %w", err)
	}
	if err := j.w.Flush(); err != nil {
		return fmt.Errorf("store: journal flush: %w", err)
	}
	j.n++
	if j.n >= j.opts.CompactEvery {
		return j.compactLocked()
	}
	return nil
}
