package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"path/filepath"

	"github.com/Dubjay18/reverie"
	"github.com/Dubjay18/reverie/adapters/hook"
	"github.com/Dubjay18/reverie/adapters/mcp"
	"github.com/Dubjay18/reverie/engine"
	"github.com/Dubjay18/reverie/internal/embed"
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
	if wd, err := os.Getwd(); err == nil && wd != "" {
		return filepath.Clean(wd)
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

func hookCmd(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("hook", flag.ContinueOnError)
	db := fs.String("db", defaultDB(), "database file (env REVERIE_DB)")
	sc := agentScope(fs, "")
	if err := fs.Parse(args); err != nil { // an optional event name argument is accepted and ignored
		return err
	}
	eng, err := openEngine(*db)
	if err != nil {
		return err
	}
	defer eng.Close()
	d := hook.Defaults{Tenant: sc.Tenant, Agent: sc.Agent, User: sc.User, Project: sc.Project}
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
