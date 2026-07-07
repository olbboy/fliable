// Command fliable is the Fliable workflow platform in a single binary:
// an embedded BPMN engine, DMN decision tables, durable storage and a
// REST API. No JVM, no database, no configuration files required.
//
//	fliable serve --addr :8080 --data ./fliable-data --deploy ./processes
//	fliable validate order.bpmn discount.dmn
//	fliable version
package main

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/olbboy/fliable/bpmn"
	"github.com/olbboy/fliable/dmn"
	"github.com/olbboy/fliable/engine"
	"github.com/olbboy/fliable/form"
	"github.com/olbboy/fliable/otel"
	"github.com/olbboy/fliable/rest"
	"github.com/olbboy/fliable/store"
	"github.com/olbboy/fliable/vault"
)

var version = "1.2.0"

func main() {
	if len(os.Args) < 2 {
		usage()
		os.Exit(2)
	}
	switch os.Args[1] {
	case "serve":
		if err := serve(os.Args[2:]); err != nil {
			fmt.Fprintln(os.Stderr, "error:", err)
			os.Exit(1)
		}
	case "validate":
		os.Exit(validate(os.Args[2:]))
	case "version":
		fmt.Printf("fliable %s\n", version)
	case "-h", "--help", "help":
		usage()
	default:
		fmt.Fprintf(os.Stderr, "unknown command %q\n\n", os.Args[1])
		usage()
		os.Exit(2)
	}
}

func usage() {
	fmt.Print(`fliable - compact, highly efficient workflow & BPM platform

Usage:
  fliable serve [flags]      start the engine + REST API
  fliable validate <files>   validate BPMN / DMN files
  fliable version            print version

Serve flags:
  --addr string     listen address (default ":8080")
  --data string     data directory for durable storage (default: in-memory)
  --fsync           fsync the journal on every write (safest, slower)
  --api-key string  require X-Api-Key header on API calls
  --deploy string   directory of .bpmn/.dmn files to deploy at startup
  --poll duration   job scheduler poll interval (default 100ms)
  --otel string     OTLP/HTTP endpoint for trace export (e.g. http://collector:4318)

Environment:
  FLIABLE_MASTER_KEY   enables the encrypted secrets vault (/v1/secrets)
`)
}

func serve(args []string) error {
	fs := flag.NewFlagSet("serve", flag.ExitOnError)
	addr := fs.String("addr", ":8080", "listen address")
	dataDir := fs.String("data", "", "data directory (empty = in-memory)")
	fsync := fs.Bool("fsync", false, "fsync journal writes")
	apiKey := fs.String("api-key", "", "require X-Api-Key header")
	deployDir := fs.String("deploy", "", "deploy all .bpmn/.dmn files from this directory at startup")
	poll := fs.Duration("poll", 100*time.Millisecond, "job poll interval")
	otelEndpoint := fs.String("otel", "", "OTLP/HTTP endpoint for trace export")
	if err := fs.Parse(args); err != nil {
		return err
	}

	log := slog.New(slog.NewTextHandler(os.Stderr, nil))
	start := time.Now()

	var st store.Store
	if *dataDir == "" {
		st = store.NewMemory()
		log.Info("storage: in-memory (use --data for durability)")
	} else {
		j, err := store.OpenJournal(*dataDir, store.JournalOptions{Fsync: *fsync})
		if err != nil {
			return err
		}
		st = j
		log.Info("storage: durable journal", "dir", *dataDir, "fsync", *fsync)
	}
	defer st.Close()

	decisions := dmn.NewRegistry()
	forms := form.NewRegistry(st)
	engOpts := []engine.Option{
		engine.WithLogger(log),
		engine.WithDecisionEvaluator(decisions),
		engine.WithJobPollInterval(*poll),
		engine.WithFormValidator(forms),
	}
	restOpts := []rest.Option{rest.WithLogger(log), rest.WithDecisions(decisions), rest.WithForms(forms)}

	if masterKey := os.Getenv("FLIABLE_MASTER_KEY"); masterKey != "" {
		v, err := vault.New(st, []byte(masterKey))
		if err != nil {
			return err
		}
		engOpts = append(engOpts, engine.WithSecrets(v))
		restOpts = append(restOpts, rest.WithVault(v))
		log.Info("secrets vault enabled")
	}

	eng := engine.New(st, engOpts...)

	if *otelEndpoint != "" {
		exp := otel.New(otel.Config{Endpoint: *otelEndpoint, Logger: log})
		exp.Attach(eng)
		defer exp.Close()
		log.Info("otel trace export enabled", "endpoint", *otelEndpoint)
	}

	if *deployDir != "" {
		if err := deployAll(eng, decisions, *deployDir, log); err != nil {
			return err
		}
	}

	eng.Start()
	defer eng.Stop()

	if *apiKey != "" {
		restOpts = append(restOpts, rest.WithAPIKey(*apiKey))
	}
	srv := &http.Server{Addr: *addr, Handler: rest.New(eng, restOpts...)}

	errCh := make(chan error, 1)
	go func() { errCh <- srv.ListenAndServe() }()
	log.Info("fliable ready", "addr", *addr, "version", version, "startup", time.Since(start).Round(time.Millisecond))

	sig := make(chan os.Signal, 1)
	signal.Notify(sig, os.Interrupt, syscall.SIGTERM)
	select {
	case err := <-errCh:
		return err
	case <-sig:
		log.Info("shutting down")
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		return srv.Shutdown(ctx)
	}
}

func deployAll(eng *engine.Engine, decisions *dmn.Registry, dir string, log *slog.Logger) error {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return fmt.Errorf("deploy dir: %w", err)
	}
	for _, ent := range entries {
		if ent.IsDir() {
			continue
		}
		path := filepath.Join(dir, ent.Name())
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		switch strings.ToLower(filepath.Ext(ent.Name())) {
		case ".bpmn", ".bpmn20.xml":
			def, err := eng.Deploy(data, "")
			if err != nil {
				return fmt.Errorf("%s: %w", path, err)
			}
			log.Info("deployed process", "file", ent.Name(), "key", def.Key, "version", def.Version)
		case ".dmn":
			ds, err := decisions.RegisterXML(data)
			if err != nil {
				return fmt.Errorf("%s: %w", path, err)
			}
			for _, d := range ds {
				log.Info("deployed decision", "file", ent.Name(), "id", d.ID)
			}
		}
	}
	return nil
}

func validate(files []string) int {
	if len(files) == 0 {
		fmt.Fprintln(os.Stderr, "usage: fliable validate <file.bpmn|file.dmn> ...")
		return 2
	}
	failed := 0
	for _, f := range files {
		data, err := os.ReadFile(f)
		if err != nil {
			fmt.Printf("✗ %s: %v\n", f, err)
			failed++
			continue
		}
		if strings.EqualFold(filepath.Ext(f), ".dmn") {
			ds, err := dmn.Parse(data)
			if err != nil {
				fmt.Printf("✗ %s: %v\n", f, err)
				failed++
				continue
			}
			fmt.Printf("✓ %s: %d decision(s)\n", f, len(ds))
			continue
		}
		defs, err := bpmn.Parse(data)
		if err != nil {
			fmt.Printf("✗ %s: %v\n", f, err)
			failed++
			continue
		}
		for _, p := range defs.Processes {
			fmt.Printf("✓ %s: process %q (%d elements, %d flows)\n", f, p.ID, len(p.Elements), len(p.Flows))
		}
	}
	if failed > 0 {
		return 1
	}
	return 0
}
