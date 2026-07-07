package store

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"
)

// SQL is a Store over database/sql: Postgres, MySQL, SQLite or anything
// with a driver. Fliable itself stays zero-dependency — the embedding
// application imports the driver and hands over an opened *sql.DB:
//
//	db, _ := sql.Open("pgx", dsn)                  // driver lives in YOUR go.mod
//	st, _ := store.NewSQL(db, store.SQLOptions{Dialect: store.DialectPostgres})
//	eng := engine.New(st)
//
// Design: one generic record table. Every record marshals to JSON in the
// data column; a handful of indexed columns (kind, id, tenant, k,
// instance, state, due, seq) narrow scans, and fine-grained filtering
// reuses the same predicate logic as the in-memory store, so all
// backends behave identically. Claims (due jobs, external/agent task
// locks) are optimistic single-row UPDATE/DELETE guards, so multiple
// engine replicas can safely share one database.
type SQL struct {
	db *sql.DB
	d  SQLDialect

	seqMu   sync.Mutex
	histSeq map[string]int64
}

// SQLDialect adapts placeholder style and upsert syntax.
type SQLDialect int

// Supported dialects.
const (
	// DialectPostgres: $n placeholders, ON CONFLICT upsert. Also correct
	// for CockroachDB.
	DialectPostgres SQLDialect = iota
	// DialectSQLite: ? placeholders, ON CONFLICT upsert.
	DialectSQLite
	// DialectMySQL: ? placeholders, ON DUPLICATE KEY upsert.
	DialectMySQL
)

// SQLOptions configures NewSQL.
type SQLOptions struct {
	Dialect SQLDialect
	// SkipMigrate disables automatic schema creation (run the DDL from
	// Schema yourself, e.g. in a migration tool).
	SkipMigrate bool
}

// Schema returns the DDL the SQL store needs.
func Schema() string {
	return `CREATE TABLE IF NOT EXISTS fliable_records (
  kind     VARCHAR(16)  NOT NULL,
  id       VARCHAR(255) NOT NULL,
  tenant   VARCHAR(255) NOT NULL DEFAULT '',
  k        VARCHAR(255) NOT NULL DEFAULT '',
  instance VARCHAR(255) NOT NULL DEFAULT '',
  state    VARCHAR(32)  NOT NULL DEFAULT '',
  due      BIGINT       NOT NULL DEFAULT 0,
  seq      BIGINT       NOT NULL DEFAULT 0,
  data     TEXT         NOT NULL,
  PRIMARY KEY (kind, id)
);
CREATE INDEX IF NOT EXISTS fliable_records_kind_instance ON fliable_records (kind, instance);
CREATE INDEX IF NOT EXISTS fliable_records_kind_due ON fliable_records (kind, due);
CREATE INDEX IF NOT EXISTS fliable_records_kind_k ON fliable_records (kind, k);`
}

// NewSQL creates a Store over an opened database handle.
func NewSQL(db *sql.DB, opts SQLOptions) (*SQL, error) {
	s := &SQL{db: db, d: opts.Dialect, histSeq: map[string]int64{}}
	if !opts.SkipMigrate {
		for _, stmt := range strings.Split(Schema(), ";") {
			stmt = strings.TrimSpace(stmt)
			if stmt == "" {
				continue
			}
			if _, err := db.Exec(stmt); err != nil {
				return nil, fmt.Errorf("store: migrate: %w", err)
			}
		}
	}
	return s, nil
}

// Close implements Store (the *sql.DB belongs to the caller).
func (s *SQL) Close() error { return nil }

// q rewrites ? placeholders for the dialect.
func (s *SQL) q(query string) string {
	if s.d != DialectPostgres {
		return query
	}
	var b strings.Builder
	n := 0
	for _, r := range query {
		if r == '?' {
			n++
			fmt.Fprintf(&b, "$%d", n)
			continue
		}
		b.WriteRune(r)
	}
	return b.String()
}

// row is the column set of one record.
type row struct {
	kind, id, tenant, k, instance, state string
	due, seq                             int64
	data                                 []byte
}

func (s *SQL) upsert(r row) error {
	var query string
	switch s.d {
	case DialectMySQL:
		query = `INSERT INTO fliable_records (kind,id,tenant,k,instance,state,due,seq,data)
VALUES (?,?,?,?,?,?,?,?,?)
ON DUPLICATE KEY UPDATE tenant=VALUES(tenant), k=VALUES(k), instance=VALUES(instance),
state=VALUES(state), due=VALUES(due), seq=VALUES(seq), data=VALUES(data)`
	default:
		query = `INSERT INTO fliable_records (kind,id,tenant,k,instance,state,due,seq,data)
VALUES (?,?,?,?,?,?,?,?,?)
ON CONFLICT (kind, id) DO UPDATE SET tenant=excluded.tenant, k=excluded.k,
instance=excluded.instance, state=excluded.state, due=excluded.due, seq=excluded.seq, data=excluded.data`
	}
	_, err := s.db.Exec(s.q(query), r.kind, r.id, r.tenant, r.k, r.instance, r.state, r.due, r.seq, string(r.data))
	return err
}

func (s *SQL) getData(kind, id string, v any) error {
	var data string
	err := s.db.QueryRow(s.q(`SELECT data FROM fliable_records WHERE kind=? AND id=?`), kind, id).Scan(&data)
	if errors.Is(err, sql.ErrNoRows) {
		return ErrNotFound
	}
	if err != nil {
		return err
	}
	return json.Unmarshal([]byte(data), v)
}

// scanKind loads every record of a kind (optionally narrowed by
// instance) and unmarshals each row through fn.
func (s *SQL) scanKind(kind, instance string, fn func(data []byte) error) error {
	var (
		rows *sql.Rows
		err  error
	)
	if instance == "" {
		rows, err = s.db.Query(s.q(`SELECT data FROM fliable_records WHERE kind=?`), kind)
	} else {
		rows, err = s.db.Query(s.q(`SELECT data FROM fliable_records WHERE kind=? AND instance=?`), kind, instance)
	}
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var data string
		if err := rows.Scan(&data); err != nil {
			return err
		}
		if err := fn([]byte(data)); err != nil {
			return err
		}
	}
	return rows.Err()
}

func (s *SQL) delete(kind, id string) (bool, error) {
	res, err := s.db.Exec(s.q(`DELETE FROM fliable_records WHERE kind=? AND id=?`), kind, id)
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	return n > 0, err
}

func marshal(v any) []byte {
	data, _ := json.Marshal(v)
	return data
}

func unixOrZero(t time.Time) int64 {
	if t.IsZero() {
		return 0
	}
	return t.UnixNano()
}

// ---- definitions -------------------------------------------------------------

// PutDefinition implements Store.
func (s *SQL) PutDefinition(d *Definition) error {
	return s.upsert(row{kind: "def", id: d.ID, tenant: d.TenantID, k: d.Key, seq: int64(d.Version), data: marshal(d)})
}

// GetDefinition implements Store.
func (s *SQL) GetDefinition(id string) (*Definition, error) {
	var d Definition
	if err := s.getData("def", id, &d); err != nil {
		return nil, err
	}
	return &d, nil
}

// LatestDefinition implements Store.
func (s *SQL) LatestDefinition(key string) (*Definition, error) {
	return s.LatestDefinitionForTenant("", key)
}

// LatestDefinitionForTenant implements Store.
func (s *SQL) LatestDefinitionForTenant(tenantID, key string) (*Definition, error) {
	var best *Definition
	err := s.scanKind("def", "", func(data []byte) error {
		var d Definition
		if err := json.Unmarshal(data, &d); err != nil {
			return err
		}
		if d.Key == key && d.TenantID == tenantID && (best == nil || d.Version > best.Version) {
			best = &d
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	if best == nil {
		return nil, ErrNotFound
	}
	return best, nil
}

// ListDefinitions implements Store.
func (s *SQL) ListDefinitions(latestOnly bool) ([]*Definition, error) {
	var all []*Definition
	err := s.scanKind("def", "", func(data []byte) error {
		var d Definition
		if err := json.Unmarshal(data, &d); err != nil {
			return err
		}
		all = append(all, &d)
		return nil
	})
	if err != nil {
		return nil, err
	}
	if latestOnly {
		byKey := map[string]*Definition{}
		for _, d := range all {
			if cur, ok := byKey[d.Key]; !ok || d.Version > cur.Version {
				byKey[d.Key] = d
			}
		}
		all = all[:0]
		for _, d := range byKey {
			all = append(all, d)
		}
	}
	sort.Slice(all, func(i, j int) bool {
		if all[i].Key != all[j].Key {
			return all[i].Key < all[j].Key
		}
		return all[i].Version < all[j].Version
	})
	return all, nil
}

// ---- instances ---------------------------------------------------------------

// PutInstance implements Store.
func (s *SQL) PutInstance(inst *Instance) error {
	inst.Rev++
	return s.upsert(row{
		kind: "inst", id: inst.ID, tenant: inst.TenantID, k: inst.DefinitionKey,
		instance: inst.ID, state: string(inst.State), data: marshal(inst),
	})
}

// GetInstance implements Store.
func (s *SQL) GetInstance(id string) (*Instance, error) {
	var in Instance
	if err := s.getData("inst", id, &in); err != nil {
		return nil, err
	}
	return &in, nil
}

// ListInstances implements Store.
func (s *SQL) ListInstances(f InstanceFilter) ([]*Instance, error) {
	var out []*Instance
	err := s.scanKind("inst", "", func(data []byte) error {
		var in Instance
		if err := json.Unmarshal(data, &in); err != nil {
			return err
		}
		if matchInstance(f, &in) {
			out = append(out, &in)
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	sortByID(out, func(i *Instance) string { return i.ID }, f.Desc)
	if f.Limit > 0 && len(out) > f.Limit {
		out = out[:f.Limit]
	}
	return out, nil
}

// PurgeInstance implements Store.
func (s *SQL) PurgeInstance(id string) error {
	// Every dependent record carries the instance column — including the
	// instance row itself and its history lines.
	_, err := s.db.Exec(s.q(`DELETE FROM fliable_records WHERE instance=?`), id)
	if err != nil {
		return err
	}
	s.seqMu.Lock()
	delete(s.histSeq, id)
	s.seqMu.Unlock()
	return nil
}

// ---- tasks ---------------------------------------------------------------------

// PutTask implements Store.
func (s *SQL) PutTask(t *Task) error {
	return s.upsert(row{
		kind: "task", id: t.ID, tenant: t.TenantID, k: t.DefinitionKey,
		instance: t.InstanceID, state: string(t.State), data: marshal(t),
	})
}

// GetTask implements Store.
func (s *SQL) GetTask(id string) (*Task, error) {
	var t Task
	if err := s.getData("task", id, &t); err != nil {
		return nil, err
	}
	return &t, nil
}

// ListTasks implements Store.
func (s *SQL) ListTasks(f TaskFilter) ([]*Task, error) {
	var out []*Task
	varsCache := map[string]map[string]any{}
	err := s.scanKind("task", f.InstanceID, func(data []byte) error {
		var t Task
		if err := json.Unmarshal(data, &t); err != nil {
			return err
		}
		if matchTask(f, &t, func() map[string]any {
			if vars, ok := varsCache[t.InstanceID]; ok {
				return vars
			}
			inst, err := s.GetInstance(t.InstanceID)
			if err != nil {
				varsCache[t.InstanceID] = nil
				return nil
			}
			varsCache[t.InstanceID] = inst.Variables
			return inst.Variables
		}) {
			out = append(out, &t)
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	sortByID(out, func(t *Task) string { return t.ID }, f.Desc)
	if f.Limit > 0 && len(out) > f.Limit {
		out = out[:f.Limit]
	}
	return out, nil
}

// ---- jobs -----------------------------------------------------------------------

// PutJob implements Store.
func (s *SQL) PutJob(j *Job) error {
	return s.upsert(row{
		kind: "job", id: j.ID, tenant: j.TenantID, k: j.DefinitionKey,
		instance: j.InstanceID, due: unixOrZero(j.DueAt), data: marshal(j),
	})
}

// GetJob implements Store.
func (s *SQL) GetJob(id string) (*Job, error) {
	var j Job
	if err := s.getData("job", id, &j); err != nil {
		return nil, err
	}
	return &j, nil
}

// DeleteJob implements Store.
func (s *SQL) DeleteJob(id string) error {
	_, err := s.delete("job", id)
	return err
}

// DueJobs implements Store. The DELETE-as-claim makes each job go to
// exactly one caller even with several engine replicas on one database.
func (s *SQL) DueJobs(now time.Time, limit int) ([]*Job, error) {
	var due []*Job
	err := s.scanKind("job", "", func(data []byte) error {
		var j Job
		if err := json.Unmarshal(data, &j); err != nil {
			return err
		}
		if !j.DueAt.After(now) {
			due = append(due, &j)
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	sort.Slice(due, func(i, j int) bool {
		if !due[i].DueAt.Equal(due[j].DueAt) {
			return due[i].DueAt.Before(due[j].DueAt)
		}
		return due[i].ID < due[j].ID
	})
	if limit > 0 && len(due) > limit {
		due = due[:limit]
	}
	claimed := due[:0]
	for _, j := range due {
		ok, err := s.delete("job", j.ID)
		if err != nil {
			return nil, err
		}
		if ok {
			claimed = append(claimed, j)
		}
	}
	return claimed, nil
}

// ListJobs implements Store.
func (s *SQL) ListJobs(instanceID string) ([]*Job, error) {
	var out []*Job
	err := s.scanKind("job", instanceID, func(data []byte) error {
		var j Job
		if err := json.Unmarshal(data, &j); err != nil {
			return err
		}
		out = append(out, &j)
		return nil
	})
	if err != nil {
		return nil, err
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out, nil
}

// ---- external tasks ----------------------------------------------------------------

// PutExternalTask implements Store.
func (s *SQL) PutExternalTask(t *ExternalTask) error {
	return s.upsert(row{
		kind: "ext", id: t.ID, k: t.Topic, instance: t.InstanceID,
		state: string(t.State), due: unixOrZero(t.LockUntil), data: marshal(t),
	})
}

// GetExternalTask implements Store.
func (s *SQL) GetExternalTask(id string) (*ExternalTask, error) {
	var t ExternalTask
	if err := s.getData("ext", id, &t); err != nil {
		return nil, err
	}
	return &t, nil
}

// claimPending optimistically locks one pending row: the UPDATE succeeds
// only while the row is still pending and its lock has expired.
func (s *SQL) claimPending(kind, id string, newData []byte, until, now time.Time) (bool, error) {
	res, err := s.db.Exec(s.q(
		`UPDATE fliable_records SET data=?, due=? WHERE kind=? AND id=? AND state=? AND due<=?`),
		string(newData), unixOrZero(until), kind, id, "pending", now.UnixNano())
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	return n > 0, err
}

// FetchAndLockExternalTasks implements Store.
func (s *SQL) FetchAndLockExternalTasks(topic, workerID string, until, now time.Time, limit int) ([]*ExternalTask, error) {
	var avail []*ExternalTask
	err := s.scanKind("ext", "", func(data []byte) error {
		var t ExternalTask
		if err := json.Unmarshal(data, &t); err != nil {
			return err
		}
		if t.Topic != topic || t.State != ExternalPending {
			return nil
		}
		if t.LockedBy != "" && t.LockUntil.After(now) {
			return nil
		}
		avail = append(avail, &t)
		return nil
	})
	if err != nil {
		return nil, err
	}
	sort.Slice(avail, func(i, j int) bool { return avail[i].ID < avail[j].ID })
	if limit > 0 && len(avail) > limit {
		avail = avail[:limit]
	}
	out := make([]*ExternalTask, 0, len(avail))
	for _, t := range avail {
		t.LockedBy = workerID
		t.LockUntil = until
		ok, err := s.claimPending("ext", t.ID, marshal(t), until, now)
		if err != nil {
			return nil, err
		}
		if ok {
			out = append(out, t)
		}
	}
	return out, nil
}

// ListExternalTasks implements Store.
func (s *SQL) ListExternalTasks(instanceID string) ([]*ExternalTask, error) {
	var out []*ExternalTask
	err := s.scanKind("ext", instanceID, func(data []byte) error {
		var t ExternalTask
		if err := json.Unmarshal(data, &t); err != nil {
			return err
		}
		out = append(out, &t)
		return nil
	})
	if err != nil {
		return nil, err
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out, nil
}

// ---- agent jobs ----------------------------------------------------------------------

// PutAgentJob implements Store.
func (s *SQL) PutAgentJob(j *AgentJob) error {
	return s.upsert(row{
		kind: "agentjob", id: j.ID, tenant: j.TenantID, k: j.Topic,
		instance: j.InstanceID, state: string(j.State), due: unixOrZero(j.LockUntil), data: marshal(j),
	})
}

// GetAgentJob implements Store.
func (s *SQL) GetAgentJob(id string) (*AgentJob, error) {
	var j AgentJob
	if err := s.getData("agentjob", id, &j); err != nil {
		return nil, err
	}
	return &j, nil
}

// FetchAndLockAgentJobs implements Store.
func (s *SQL) FetchAndLockAgentJobs(topic, workerID string, until, now time.Time, limit int) ([]*AgentJob, error) {
	var avail []*AgentJob
	err := s.scanKind("agentjob", "", func(data []byte) error {
		var j AgentJob
		if err := json.Unmarshal(data, &j); err != nil {
			return err
		}
		if j.Topic != topic || j.State != AgentPending {
			return nil
		}
		if j.LockedBy != "" && j.LockUntil.After(now) {
			return nil
		}
		avail = append(avail, &j)
		return nil
	})
	if err != nil {
		return nil, err
	}
	sort.Slice(avail, func(i, j int) bool { return avail[i].ID < avail[j].ID })
	if limit > 0 && len(avail) > limit {
		avail = avail[:limit]
	}
	out := make([]*AgentJob, 0, len(avail))
	for _, j := range avail {
		j.LockedBy = workerID
		j.LockUntil = until
		ok, err := s.claimPending("agentjob", j.ID, marshal(j), until, now)
		if err != nil {
			return nil, err
		}
		if ok {
			out = append(out, j)
		}
	}
	return out, nil
}

// ListAgentJobs implements Store.
func (s *SQL) ListAgentJobs(instanceID string) ([]*AgentJob, error) {
	var out []*AgentJob
	err := s.scanKind("agentjob", instanceID, func(data []byte) error {
		var j AgentJob
		if err := json.Unmarshal(data, &j); err != nil {
			return err
		}
		out = append(out, &j)
		return nil
	})
	if err != nil {
		return nil, err
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out, nil
}

// ---- subscriptions --------------------------------------------------------------------

// PutSubscription implements Store.
func (s *SQL) PutSubscription(sub *Subscription) error {
	return s.upsert(row{
		kind: "sub", id: sub.ID, tenant: sub.TenantID, k: sub.Name,
		instance: sub.InstanceID, data: marshal(sub),
	})
}

// DeleteSubscription implements Store.
func (s *SQL) DeleteSubscription(id string) error {
	_, err := s.delete("sub", id)
	return err
}

// ListSubscriptions implements Store.
func (s *SQL) ListSubscriptions(f SubscriptionFilter) ([]*Subscription, error) {
	var out []*Subscription
	err := s.scanKind("sub", f.InstanceID, func(data []byte) error {
		var sub Subscription
		if err := json.Unmarshal(data, &sub); err != nil {
			return err
		}
		if f.Kind != "" && sub.Kind != f.Kind {
			return nil
		}
		if f.Name != "" && sub.Name != f.Name {
			return nil
		}
		if f.CorrelationKey != "" && sub.CorrelationKey != f.CorrelationKey {
			return nil
		}
		if f.IsStart != nil && sub.IsStart != *f.IsStart {
			return nil
		}
		out = append(out, &sub)
		return nil
	})
	if err != nil {
		return nil, err
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out, nil
}

// ---- incidents -----------------------------------------------------------------------

// PutIncident implements Store.
func (s *SQL) PutIncident(i *Incident) error {
	state := "open"
	if i.Resolved {
		state = "resolved"
	}
	return s.upsert(row{kind: "inc", id: i.ID, instance: i.InstanceID, state: state, data: marshal(i)})
}

// GetIncident implements Store.
func (s *SQL) GetIncident(id string) (*Incident, error) {
	var i Incident
	if err := s.getData("inc", id, &i); err != nil {
		return nil, err
	}
	return &i, nil
}

// ListIncidents implements Store.
func (s *SQL) ListIncidents(f IncidentFilter) ([]*Incident, error) {
	var out []*Incident
	err := s.scanKind("inc", f.InstanceID, func(data []byte) error {
		var i Incident
		if err := json.Unmarshal(data, &i); err != nil {
			return err
		}
		if f.Resolved != nil && i.Resolved != *f.Resolved {
			return nil
		}
		out = append(out, &i)
		return nil
	})
	if err != nil {
		return nil, err
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	if f.Limit > 0 && len(out) > f.Limit {
		out = out[:f.Limit]
	}
	return out, nil
}

// ---- blobs ---------------------------------------------------------------------------

// PutBlob implements Store.
func (s *SQL) PutBlob(b *Blob) error {
	return s.upsert(row{kind: "blob", id: blobKey(b.Kind, b.Key), k: b.Kind, tenant: b.TenantID, data: marshal(b)})
}

// GetBlob implements Store.
func (s *SQL) GetBlob(kind, key string) (*Blob, error) {
	var b Blob
	if err := s.getData("blob", blobKey(kind, key), &b); err != nil {
		return nil, err
	}
	return &b, nil
}

// ListBlobs implements Store.
func (s *SQL) ListBlobs(kind string) ([]*Blob, error) {
	var out []*Blob
	err := s.scanKind("blob", "", func(data []byte) error {
		var b Blob
		if err := json.Unmarshal(data, &b); err != nil {
			return err
		}
		if kind == "" || b.Kind == kind {
			out = append(out, &b)
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Kind != out[j].Kind {
			return out[i].Kind < out[j].Kind
		}
		return out[i].Key < out[j].Key
	})
	return out, nil
}

// DeleteBlob implements Store.
func (s *SQL) DeleteBlob(kind, key string) error {
	_, err := s.delete("blob", blobKey(kind, key))
	return err
}

// ---- history --------------------------------------------------------------------------

// AppendHistory implements Store.
func (s *SQL) AppendHistory(ev *HistoryEvent) error {
	s.seqMu.Lock()
	if _, ok := s.histSeq[ev.InstanceID]; !ok {
		var max sql.NullInt64
		err := s.db.QueryRow(s.q(
			`SELECT MAX(seq) FROM fliable_records WHERE kind=? AND instance=?`),
			"hist", ev.InstanceID).Scan(&max)
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			s.seqMu.Unlock()
			return err
		}
		s.histSeq[ev.InstanceID] = max.Int64
	}
	if ev.Seq == 0 {
		s.histSeq[ev.InstanceID]++
		ev.Seq = s.histSeq[ev.InstanceID]
	} else if ev.Seq > s.histSeq[ev.InstanceID] {
		s.histSeq[ev.InstanceID] = ev.Seq
	}
	s.seqMu.Unlock()

	id := fmt.Sprintf("%s/%016x", ev.InstanceID, ev.Seq)
	return s.upsert(row{kind: "hist", id: id, instance: ev.InstanceID, seq: ev.Seq, data: marshal(ev)})
}

// ListHistory implements Store.
func (s *SQL) ListHistory(instanceID string, f HistoryFilter) ([]*HistoryEvent, error) {
	var out []*HistoryEvent
	err := s.scanKind("hist", instanceID, func(data []byte) error {
		var ev HistoryEvent
		if err := json.Unmarshal(data, &ev); err != nil {
			return err
		}
		if f.Type != "" && !strings.HasPrefix(ev.Type, f.Type) {
			return nil
		}
		if ev.Seq <= f.AfterSeq {
			return nil
		}
		out = append(out, &ev)
		return nil
	})
	if err != nil {
		return nil, err
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Seq < out[j].Seq })
	if f.Limit > 0 && len(out) > f.Limit {
		out = out[:f.Limit]
	}
	return out, nil
}

var _ Store = (*SQL)(nil)
