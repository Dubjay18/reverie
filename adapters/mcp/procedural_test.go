package mcp

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/Dubjay18/reverie"
	"github.com/Dubjay18/reverie/internal/store"
)

func TestPackResource(t *testing.T) {
	s, e := start(t)
	init := s.rpc("initialize", map[string]any{"protocolVersion": "2025-06-18"})
	caps := init["result"].(map[string]any)["capabilities"].(map[string]any)
	if _, ok := caps["resources"]; !ok {
		t.Fatalf("resources capability missing: %v", caps)
	}
	list := s.rpc("resources/list", nil)["result"].(map[string]any)["resources"].([]any)
	if len(list) != 1 || list[0].(map[string]any)["uri"] != PackURI {
		t.Fatalf("resources/list: %v", list)
	}
	read := func() string {
		res := s.rpc("resources/read", map[string]any{"uri": PackURI})["result"].(map[string]any)
		c := res["contents"].([]any)[0].(map[string]any)
		if c["uri"] != PackURI || c["mimeType"] != "text/markdown" {
			t.Fatalf("contents: %v", c)
		}
		return c["text"].(string)
	}
	if got := read(); got != "No relevant memory yet." {
		t.Fatalf("empty pack: %q", got)
	}
	if _, err := e.Remember(context.Background(), sc, reverie.FactInput{Value: "prefers tabs", Source: reverie.SourceUser}); err != nil {
		t.Fatal(err)
	}
	if got := read(); !strings.Contains(got, "## Facts") || !strings.Contains(got, "prefers tabs") {
		t.Fatalf("pack: %q", got)
	}
	bad := s.rpc("resources/read", map[string]any{"uri": "memory://other"})
	if errObj, ok := bad["error"].(map[string]any); !ok || errObj["code"].(float64) != -32002 {
		t.Fatalf("unknown resource: %v", bad)
	}
}

func TestSkillOutcomeTool(t *testing.T) {
	s, e := start(t)
	now := time.Now().UTC()
	err := e.Store().For(sc).Tx(context.Background(), func(tx store.Tx) error {
		return tx.Skills().Save(context.Background(), store.Skill{ID: "deploy", Version: 1, Name: "deploy",
			Steps: []string{"build", "ship"}, Status: store.SkillCandidate, CreatedAt: now, UpdatedAt: now})
	})
	if err != nil {
		t.Fatal(err)
	}
	var out string
	for i := 0; i < 3; i++ {
		var isErr bool
		out, isErr = s.tool("memory_skill_outcome", map[string]any{"ref": "skill:deploy@v1", "success": true})
		if isErr {
			t.Fatal(out)
		}
	}
	if !strings.Contains(out, "promoted to an active skill") || !strings.Contains(out, "3/3") {
		t.Fatalf("third success: %q", out)
	}
	if out, isErr := s.tool("memory_skill_outcome", map[string]any{"ref": "skill:deploy@v1"}); !isErr || !strings.Contains(out, "success is required") {
		t.Fatalf("missing success: %q %v", out, isErr)
	}
	if out, isErr := s.tool("memory_skill_outcome", map[string]any{"ref": "skill:nope@v1", "success": true}); !isErr {
		t.Fatalf("unknown skill: %q", out)
	}
	// Recall now offers it as a skill, with its ref.
	pack, _ := s.tool("memory_recall", map[string]any{"task": "deploy"})
	if !strings.Contains(pack, "## Skills") || !strings.Contains(pack, "[skill:deploy@v1") {
		t.Fatalf("recall: %q", pack)
	}
}
