package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"os/signal"
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
	fs.StringVar(&c.db, "db", envOr("REVERIE_DB", "reverie.db"), "database file (env REVERIE_DB)")
	fs.StringVar(&c.scope.Tenant, "tenant", "default", "tenant")
	fs.StringVar(&c.scope.Agent, "agent", "default", "agent")
	fs.StringVar(&c.scope.Project, "project", "default", "project")
	fs.StringVar(&c.scope.User, "user", "default", "user")
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

	eng, err := engine.Open(c.db)
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

	eng, err := engine.Open(c.db)
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
