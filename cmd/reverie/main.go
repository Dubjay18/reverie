package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"time"

	"github.com/Dubjay18/reverie"
	"github.com/Dubjay18/reverie/engine"
	"github.com/Dubjay18/reverie/internal/store"
	"github.com/oklog/ulid/v2"
)

const usage = `usage: reverie <command> [flags]

commands:
  record   append an event
  tail     print events, then follow new ones
  recall   print the memory pack for a task
  expand   print the raw events behind a ref from a pack
  hook     handle a Claude Code hook (reads the payload on stdin)
  mcp      serve memory tools over MCP on stdio
  embed    embed waiting items (needs REVERIE_EMBEDDINGS=openai and OPENAI_API_KEY)
  remember state a fact now (stored as your own words)
  facts    list what is believed (-asof to see the past, -contested for disputes)
  forget   forget a fact by its ref
  pin      protect a fact from dreams; unpin to release it
  journal  list recent changes to long-term memory, with run ids
  revert   undo a change by run id
  scrub    redact secrets from data stored before redaction existed

run "reverie <command> -h" for flags.
`

func main() {
	if len(os.Args) < 2 {
		fmt.Fprint(os.Stderr, usage)
		os.Exit(2)
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()

	var err error
	switch os.Args[1] {
	case "record":
		err = record(ctx, os.Args[2:])
	case "tail":
		err = tail(ctx, os.Args[2:])
	case "recall":
		err = recallCmd(ctx, os.Args[2:])
	case "expand":
		err = expandCmd(ctx, os.Args[2:])
	case "mcp":
		err = mcpCmd(ctx, os.Args[2:])
	case "embed":
		err = embedCmd(ctx, os.Args[2:])
	case "scrub":
		err = scrubCmd(ctx, os.Args[2:])
	case "remember", "facts", "forget", "pin", "unpin", "journal", "revert":
		err = factCmds(ctx, os.Args[1], os.Args[2:])
	case "hook":
		// A hook must never break the agent: report problems on stderr, exit 0.
		if herr := hookCmd(ctx, os.Args[2:]); herr != nil {
			fmt.Fprintln(os.Stderr, "reverie hook:", herr)
		}
	default:
		fmt.Fprint(os.Stderr, usage)
		os.Exit(2)
	}
	if err != nil && ctx.Err() == nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}

type common struct {
	db    string
	scope reverie.Scope
}

func (c *common) bind(fs *flag.FlagSet) {
	fs.StringVar(&c.db, "db", defaultDB(), "database file (env REVERIE_DB)")
	fs.StringVar(&c.scope.Tenant, "tenant", "default", "tenant")
	fs.StringVar(&c.scope.Agent, "agent", "default", "agent")
	fs.StringVar(&c.scope.Project, "project", "default", "project")
	fs.StringVar(&c.scope.User, "user", "default", "user")
}

// defaultDB is one per-user file, so every project and every hook process
// shares the same memory; a relative path would split it by working directory.
func defaultDB() string {
	if v := os.Getenv("REVERIE_DB"); v != "" {
		return v
	}
	dir := os.Getenv("XDG_DATA_HOME")
	if dir == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return "reverie.db"
		}
		dir = filepath.Join(home, ".local", "share")
	}
	return filepath.Join(dir, "reverie", "reverie.db")
}

func openEngine(path string, opts ...engine.Option) (*engine.Engine, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, err
	}
	// Create the file private before SQLite does: it holds everything agents
	// did, and SQLite would use the umask (usually world-readable).
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, err
	}
	f.Close()
	return engine.Open(path, opts...)
}

func envOr(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}

func record(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("record", flag.ExitOnError)
	var c common
	c.bind(fs)
	task := fs.String("task", "", "task id (required)")
	kind := fs.String("kind", string(reverie.Message), "event kind")
	origin := fs.String("origin", string(reverie.OriginUser), "event origin")
	text := fs.String("text", "", `shorthand for -payload '{"text": ...}'`)
	payload := fs.String("payload", "", "raw JSON payload")
	fs.Parse(args)

	p := json.RawMessage(*payload)
	if *payload == "" {
		var err error
		if p, err = json.Marshal(map[string]string{"text": *text}); err != nil {
			return err
		}
	}
	e := reverie.NewEvent(c.scope, *task, reverie.EventKind(*kind), reverie.Origin(*origin), p)

	eng, err := openEngine(c.db)
	if err != nil {
		return err
	}
	defer eng.Close()
	if err := eng.Record(ctx, e); err != nil {
		return err
	}
	fmt.Println(e.ID)
	return nil
}

func tail(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("tail", flag.ExitOnError)
	var c common
	c.bind(fs)
	task := fs.String("task", "", "only this task id")
	follow := fs.Bool("f", true, "keep waiting for new events")
	fs.Parse(args)

	eng, err := openEngine(c.db)
	if err != nil {
		return err
	}
	defer eng.Close()

	var after ulid.ULID
	for {
		var batch []reverie.Event
		err := eng.Store().For(c.scope).View(ctx, func(tx store.Tx) (err error) {
			batch, err = tx.Events().List(ctx, store.EventQuery{TaskID: *task, After: after})
			return err
		})
		if err != nil {
			return err
		}
		for _, e := range batch {
			printEvent(e)
			after = e.ID
		}
		if len(batch) > 0 {
			continue // there may be more than one page
		}
		if !*follow {
			return nil
		}
		select {
		case <-ctx.Done():
			return nil
		case <-time.After(300 * time.Millisecond):
		}
	}
}

func printEvent(e reverie.Event) {
	// %q keeps control sequences stored in an event from running in the terminal.
	fmt.Printf("%s  %s  %-12s %-14s %q  %q\n",
		e.ID, e.At.Local().Format("15:04:05"), e.Kind, e.Origin, e.TaskID, string(e.Payload))
}
