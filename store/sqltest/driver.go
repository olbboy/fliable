// Package sqltest provides an in-memory database/sql driver that speaks
// exactly the statement set store.SQL issues — upserts, point/kind/
// instance selects, the optimistic claim UPDATE, deletes and MAX(seq) —
// with faithful semantics. It exists so the SQL store (and engines on
// top of it) can be tested without a running database; swap the *sql.DB
// for a real Postgres handle and the same code paths run in production.
package sqltest

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"fmt"
	"io"
	"strings"
	"sync"
)

type frow struct {
	kind, id, tenant, k, instance, state string
	due, seq                             int64
	data                                 string
}

type fakeDB struct {
	mu   sync.Mutex
	rows map[string]*frow // kind\x00id
}

type fakeDriver struct {
	mu  sync.Mutex
	dbs map[string]*fakeDB
}

var theDriver = &fakeDriver{dbs: map[string]*fakeDB{}}

func init() { sql.Register("fliable-sqltest", theDriver) }

var seq int
var seqMu sync.Mutex

// OpenDB returns a database handle over a fresh, isolated in-memory
// table, ready for store.NewSQL.
func OpenDB() (*sql.DB, error) {
	seqMu.Lock()
	seq++
	name := fmt.Sprintf("db-%d", seq)
	seqMu.Unlock()
	return sql.Open("fliable-sqltest", name)
}

func (d *fakeDriver) Open(name string) (driver.Conn, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	db, ok := d.dbs[name]
	if !ok {
		db = &fakeDB{rows: map[string]*frow{}}
		d.dbs[name] = db
	}
	return &fakeConn{db: db}, nil
}

type fakeConn struct{ db *fakeDB }

func (c *fakeConn) Prepare(query string) (driver.Stmt, error) {
	return nil, fmt.Errorf("sqltest: prepare not supported (%s)", query)
}
func (c *fakeConn) Close() error              { return nil }
func (c *fakeConn) Begin() (driver.Tx, error) { return nil, fmt.Errorf("sqltest: no tx") }

func vals(args []driver.NamedValue) []driver.Value {
	out := make([]driver.Value, len(args))
	for i, a := range args {
		out[i] = a.Value
	}
	return out
}

func asString(v driver.Value) string {
	switch t := v.(type) {
	case string:
		return t
	case []byte:
		return string(t)
	}
	return fmt.Sprint(v)
}

func asInt(v driver.Value) int64 {
	if n, ok := v.(int64); ok {
		return n
	}
	return 0
}

// ExecContext implements driver.ExecerContext.
func (c *fakeConn) ExecContext(_ context.Context, query string, nargs []driver.NamedValue) (driver.Result, error) {
	args := vals(nargs)
	q := strings.TrimSpace(query)
	c.db.mu.Lock()
	defer c.db.mu.Unlock()
	switch {
	case strings.HasPrefix(q, "CREATE TABLE"), strings.HasPrefix(q, "CREATE INDEX"):
		return driver.RowsAffected(0), nil

	case strings.HasPrefix(q, "INSERT INTO fliable_records"):
		if len(args) != 9 {
			return nil, fmt.Errorf("sqltest: upsert wants 9 args, got %d", len(args))
		}
		r := &frow{
			kind: asString(args[0]), id: asString(args[1]), tenant: asString(args[2]),
			k: asString(args[3]), instance: asString(args[4]), state: asString(args[5]),
			due: asInt(args[6]), seq: asInt(args[7]), data: asString(args[8]),
		}
		c.db.rows[r.kind+"\x00"+r.id] = r
		return driver.RowsAffected(1), nil

	case strings.HasPrefix(q, "UPDATE fliable_records SET data=?, due=? WHERE kind=? AND id=? AND state=? AND due<=?"):
		data, due := asString(args[0]), asInt(args[1])
		kind, id, state, maxDue := asString(args[2]), asString(args[3]), asString(args[4]), asInt(args[5])
		r, ok := c.db.rows[kind+"\x00"+id]
		if !ok || r.state != state || r.due > maxDue {
			return driver.RowsAffected(0), nil
		}
		r.data, r.due = data, due
		return driver.RowsAffected(1), nil

	case strings.HasPrefix(q, "DELETE FROM fliable_records WHERE kind=? AND id=?"):
		key := asString(args[0]) + "\x00" + asString(args[1])
		if _, ok := c.db.rows[key]; !ok {
			return driver.RowsAffected(0), nil
		}
		delete(c.db.rows, key)
		return driver.RowsAffected(1), nil

	case strings.HasPrefix(q, "DELETE FROM fliable_records WHERE instance=?"):
		inst := asString(args[0])
		n := int64(0)
		for key, r := range c.db.rows {
			if r.instance == inst {
				delete(c.db.rows, key)
				n++
			}
		}
		return driver.RowsAffected(n), nil
	}
	return nil, fmt.Errorf("sqltest: unsupported exec: %s", q)
}

// QueryContext implements driver.QueryerContext.
func (c *fakeConn) QueryContext(_ context.Context, query string, nargs []driver.NamedValue) (driver.Rows, error) {
	args := vals(nargs)
	q := strings.TrimSpace(query)
	c.db.mu.Lock()
	defer c.db.mu.Unlock()
	switch {
	case strings.HasPrefix(q, "SELECT data FROM fliable_records WHERE kind=? AND id=?"):
		key := asString(args[0]) + "\x00" + asString(args[1])
		if r, ok := c.db.rows[key]; ok {
			return &fakeRows{cols: []string{"data"}, vals: [][]driver.Value{{r.data}}}, nil
		}
		return &fakeRows{cols: []string{"data"}}, nil

	case strings.HasPrefix(q, "SELECT data FROM fliable_records WHERE kind=? AND instance=?"):
		kind, inst := asString(args[0]), asString(args[1])
		out := &fakeRows{cols: []string{"data"}}
		for _, r := range c.db.rows {
			if r.kind == kind && r.instance == inst {
				out.vals = append(out.vals, []driver.Value{r.data})
			}
		}
		return out, nil

	case strings.HasPrefix(q, "SELECT data FROM fliable_records WHERE kind=?"):
		kind := asString(args[0])
		out := &fakeRows{cols: []string{"data"}}
		for _, r := range c.db.rows {
			if r.kind == kind {
				out.vals = append(out.vals, []driver.Value{r.data})
			}
		}
		return out, nil

	case strings.HasPrefix(q, "SELECT MAX(seq) FROM fliable_records WHERE kind=? AND instance=?"):
		kind, inst := asString(args[0]), asString(args[1])
		var max driver.Value // NULL when no rows
		for _, r := range c.db.rows {
			if r.kind == kind && r.instance == inst {
				if max == nil || r.seq > max.(int64) {
					max = r.seq
				}
			}
		}
		return &fakeRows{cols: []string{"max"}, vals: [][]driver.Value{{max}}}, nil
	}
	return nil, fmt.Errorf("sqltest: unsupported query: %s", q)
}

type fakeRows struct {
	cols []string
	vals [][]driver.Value
	i    int
}

func (r *fakeRows) Columns() []string { return r.cols }
func (r *fakeRows) Close() error      { return nil }
func (r *fakeRows) Next(dest []driver.Value) error {
	if r.i >= len(r.vals) {
		return io.EOF
	}
	copy(dest, r.vals[r.i])
	r.i++
	return nil
}
