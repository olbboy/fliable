package store

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// Journal is a durable Store: every mutation is appended as a JSON line to
// a write-ahead journal, and the full state is periodically compacted into
// a snapshot file. Restart = load snapshot + replay journal. This gives
// crash-safe durability from a single binary with zero external
// dependencies — no JVM, no RDBMS, no schema migrations.
//
// For multi-node or SQL-backed deployments, implement the Store interface
// against your database of choice; the engine is storage-agnostic.
type Journal struct {
	mu   sync.Mutex
	mem  *Memory
	dir  string
	f    *os.File
	w    *bufio.Writer
	n    int // entries since last snapshot
	opts JournalOptions
}

// JournalOptions tunes the journal store.
type JournalOptions struct {
	// Fsync forces an fsync after every append. Slower but survives OS
	// crashes, not just process crashes. Default false.
	Fsync bool
	// CompactEvery triggers snapshot compaction after this many journal
	// entries. Default 20000.
	CompactEvery int
}

type journalEntry struct {
	Op string          `json:"op"`
	D  json.RawMessage `json:"d,omitempty"`
	ID string          `json:"id,omitempty"`
}

type snapshotFile struct {
	Definitions []*Definition              `json:"definitions"`
	Instances   []*Instance                `json:"instances"`
	Tasks       []*Task                    `json:"tasks"`
	Jobs        []*Job                     `json:"jobs"`
	Externals   []*ExternalTask            `json:"externals"`
	Subs        []*Subscription            `json:"subs"`
	Incidents   []*Incident                `json:"incidents"`
	History     map[string][]*HistoryEvent `json:"history"`
}

// OpenJournal opens (or creates) a journal store in dir.
func OpenJournal(dir string, opts JournalOptions) (*Journal, error) {
	if opts.CompactEvery <= 0 {
		opts.CompactEvery = 20000
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, fmt.Errorf("store: create dir: %w", err)
	}
	j := &Journal{mem: NewMemory(), dir: dir, opts: opts}
	if err := j.load(); err != nil {
		return nil, err
	}
	f, err := os.OpenFile(j.journalPath(), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return nil, fmt.Errorf("store: open journal: %w", err)
	}
	j.f = f
	j.w = bufio.NewWriter(f)
	return j, nil
}

func (j *Journal) journalPath() string  { return filepath.Join(j.dir, "journal.jsonl") }
func (j *Journal) snapshotPath() string { return filepath.Join(j.dir, "snapshot.json") }

func (j *Journal) load() error {
	// Snapshot first.
	if data, err := os.ReadFile(j.snapshotPath()); err == nil {
		var snap snapshotFile
		if err := json.Unmarshal(data, &snap); err != nil {
			return fmt.Errorf("store: corrupt snapshot: %w", err)
		}
		for _, d := range snap.Definitions {
			j.mem.definitions[d.ID] = d
		}
		for _, in := range snap.Instances {
			j.mem.instances[in.ID] = in
		}
		for _, t := range snap.Tasks {
			j.mem.tasks[t.ID] = t
		}
		for _, jb := range snap.Jobs {
			j.mem.jobs[jb.ID] = jb
		}
		for _, e := range snap.Externals {
			j.mem.externals[e.ID] = e
		}
		for _, s := range snap.Subs {
			j.mem.subs[s.ID] = s
		}
		for _, i := range snap.Incidents {
			j.mem.incidents[i.ID] = i
		}
		for id, evs := range snap.History {
			j.mem.history[id] = evs
			var maxSeq int64
			for _, ev := range evs {
				if ev.Seq > maxSeq {
					maxSeq = ev.Seq
				}
			}
			j.mem.histSeq[id] = maxSeq
		}
	} else if !os.IsNotExist(err) {
		return fmt.Errorf("store: read snapshot: %w", err)
	}

	// Replay journal.
	f, err := os.Open(j.journalPath())
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return fmt.Errorf("store: open journal: %w", err)
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 1<<20), 16<<20)
	line := 0
	for sc.Scan() {
		line++
		raw := sc.Bytes()
		if len(raw) == 0 {
			continue
		}
		var e journalEntry
		if err := json.Unmarshal(raw, &e); err != nil {
			// A torn final write after a crash is expected; stop replay
			// at the first corrupt line.
			break
		}
		if err := j.apply(e); err != nil {
			return fmt.Errorf("store: replay line %d: %w", line, err)
		}
		j.n++
	}
	return sc.Err()
}

func (j *Journal) apply(e journalEntry) error {
	switch e.Op {
	case "def":
		var d Definition
		if err := json.Unmarshal(e.D, &d); err != nil {
			return err
		}
		return j.mem.PutDefinition(&d)
	case "inst":
		var in Instance
		if err := json.Unmarshal(e.D, &in); err != nil {
			return err
		}
		// Replay keeps the recorded revision.
		j.mem.mu.Lock()
		j.mem.instances[in.ID] = &in
		j.mem.mu.Unlock()
		return nil
	case "task":
		var t Task
		if err := json.Unmarshal(e.D, &t); err != nil {
			return err
		}
		return j.mem.PutTask(&t)
	case "job":
		var jb Job
		if err := json.Unmarshal(e.D, &jb); err != nil {
			return err
		}
		return j.mem.PutJob(&jb)
	case "deljob":
		return j.mem.DeleteJob(e.ID)
	case "ext":
		var t ExternalTask
		if err := json.Unmarshal(e.D, &t); err != nil {
			return err
		}
		return j.mem.PutExternalTask(&t)
	case "sub":
		var s Subscription
		if err := json.Unmarshal(e.D, &s); err != nil {
			return err
		}
		return j.mem.PutSubscription(&s)
	case "delsub":
		return j.mem.DeleteSubscription(e.ID)
	case "inc":
		var i Incident
		if err := json.Unmarshal(e.D, &i); err != nil {
			return err
		}
		return j.mem.PutIncident(&i)
	case "hist":
		var ev HistoryEvent
		if err := json.Unmarshal(e.D, &ev); err != nil {
			return err
		}
		return j.mem.AppendHistory(&ev)
	}
	return fmt.Errorf("unknown journal op %q", e.Op)
}

// append writes a journal entry and possibly compacts.
func (j *Journal) append(op string, id string, v any) error {
	j.mu.Lock()
	defer j.mu.Unlock()
	e := journalEntry{Op: op, ID: id}
	if v != nil {
		d, err := json.Marshal(v)
		if err != nil {
			return fmt.Errorf("store: marshal %s: %w", op, err)
		}
		e.D = d
	}
	line, err := json.Marshal(e)
	if err != nil {
		return err
	}
	if _, err := j.w.Write(append(line, '\n')); err != nil {
		return fmt.Errorf("store: journal write: %w", err)
	}
	if err := j.w.Flush(); err != nil {
		return fmt.Errorf("store: journal flush: %w", err)
	}
	if j.opts.Fsync {
		if err := j.f.Sync(); err != nil {
			return fmt.Errorf("store: journal fsync: %w", err)
		}
	}
	j.n++
	if j.n >= j.opts.CompactEvery {
		return j.compactLocked()
	}
	return nil
}

// Compact forces snapshot compaction now.
func (j *Journal) Compact() error {
	j.mu.Lock()
	defer j.mu.Unlock()
	return j.compactLocked()
}

func (j *Journal) compactLocked() error {
	j.mem.mu.RLock()
	snap := snapshotFile{History: j.mem.history}
	for _, d := range j.mem.definitions {
		snap.Definitions = append(snap.Definitions, d)
	}
	for _, in := range j.mem.instances {
		snap.Instances = append(snap.Instances, in)
	}
	for _, t := range j.mem.tasks {
		snap.Tasks = append(snap.Tasks, t)
	}
	for _, jb := range j.mem.jobs {
		snap.Jobs = append(snap.Jobs, jb)
	}
	for _, e := range j.mem.externals {
		snap.Externals = append(snap.Externals, e)
	}
	for _, s := range j.mem.subs {
		snap.Subs = append(snap.Subs, s)
	}
	for _, i := range j.mem.incidents {
		snap.Incidents = append(snap.Incidents, i)
	}
	data, err := json.Marshal(&snap)
	j.mem.mu.RUnlock()
	if err != nil {
		return fmt.Errorf("store: marshal snapshot: %w", err)
	}

	tmp := j.snapshotPath() + ".tmp"
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		return fmt.Errorf("store: write snapshot: %w", err)
	}
	if err := os.Rename(tmp, j.snapshotPath()); err != nil {
		return fmt.Errorf("store: rename snapshot: %w", err)
	}

	// Truncate the journal: snapshot now holds everything.
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

// Close flushes and closes the journal.
func (j *Journal) Close() error {
	j.mu.Lock()
	defer j.mu.Unlock()
	if err := j.w.Flush(); err != nil {
		return err
	}
	return j.f.Close()
}

// ---- Store interface: write-through to memory + journal --------------------

// PutDefinition implements Store.
func (j *Journal) PutDefinition(d *Definition) error {
	if err := j.mem.PutDefinition(d); err != nil {
		return err
	}
	return j.append("def", "", d)
}

// GetDefinition implements Store.
func (j *Journal) GetDefinition(id string) (*Definition, error) { return j.mem.GetDefinition(id) }

// LatestDefinition implements Store.
func (j *Journal) LatestDefinition(key string) (*Definition, error) {
	return j.mem.LatestDefinition(key)
}

// ListDefinitions implements Store.
func (j *Journal) ListDefinitions(latestOnly bool) ([]*Definition, error) {
	return j.mem.ListDefinitions(latestOnly)
}

// PutInstance implements Store.
func (j *Journal) PutInstance(inst *Instance) error {
	if err := j.mem.PutInstance(inst); err != nil {
		return err
	}
	return j.append("inst", "", inst)
}

// GetInstance implements Store.
func (j *Journal) GetInstance(id string) (*Instance, error) { return j.mem.GetInstance(id) }

// ListInstances implements Store.
func (j *Journal) ListInstances(f InstanceFilter) ([]*Instance, error) {
	return j.mem.ListInstances(f)
}

// PutTask implements Store.
func (j *Journal) PutTask(t *Task) error {
	if err := j.mem.PutTask(t); err != nil {
		return err
	}
	return j.append("task", "", t)
}

// GetTask implements Store.
func (j *Journal) GetTask(id string) (*Task, error) { return j.mem.GetTask(id) }

// ListTasks implements Store.
func (j *Journal) ListTasks(f TaskFilter) ([]*Task, error) { return j.mem.ListTasks(f) }

// PutJob implements Store.
func (j *Journal) PutJob(jb *Job) error {
	if err := j.mem.PutJob(jb); err != nil {
		return err
	}
	return j.append("job", "", jb)
}

// GetJob implements Store.
func (j *Journal) GetJob(id string) (*Job, error) { return j.mem.GetJob(id) }

// DeleteJob implements Store.
func (j *Journal) DeleteJob(id string) error {
	if err := j.mem.DeleteJob(id); err != nil {
		return err
	}
	return j.append("deljob", id, nil)
}

// DueJobs implements Store.
func (j *Journal) DueJobs(now time.Time, limit int) ([]*Job, error) {
	jobs, err := j.mem.DueJobs(now, limit)
	if err != nil {
		return nil, err
	}
	for _, jb := range jobs {
		if err := j.append("deljob", jb.ID, nil); err != nil {
			return nil, err
		}
	}
	return jobs, nil
}

// ListJobs implements Store.
func (j *Journal) ListJobs(instanceID string) ([]*Job, error) { return j.mem.ListJobs(instanceID) }

// PutExternalTask implements Store.
func (j *Journal) PutExternalTask(t *ExternalTask) error {
	if err := j.mem.PutExternalTask(t); err != nil {
		return err
	}
	return j.append("ext", "", t)
}

// GetExternalTask implements Store.
func (j *Journal) GetExternalTask(id string) (*ExternalTask, error) {
	return j.mem.GetExternalTask(id)
}

// FetchAndLockExternalTasks implements Store.
func (j *Journal) FetchAndLockExternalTasks(topic, workerID string, until, now time.Time, limit int) ([]*ExternalTask, error) {
	tasks, err := j.mem.FetchAndLockExternalTasks(topic, workerID, until, now, limit)
	if err != nil {
		return nil, err
	}
	for _, t := range tasks {
		if err := j.append("ext", "", t); err != nil {
			return nil, err
		}
	}
	return tasks, nil
}

// ListExternalTasks implements Store.
func (j *Journal) ListExternalTasks(instanceID string) ([]*ExternalTask, error) {
	return j.mem.ListExternalTasks(instanceID)
}

// PutSubscription implements Store.
func (j *Journal) PutSubscription(s *Subscription) error {
	if err := j.mem.PutSubscription(s); err != nil {
		return err
	}
	return j.append("sub", "", s)
}

// DeleteSubscription implements Store.
func (j *Journal) DeleteSubscription(id string) error {
	if err := j.mem.DeleteSubscription(id); err != nil {
		return err
	}
	return j.append("delsub", id, nil)
}

// ListSubscriptions implements Store.
func (j *Journal) ListSubscriptions(f SubscriptionFilter) ([]*Subscription, error) {
	return j.mem.ListSubscriptions(f)
}

// PutIncident implements Store.
func (j *Journal) PutIncident(i *Incident) error {
	if err := j.mem.PutIncident(i); err != nil {
		return err
	}
	return j.append("inc", "", i)
}

// GetIncident implements Store.
func (j *Journal) GetIncident(id string) (*Incident, error) { return j.mem.GetIncident(id) }

// ListIncidents implements Store.
func (j *Journal) ListIncidents(f IncidentFilter) ([]*Incident, error) {
	return j.mem.ListIncidents(f)
}

// AppendHistory implements Store.
func (j *Journal) AppendHistory(ev *HistoryEvent) error {
	if err := j.mem.AppendHistory(ev); err != nil {
		return err
	}
	return j.append("hist", "", ev)
}

// ListHistory implements Store.
func (j *Journal) ListHistory(instanceID string, f HistoryFilter) ([]*HistoryEvent, error) {
	return j.mem.ListHistory(instanceID, f)
}

var _ Store = (*Memory)(nil)
var _ Store = (*Journal)(nil)
