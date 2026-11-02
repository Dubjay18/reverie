package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"time"

	"net/http"

	"github.com/Dubjay18/reverie"
	"github.com/Dubjay18/reverie/adapters/hook"
	revhttp "github.com/Dubjay18/reverie/adapters/http"
	"github.com/Dubjay18/reverie/adapters/mcp"
	"github.com/Dubjay18/reverie/engine"
	"github.com/Dubjay18/reverie/internal/dream"
	"github.com/Dubjay18/reverie/internal/embed"
	"github.com/Dubjay18/reverie/internal/llm"
	"github.com/Dubjay18/reverie/internal/store"
)

// agentScope is the identity hooks and the MCP server share, so what a hook
// records the MCP tools can recall. Override with flags or REVERIE_* env.
func agentScope(fs *flag.FlagSet, project string) *reverie.Scope {
	user := os.Getenv("USER")
	if user == "" {
		user = "default"
	}
	sc := &reverie.Scope{}
	fs.StringVar(&sc.Tenant, "tenant", envOr("REVERIE_TENANT", "local"), "tenant (env REVERIE_TENANT)")
	fs.StringVar(&sc.Agent, "agent", envOr("REVERIE_AGENT", "claude-code"), "agent (env REVERIE_AGENT)")
	fs.StringVar(&sc.Project, "project", envOr("REVERIE_PROJECT", project), "project (env REVERIE_PROJECT; default: absolute current directory)")
	fs.StringVar(&sc.User, "user", envOr("REVERIE_USER", user), "user (env REVERIE_USER)")
	return sc
}

// cwdName is the default project: the absolute working directory, matching
// what the hook derives from the payload, so both see the same memory.
func cwdName() string {
	if wd, err := os.Getwd(); err == nil {
		return hook.Project(wd)
	}
	return "unknown"
}

// embedOpts enables vector search only when asked for, because it needs a
// network call and an API key. Hooks never use it: they must stay fast.
func embedOpts() []engine.Option {
	if os.Getenv("REVERIE_EMBEDDINGS") == "openai" && os.Getenv("OPENAI_API_KEY") != "" {
		return []engine.Option{engine.WithEmbedder(&embed.OpenAI{APIKey: os.Getenv("OPENAI_API_KEY")})}
	}
	return nil
}

func llmOpts() []engine.Option {
	if key := os.Getenv("OPENAI_API_KEY"); key != "" {
		return []engine.Option{engine.WithLLM(&llm.OpenAI{
			APIKey:  key,
			Model:   os.Getenv("REVERIE_LLM_MODEL"),
			BaseURL: os.Getenv("REVERIE_LLM_BASE_URL"),
		})}
	}
	return nil
}

func hookCmd(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("hook", flag.ContinueOnError)
	db := fs.String("db", defaultDB(), "database file (env REVERIE_DB)")
	sc := agentScope(fs, "")
	if err := fs.Parse(args); err != nil { // an optional event name argument is accepted and ignored
		return err
	}
	eng, err := openEngine(*db, llmOpts()...)
	if err != nil {
		return err
	}
	defer eng.Close()
	d := hook.Defaults{Tenant: sc.Tenant, Agent: sc.Agent, User: sc.User, Project: sc.Project}
	if fs.NArg() > 0 && fs.Arg(0) == "stop" {
		proj := sc.Project
		if proj == "" {
			proj = cwdName()
		}
		s := reverie.Scope{Tenant: sc.Tenant, Agent: sc.Agent, Project: proj, User: sc.User}
		return eng.TriggerStopDream(ctx, s)
	}
	return hook.Handle(ctx, eng, d, os.Stdin, os.Stdout)
}

func mcpCmd(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("mcp", flag.ExitOnError)
	db := fs.String("db", defaultDB(), "database file (env REVERIE_DB)")
	sc := agentScope(fs, cwdName())
	fs.Parse(args)
	eng, err := openEngine(*db, embedOpts()...)
	if err != nil {
		return err
	}
	defer eng.Close()
	srv := &mcp.Server{Mem: eng, Scope: *sc, Version: "0.1"}
	return srv.Serve(ctx, os.Stdin, os.Stdout)
}

func serveCmd(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("serve", flag.ExitOnError)
	db := fs.String("db", defaultDB(), "database file (env REVERIE_DB)")
	addr := fs.String("addr", envOr("REVERIE_ADDR", "127.0.0.1:8080"), "listen address (env REVERIE_ADDR)")
	sc := agentScope(fs, cwdName())
	fs.Parse(args)
	opts := append(embedOpts(), llmOpts()...)
	eng, err := openEngine(*db, opts...)
	if err != nil {
		return err
	}
	defer eng.Close()
	h := &revhttp.Server{Mem: eng, DefaultScope: *sc}
	srv := &http.Server{Addr: *addr, Handler: h}
	go func() {
		<-ctx.Done()
		_ = srv.Shutdown(context.Background())
	}()
	err = srv.ListenAndServe()
	if err == http.ErrServerClosed {
		return nil
	}
	return err
}

func recallCmd(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("recall", flag.ExitOnError)
	db := fs.String("db", defaultDB(), "database file (env REVERIE_DB)")
	sc := agentScope(fs, cwdName())
	task := fs.String("task", "", "what you are about to do")
	budget := fs.Int("budget", 1500, "token budget")
	fs.Parse(args)
	eng, err := openEngine(*db, embedOpts()...)
	if err != nil {
		return err
	}
	defer eng.Close()
	pack, err := eng.Recall(ctx, *sc, *task, *budget)
	if err != nil {
		return err
	}
	fmt.Print(pack.Render())
	fmt.Fprintf(os.Stderr, "%d/%d tokens, %d dropped\n", pack.Tokens, pack.Budget, pack.Dropped)
	return nil
}

func expandCmd(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("expand", flag.ExitOnError)
	db := fs.String("db", defaultDB(), "database file (env REVERIE_DB)")
	sc := agentScope(fs, cwdName())
	fs.Parse(args)
	if fs.NArg() != 1 {
		return fmt.Errorf("usage: reverie expand [flags] <ref>")
	}
	ref, err := reverie.ParseRef(fs.Arg(0))
	if err != nil {
		return err
	}
	eng, err := openEngine(*db)
	if err != nil {
		return err
	}
	defer eng.Close()
	events, err := eng.Expand(ctx, *sc, ref)
	if err != nil {
		return err
	}
	for _, e := range events {
		printEvent(e)
	}
	if len(events) == engine.MaxExpand {
		fmt.Fprintln(os.Stderr, "result hit the cap; narrow the ref to a range")
	}
	return nil
}

func embedCmd(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("embed", flag.ExitOnError)
	db := fs.String("db", defaultDB(), "database file (env REVERIE_DB)")
	fs.Parse(args)
	opts := embedOpts()
	if opts == nil {
		return fmt.Errorf("set REVERIE_EMBEDDINGS=openai and OPENAI_API_KEY")
	}
	eng, err := openEngine(*db, opts...)
	if err != nil {
		return err
	}
	defer eng.Close()
	total := 0
	for {
		n, err := eng.EmbedPending(ctx, 64)
		total += n
		if err != nil {
			return err
		}
		if n == 0 {
			break
		}
	}
	fmt.Println("embedded", total)
	return nil
}

func factCmds(ctx context.Context, name string, args []string) error {
	fs := flag.NewFlagSet(name, flag.ExitOnError)
	db := fs.String("db", defaultDB(), "database file (env REVERIE_DB)")
	sc := agentScope(fs, cwdName())
	var (
		text, entity, relation, value, asof *string
		contested                           *bool
		limit                               *int
	)
	switch name {
	case "remember":
		text = fs.String("text", "", "what to remember, in words")
		entity = fs.String("entity", "", "who or what it is about (default: user)")
		relation = fs.String("relation", "", "relation, e.g. prefers, uses, lives_in (default: note)")
		value = fs.String("value", "", "the value of the relation")
	case "facts":
		asof = fs.String("asof", "", "show what was true at this time (RFC 3339) instead of now")
		contested = fs.Bool("contested", false, "only facts in dispute")
		limit = fs.Int("n", 50, "how many to show")
	case "journal":
		limit = fs.Int("n", 20, "how many entries to show")
	}
	fs.Parse(args)

	eng, err := openEngine(*db)
	if err != nil {
		return err
	}
	defer eng.Close()

	switch name {
	case "remember":
		in := reverie.FactInput{Entity: *entity, Relation: *relation, Value: *value, Source: reverie.SourceUser}
		if in.Value == "" || in.Relation == "" {
			in.Entity, in.Relation, in.Value = "", "", *text
		}
		f, err := eng.Remember(ctx, *sc, in)
		if err != nil {
			return err
		}
		fmt.Printf("fact:%s  %s %s: %s  (%s)\n", f.ID, f.Entity, f.Relation, f.Value, f.Status)
	case "forget", "pin", "unpin":
		if fs.NArg() != 1 {
			return fmt.Errorf("usage: reverie %s [flags] <fact:id>", name)
		}
		ref, err := reverie.ParseRef(fs.Arg(0))
		if err != nil || ref.FactID == "" {
			return fmt.Errorf("want a fact ref like fact:01J...")
		}
		switch name {
		case "forget":
			var run string
			run, err = eng.Forget(ctx, *sc, ref.FactID, reverie.SourceUser)
			if err == nil {
				fmt.Println("undo with: reverie revert", run)
			}
		default:
			err = eng.Pin(ctx, *sc, ref.FactID, name == "pin")
		}
		if err != nil {
			return err
		}
		fmt.Println("ok")
	case "revert":
		if fs.NArg() != 1 {
			return fmt.Errorf("usage: reverie revert [flags] <run-id>   (see `reverie journal`)")
		}
		n, err := eng.Revert(ctx, *sc, fs.Arg(0))
		if err != nil {
			return err
		}
		fmt.Println("reverted", n, "change(s)")
	case "facts":
		var at time.Time
		if *asof != "" {
			if at, err = time.Parse(time.RFC3339, *asof); err != nil {
				return fmt.Errorf("-asof: %w", err)
			}
		}
		fs, err := eng.Facts(ctx, *sc, store.FactQuery{OnlyContested: *contested, Limit: *limit}, at)
		if err != nil {
			return err
		}
		for _, f := range fs {
			flags := ""
			if f.Pinned {
				flags += " pinned"
			}
			if f.Status != reverie.FactActive {
				flags += " " + string(f.Status)
			}
			fmt.Printf("fact:%s  %s %s: %s  [%s, %.2f%s]\n", f.ID, f.Entity, f.Relation, f.Value, f.Source, f.Confidence, flags)
		}
	case "journal":
		es, err := eng.Journal(ctx, *sc, *limit)
		if err != nil {
			return err
		}
		for _, e := range es {
			mark := ""
			if e.Reverted {
				mark = " (reverted)"
			}
			fmt.Printf("%s  run=%s  %s %s:%s%s  %s\n", e.At.Local().Format("2006-01-02 15:04:05"), e.RunID, e.Op, e.TargetKind, e.TargetID, mark, e.Rationale)
		}
	}
	return nil
}

func scrubCmd(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("scrub", flag.ExitOnError)
	db := fs.String("db", defaultDB(), "database file (env REVERIE_DB)")
	fs.Parse(args)
	eng, err := openEngine(*db)
	if err != nil {
		return err
	}
	defer eng.Close()
	rep, err := eng.Scrub(ctx)
	if err != nil {
		return err
	}
	fmt.Printf("scrubbed %d event(s) and %d episode(s)\n", rep.Events, rep.Episodes)
	return nil
}

func dreamCmd(ctx context.Context, args []string) error {
	sub := ""
	if len(args) > 0 && (args[0] == "show" || args[0] == "revert") {
		sub = args[0]
		args = args[1:]
	}
	fs := flag.NewFlagSet("dream", flag.ExitOnError)
	db := fs.String("db", defaultDB(), "database file (env REVERIE_DB)")
	sc := agentScope(fs, cwdName())
	budget := fs.Int("budget", dream.DefaultDreamBudget, "token budget for the dream run")
	trigger := fs.String("trigger", dream.TriggerManual, "dream trigger name")
	fs.Parse(args)
	pos := fs.Args()
	if sub == "" && len(pos) > 0 && (pos[0] == "show" || pos[0] == "revert") {
		sub = pos[0]
		pos = pos[1:]
	}

	eng, err := openEngine(*db, append(llmOpts(), recombineOpts()...)...)
	if err != nil {
		return err
	}
	defer eng.Close()

	switch sub {
	case "show":
		if len(pos) != 1 {
			return fmt.Errorf("usage: reverie dream show [flags] <run-id>")
		}
		run, entries, err := eng.DreamShow(ctx, *sc, pos[0])
		if err != nil {
			return err
		}
		fmt.Print(dream.FormatDiff(run, entries))
		return nil
	case "revert":
		if len(pos) != 1 {
			return fmt.Errorf("usage: reverie dream revert [flags] <run-id>")
		}
		n, err := eng.Revert(ctx, *sc, pos[0])
		if err != nil {
			return err
		}
		fmt.Printf("reverted %d change(s) from %s\n", n, pos[0])
		return nil
	default:
		rep, err := eng.Dream(ctx, *sc, *trigger, *budget)
		if err != nil {
			return err
		}
		fmt.Println(dream.FormatSummary(rep))
		return nil
	}
}
