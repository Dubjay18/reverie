package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"strings"

	"github.com/Dubjay18/reverie/engine"
	"github.com/Dubjay18/reverie/internal/skills"
	"github.com/Dubjay18/reverie/internal/store"
)

// recombineOpts turns on the Recombine dream phase when REVERIE_RECOMBINE=1.
func recombineOpts() []engine.Option {
	if os.Getenv("REVERIE_RECOMBINE") == "1" {
		return []engine.Option{engine.WithRecombine(true)}
	}
	return nil
}

// skillCmd: reverie skill list | outcome <ref> success|failure | rollback <id>
func skillCmd(ctx context.Context, args []string) error {
	sub := "list"
	if len(args) > 0 && !strings.HasPrefix(args[0], "-") {
		sub, args = args[0], args[1:]
	}
	fs := flag.NewFlagSet("skill "+sub, flag.ExitOnError)
	db := fs.String("db", defaultDB(), "database file (env REVERIE_DB)")
	sc := agentScope(fs, cwdName())
	status := fs.String("status", "", "list only candidate, active or deprecated skills")
	fs.Parse(args)
	eng, err := openEngine(*db)
	if err != nil {
		return err
	}
	defer eng.Close()
	switch sub {
	case "list":
		list, err := eng.Skills(ctx, *sc, store.SkillQuery{Status: store.SkillStatus(*status)})
		if err != nil {
			return err
		}
		for _, sk := range list {
			fmt.Printf("%s  %-10s  %d/%d ok, rate %.2f  %s: %s\n", skills.Ref(sk), sk.Status, sk.Successes, sk.Uses, skills.Rate(sk), sk.Trigger, strings.Join(sk.Steps, " -> "))
		}
	case "outcome":
		if fs.NArg() != 2 || (fs.Arg(1) != "success" && fs.Arg(1) != "failure") {
			return fmt.Errorf("usage: reverie skill outcome [flags] <skill:id@vN> success|failure")
		}
		sk, tr, err := eng.SkillOutcome(ctx, *sc, fs.Arg(0), fs.Arg(1) == "success")
		if err != nil {
			return err
		}
		fmt.Printf("%s  %s  %d/%d ok, rate %.2f", skills.Ref(sk), sk.Status, sk.Successes, sk.Uses, skills.Rate(sk))
		if tr != skills.Kept {
			fmt.Printf("  (%s)", tr)
		}
		fmt.Println()
	case "rollback":
		if fs.NArg() != 1 {
			return fmt.Errorf("usage: reverie skill rollback [flags] <id>")
		}
		restored, run, err := eng.SkillRollback(ctx, *sc, fs.Arg(0))
		if err != nil {
			return err
		}
		if restored.ID != "" {
			fmt.Println("active again:", skills.Ref(restored))
		} else {
			fmt.Println("withdrawn; no earlier proven version")
		}
		fmt.Println("undo with: reverie revert", run)
	default:
		return fmt.Errorf("usage: reverie skill list|outcome|rollback")
	}
	return nil
}

// insightCmd: reverie insight list | confirm <id> | reject <id>
func insightCmd(ctx context.Context, args []string) error {
	sub := "list"
	if len(args) > 0 && !strings.HasPrefix(args[0], "-") {
		sub, args = args[0], args[1:]
	}
	fs := flag.NewFlagSet("insight "+sub, flag.ExitOnError)
	db := fs.String("db", defaultDB(), "database file (env REVERIE_DB)")
	sc := agentScope(fs, cwdName())
	status := fs.String("status", "", "list only speculative, confirmed or rejected insights")
	fs.Parse(args)
	eng, err := openEngine(*db)
	if err != nil {
		return err
	}
	defer eng.Close()
	switch sub {
	case "list":
		list, err := eng.Insights(ctx, *sc, store.InsightQuery{Status: store.InsightStatus(*status)})
		if err != nil {
			return err
		}
		for _, in := range list {
			fmt.Printf("insight:%s  %-11s  %.2f  %q  (from %s)\n", in.ID, in.Status, in.Confidence, in.Body, strings.Join(in.EpisodeIDs, " + "))
		}
	case "confirm", "reject":
		if fs.NArg() != 1 {
			return fmt.Errorf("usage: reverie insight %s [flags] <id>", sub)
		}
		id := strings.TrimPrefix(fs.Arg(0), "insight:")
		var run string
		if sub == "confirm" {
			run, err = eng.ConfirmInsight(ctx, *sc, id)
		} else {
			run, err = eng.RejectInsight(ctx, *sc, id)
		}
		if err != nil {
			return err
		}
		fmt.Println("ok; undo with: reverie revert", run)
	default:
		return fmt.Errorf("usage: reverie insight list|confirm|reject")
	}
	return nil
}
