package guard

import (
	"strings"
	"testing"
)

func TestRedact(t *testing.T) {
	cases := map[string]string{
		"aws":       "key AKIAIOSFODNN7EXAMPLE end",
		"github":    "token ghp_" + strings.Repeat("a1B2", 10),
		"gh-pat":    "github_pat_" + strings.Repeat("x", 30),
		"slack":     "xoxb-1234567890-abcdefghij",
		"openai":    "sk-" + strings.Repeat("Ab3_", 8),
		"anthropic": "sk-ant-api03-" + strings.Repeat("Zz9", 12),
		"google":    "AIza" + strings.Repeat("a", 35),
		// Built at runtime so the source holds no token-shaped literal for scanners to flag.
		"jwt":      "eyJhbGci" + "OiJIUzI1NiJ9." + "eyJzdWIi" + "OiIxMjM0NTY3ODkwIn0." + strings.Repeat("abcd", 4),
		"bearer":   "Authorization: Bearer " + strings.Repeat("abcdefghij", 4),
		"pem":      "-----BEGIN RSA PRIVATE KEY-----\nMIIEow\nabc\n-----END RSA PRIVATE KEY-----",
		"pem-cut":  "-----BEGIN PRIVATE KEY-----\nMIIEvQIBADANBg",
		"env":      "DB_PASSWORD=hunter2hunter2",
		"json":     `{"api_key": "abcd1234efgh"}`,
		"json-esc": `{"output":"export TOKEN=\"s3cr3tvalue\" ok"}`,
	}
	secrets := map[string]string{
		"aws": "AKIAIOSFODNN7EXAMPLE", "github": "ghp_", "gh-pat": "github_pat_", "slack": "xoxb-", "openai": "sk-Ab3_",
		"anthropic": "sk-ant-api03-", "google": "AIza", "jwt": "eyJhbGci", "bearer": "abcdefghijklmnopqrstuvwxyz",
		"pem": "MIIEow", "pem-cut": "MIIEvQ", "env": "hunter2", "json": "abcd1234efgh", "json-esc": "s3cr3tvalue",
	}
	for name, in := range cases {
		out := Redact(in)
		if strings.Contains(out, secrets[name]) || !strings.Contains(out, Redacted) {
			t.Errorf("%s: secret survived: %q", name, out)
		}
	}
}

func TestRedactLeavesOrdinaryTextAlone(t *testing.T) {
	for _, s := range []string{
		"deploy failed with ERR_42 near the gateway",
		"the token bucket algorithm limits rate",
		"password reset email template",
		"sk-short",
		`{"file_path":"/home/u/gw.go","type":"update"}`,
	} {
		if got := Redact(s); got != s {
			t.Errorf("changed %q -> %q", s, got)
		}
	}
}

func TestRedactKeepsJSONValid(t *testing.T) {
	in := `{"output":"export API_KEY=\"abcd1234efgh\"\n-----BEGIN PRIVATE KEY-----\nMII\n-----END PRIVATE KEY-----\nok"}`
	out := Redact(in)
	if !strings.HasPrefix(out, `{"output":"`) || !strings.HasSuffix(out, `"}`) || strings.Count(out, `"`) != strings.Count(in, `"`)-2 && strings.Count(out, `"`) != strings.Count(in, `"`) {
		t.Fatalf("structure damaged: %q", out)
	}
}

func TestSensitivePath(t *testing.T) {
	for p, want := range map[string]bool{
		"/home/u/app/.env": true, "/x/.env.production": true, "/x/server.pem": true, "/home/u/.ssh/id_rsa": true,
		"/x/credentials": true, "/x/.npmrc": true, "/x/secrets.yaml": true,
		"/x/environment.go": false, "/x/main.go": false, "/x/keyboard.go": false, "/x/README.md": false,
	} {
		if SensitivePath(p) != want {
			t.Errorf("%s: want %v", p, want)
		}
	}
}
