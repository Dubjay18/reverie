package main

import (
	"bufio"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"time"

	"reverie"
)

const usage = `usage: reverie <command> [flags]

commands:
  record   append an event to the log
  tail     print events, then follow new ones

run "reverie <command> -h" for flags.
`

func main() {
	if len(os.Args) < 2 {
		fmt.Fprint(os.Stderr, usage)
		os.Exit(2)
	}
	var err error
	switch os.Args[1] {
	case "record":
		err = record(os.Args[2:])
	case "tail":
		err = tail(os.Args[2:])
	default:
		fmt.Fprint(os.Stderr, usage)
		os.Exit(2)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}

func record(args []string) error {
	fs := flag.NewFlagSet("record", flag.ExitOnError)
	file := fs.String("file", "reverie.jsonl", "event log file")
	var s reverie.Scope
	fs.StringVar(&s.Tenant, "tenant", "default", "tenant")
	fs.StringVar(&s.Agent, "agent", "default", "agent")
	fs.StringVar(&s.Project, "project", "default", "project")
	fs.StringVar(&s.User, "user", "default", "user")
	task := fs.String("task", "", "task id")
	kind := fs.String("kind", string(reverie.Message), "event kind")
	text := fs.String("text", "", "event text (stored as payload {\"text\": ...})")
	fs.Parse(args)

	payload, err := json.Marshal(map[string]string{"text": *text})
	if err != nil {
		return err
	}
	e := reverie.NewEvent(s, *task, reverie.EventKind(*kind), payload)

	f, err := os.OpenFile(*file, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return err
	}
	defer f.Close()
	line, err := json.Marshal(e)
	if err != nil {
		return err
	}
	if _, err := f.Write(append(line, '\n')); err != nil {
		return err
	}
	fmt.Println(e.ID)
	return nil
}

func tail(args []string) error {
	fs := flag.NewFlagSet("tail", flag.ExitOnError)
	file := fs.String("file", "reverie.jsonl", "event log file")
	follow := fs.Bool("f", true, "keep waiting for new events")
	fs.Parse(args)

	f, err := os.Open(*file)
	if errors.Is(err, os.ErrNotExist) && *follow {
		// wait for the first record
		for errors.Is(err, os.ErrNotExist) {
			time.Sleep(500 * time.Millisecond)
			f, err = os.Open(*file)
		}
	}
	if err != nil {
		return err
	}
	defer f.Close()

	r := bufio.NewReader(f)
	var partial []byte
	for {
		chunk, err := r.ReadBytes('\n')
		partial = append(partial, chunk...)
		if err == nil {
			printEvent(partial)
			partial = nil
			continue
		}
		if !errors.Is(err, io.EOF) {
			return err
		}
		if !*follow {
			return nil
		}
		time.Sleep(300 * time.Millisecond)
	}
}

func printEvent(line []byte) {
	var e reverie.Event
	if err := json.Unmarshal(line, &e); err != nil {
		fmt.Fprintln(os.Stderr, "skipping bad line:", err)
		return
	}
	fmt.Printf("%s  %s  %-12s %s/%s/%s/%s  %s\n",
		e.ID, e.At.Local().Format("15:04:05"), e.Kind,
		e.Scope.Tenant, e.Scope.Agent, e.Scope.Project, e.Scope.User, e.Payload)
}
