package guard

import (
	"encoding/base64"
	"encoding/json"
	"strings"
	"testing"
)

func TestRedactRealisticLeaks(t *testing.T) {
	for name, c := range map[string]struct{ in, secret string }{
		"url":    {"connect postgres://app:" + "hunter2secret" + "@db:5432/x now", "hunter2secret"},
		"stripe": {"key sk_live_" + strings.Repeat("a1B2", 6), "sk_live_a1B2"},
		"basic":  {"Authorization: Basic " + base64.StdEncoding.EncodeToString([]byte("user:password"+"12345678")), "dXNlcjpwYXNz"},
		"spaces": {"password: correct horse battery staple", "horse battery"},
		"quoted": {`{"password": "two words here"}`, "two words"},
	} {
		out := Redact(c.in)
		if strings.Contains(out, c.secret) || !strings.Contains(out, Redacted) {
			t.Errorf("%s: secret survived: %q", name, out)
		}
	}
}

func TestRedactNeverBreaksJSON(t *testing.T) {
	for _, in := range []string{
		`{"output":"-----BEGIN PRIVATE KEY-----\nMIIEvQ"}`,
		`{"a":"-----BEGIN RSA PRIVATE KEY-----\nMII","b":"still here"}`,
		`{"cmd":"curl postgres://u:pw1234567@h/db","n":1}`,
		`{"x":"password: correct horse battery staple","y":2}`,
		`{"x":"token=abcdefgh and more","y":2}`,
		`{"auth": true, "tokens": 12}`,
	} {
		out := Redact(in)
		if !json.Valid([]byte(out)) {
			t.Errorf("redaction broke the JSON:\n in  %s\n out %s", in, out)
		}
	}
	if out := Redact(`{"a":"-----BEGIN RSA PRIVATE KEY-----\nMII","b":"still here"}`); !strings.Contains(out, "still here") {
		t.Errorf("a truncated key must not swallow the fields after it: %s", out)
	}
	if got := Redact(`{"auth": true, "tokens": 12}`); got != `{"auth": true, "tokens": 12}` {
		t.Errorf("ordinary JSON was changed: %s", got)
	}
}
