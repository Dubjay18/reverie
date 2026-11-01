// Package guard holds the deterministic rules that keep secrets and
// instruction-like text out of memory. No model is involved: a model judge
// can be argued with, a regular expression cannot.
package guard

import "regexp"

const Redacted = "[REDACTED]"

type rule struct {
	re   *regexp.Regexp
	repl string
}

var rules = []rule{
	// PEM private keys, including a block cut off before its END line.
	{regexp.MustCompile(`-----BEGIN [A-Z ]*PRIVATE KEY-----(?:[\s\S]*?-----END [A-Z ]*PRIVATE KEY-----|[\s\S]*$)`), Redacted},
	// well-known token shapes
	{regexp.MustCompile(`\bAKIA[0-9A-Z]{16}\b`), Redacted},
	{regexp.MustCompile(`\bgh[pousr]_[A-Za-z0-9]{30,}\b`), Redacted},
	{regexp.MustCompile(`\bgithub_pat_[A-Za-z0-9_]{20,}\b`), Redacted},
	{regexp.MustCompile(`\bxox[abprs]-[A-Za-z0-9-]{10,}\b`), Redacted},
	{regexp.MustCompile(`\bsk-[A-Za-z0-9_-]{20,}\b`), Redacted},
	{regexp.MustCompile(`\bAIza[0-9A-Za-z_-]{35}\b`), Redacted},
	{regexp.MustCompile(`\beyJ[A-Za-z0-9_-]{8,}\.[A-Za-z0-9_-]{8,}\.[A-Za-z0-9_-]{8,}\b`), Redacted},
	{regexp.MustCompile(`(?i)\bbearer\s+[A-Za-z0-9._~+/=-]{16,}`), "Bearer " + Redacted},
	// key=value and "key": "value" assignments: keep the key, drop the value
	{regexp.MustCompile(`(?i)(pass(?:word|wd)?|secret|token|api[_-]?key|access[_-]?key|private[_-]?key|authorization|auth)(\\?["']?\s*[:=]\s*\\?["']?)[^\s"',;&\\]{4,}`), "${1}${2}" + Redacted},
}

// Redact replaces anything that looks like a credential with [REDACTED]. It
// is a floor, not a guarantee: unusual secrets will get through.
func Redact(s string) string {
	for _, r := range rules {
		s = r.re.ReplaceAllString(s, r.repl)
	}
	return s
}

// RedactBytes is Redact for a payload, returning the input when nothing matched.
func RedactBytes(b []byte) []byte {
	out := Redact(string(b))
	if out == string(b) {
		return b
	}
	return []byte(out)
}

var sensitivePath = regexp.MustCompile(`(?i)(^|/)(\.env(\..*)?|.*\.(pem|key|p12|pfx|kdbx)|id_(rsa|dsa|ecdsa|ed25519)(\.pub)?|credentials|\.netrc|\.npmrc|\.pgpass|secrets?\.(ya?ml|json|toml))$`)

// SensitivePath reports whether a file path names a file that usually holds
// credentials, so its contents should not be recorded at all.
func SensitivePath(p string) bool { return sensitivePath.MatchString(p) }
