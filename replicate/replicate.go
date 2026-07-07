// Package replicate ships a leader journal's entries to a warm-standby
// follower over HTTP — Fliable's built-in disaster-recovery path, zero
// dependencies. The leader pushes an initial snapshot and then streams
// batched journal lines; the follower applies them to its own journal
// store. Promotion is: stop the receiver, start an engine on the
// follower's store (~2 ms, same as any Fliable boot).
//
//	leader:   j, _ := store.OpenJournal(dirA, opts)
//	          snd := replicate.NewSender(j, "https://standby:9090", secret)
//	          defer snd.Close()
//	follower: j, _ := store.OpenJournal(dirB, opts)
//	          http.ListenAndServe(":9090", replicate.NewReceiver(j, secret))
//
// Replication is asynchronous: RPO is the in-flight batch (bounded by
// FlushInterval), RTO is engine start time. Any error or buffer overflow
// triggers a full snapshot resync, so the follower converges no matter
// what was lost in between.
package replicate

import (
	"bytes"
	"crypto/subtle"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"sync"
	"time"

	"github.com/olbboy/fliable/store"
)

const secretHeader = "X-Replication-Secret"

// SenderOptions tunes the leader side.
type SenderOptions struct {
	// FlushInterval bounds how long a journal line waits before shipping
	// (default 200ms). BatchSize ships earlier when reached (default 256).
	FlushInterval time.Duration
	BatchSize     int
	// Buffer is the in-flight line capacity; overflow forces a snapshot
	// resync instead of blocking the engine (default 65536).
	Buffer int
	Client *http.Client
	Logger *slog.Logger
}

// Sender streams a journal to one follower.
type Sender struct {
	j      *store.Journal
	target string
	secret string
	opts   SenderOptions

	ch      chan []byte
	overrun chan struct{} // signaled on buffer overflow -> resync
	stop    chan struct{}
	stopped chan struct{}
	once    sync.Once
}

// NewSender starts replicating j to the receiver at target. The first
// sync ships a full snapshot; afterwards journal lines stream in order.
func NewSender(j *store.Journal, target, secret string, opts ...SenderOptions) *Sender {
	var o SenderOptions
	if len(opts) > 0 {
		o = opts[0]
	}
	if o.FlushInterval <= 0 {
		o.FlushInterval = 200 * time.Millisecond
	}
	if o.BatchSize <= 0 {
		o.BatchSize = 256
	}
	if o.Buffer <= 0 {
		o.Buffer = 65536
	}
	if o.Client == nil {
		o.Client = &http.Client{Timeout: 30 * time.Second}
	}
	if o.Logger == nil {
		o.Logger = slog.Default()
	}
	s := &Sender{
		j: j, target: target, secret: secret, opts: o,
		ch:      make(chan []byte, o.Buffer),
		overrun: make(chan struct{}, 1),
		stop:    make(chan struct{}),
		stopped: make(chan struct{}),
	}
	// Register the tap before the first snapshot so no line can fall
	// between snapshot and stream; overlap is deduplicated follower-side.
	j.OnAppend(func(line []byte) {
		select {
		case s.ch <- line:
		default:
			// Never block the engine: drop and schedule a resync.
			select {
			case s.overrun <- struct{}{}:
			default:
			}
		}
	})
	go s.loop()
	return s
}

// Close stops the sender after a best-effort final flush.
func (s *Sender) Close() {
	s.once.Do(func() { close(s.stop) })
	<-s.stopped
}

func (s *Sender) loop() {
	defer close(s.stopped)
	needSync := true
	t := time.NewTicker(s.opts.FlushInterval)
	defer t.Stop()
	var batch [][]byte

	flush := func() {
		if needSync {
			if err := s.snapshotSync(); err != nil {
				s.opts.Logger.Warn("replicate: snapshot sync failed", "error", err)
				return
			}
			needSync = false
			batch = nil // snapshot already covers everything buffered
			// Drain whatever queued during the sync into the next batch.
			for {
				select {
				case l := <-s.ch:
					batch = append(batch, l)
				default:
					return
				}
			}
		}
		if len(batch) == 0 {
			return
		}
		if err := s.send(batch); err != nil {
			s.opts.Logger.Warn("replicate: append failed, will resync", "error", err)
			needSync = true
			return
		}
		batch = nil
	}

	for {
		select {
		case <-s.stop:
			for {
				select {
				case l := <-s.ch:
					batch = append(batch, l)
				default:
					flush()
					return
				}
			}
		case <-s.overrun:
			needSync = true
		case l := <-s.ch:
			batch = append(batch, l)
			if len(batch) >= s.opts.BatchSize {
				flush()
			}
		case <-t.C:
			flush()
		}
	}
}

// snapshotSync ships the full current state.
func (s *Sender) snapshotSync() error {
	snap, err := s.j.StateSnapshot()
	if err != nil {
		return err
	}
	return s.post("/snapshot", snap)
}

func (s *Sender) send(batch [][]byte) error {
	var body bytes.Buffer
	for _, l := range batch {
		body.Write(l)
		body.WriteByte('\n')
	}
	return s.post("/append", body.Bytes())
}

func (s *Sender) post(path string, body []byte) error {
	req, err := http.NewRequest(http.MethodPost, s.target+path, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/x-ndjson")
	if s.secret != "" {
		req.Header.Set(secretHeader, s.secret)
	}
	resp, err := s.opts.Client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		msg, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return errors.New("replicate: follower returned " + resp.Status + ": " + string(msg))
	}
	return nil
}

// NewReceiver builds the follower-side handler: POST /snapshot resets
// state, POST /append applies streamed journal lines. Mount it on a
// private listener — it is the replication plane, not the public API.
func NewReceiver(j *store.Journal, secret string) http.Handler {
	mux := http.NewServeMux()
	authed := func(h http.HandlerFunc) http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) {
			if secret != "" {
				got := r.Header.Get(secretHeader)
				if subtle.ConstantTimeCompare([]byte(got), []byte(secret)) != 1 {
					http.Error(w, "bad replication secret", http.StatusUnauthorized)
					return
				}
			}
			h(w, r)
		}
	}
	mux.HandleFunc("POST /snapshot", authed(func(w http.ResponseWriter, r *http.Request) {
		defer r.Body.Close()
		data, err := io.ReadAll(io.LimitReader(r.Body, 1<<30))
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		if err := j.ResetFromSnapshot(data); err != nil {
			http.Error(w, err.Error(), http.StatusUnprocessableEntity)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	}))
	mux.HandleFunc("POST /append", authed(func(w http.ResponseWriter, r *http.Request) {
		defer r.Body.Close()
		sc := newLineScanner(io.LimitReader(r.Body, 1<<30))
		for sc.Scan() {
			line := sc.Bytes()
			if len(line) == 0 {
				continue
			}
			if err := j.ApplyReplicated(line); err != nil {
				http.Error(w, err.Error(), http.StatusUnprocessableEntity)
				return
			}
		}
		if err := sc.Err(); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	}))
	return mux
}
