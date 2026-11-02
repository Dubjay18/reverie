// Package guard holds the deterministic rules that keep secrets and
// instruction-like text out of memory. No model is involved: a model judge
// can be argued with, a regular expression cannot.
package guard

import (
	"regexp"
	"strings"
	"unicode"
)

const Redacted = "[REDACTED]"

type rule struct {
	re   *regexp.Regexp
	repl string
}

var rules = []rule{
	// PEM private keys. A block cut off before its END line is redacted up to
	// the next quote, so a JSON string around it keeps its closing quote.
	{regexp.MustCompile(`-----BEGIN [A-Z ]*PRIVATE KEY-----(?:[\s\S]*?-----END [A-Z ]*PRIVATE KEY-----|[^"]*)`), Redacted},
	// well-known token shapes
	{regexp.MustCompile(`\bAKIA[0-9A-Z]{16}\b`), Redacted},
	{regexp.MustCompile(`\bgh[pousr]_[A-Za-z0-9]{30,}\b`), Redacted},
	{regexp.MustCompile(`\bgithub_pat_[A-Za-z0-9_]{20,}\b`), Redacted},
	{regexp.MustCompile(`\bxox[abprs]-[A-Za-z0-9-]{10,}\b`), Redacted},
	{regexp.MustCompile(`\b(?:sk|rk|pk)_(?:live|test)_[A-Za-z0-9]{16,}\b`), Redacted},
	{regexp.MustCompile(`\bsk-[A-Za-z0-9_-]{20,}\b`), Redacted},
	{regexp.MustCompile(`\bAIza[0-9A-Za-z_-]{35}\b`), Redacted},
	{regexp.MustCompile(`\beyJ[A-Za-z0-9_-]{8,}\.[A-Za-z0-9_-]{8,}\.[A-Za-z0-9_-]{8,}\b`), Redacted},
	{regexp.MustCompile(`(?i)\b(bearer|basic)\s+[A-Za-z0-9._~+/=-]{16,}`), "${1} " + Redacted},
	// credentials inside a URL: scheme://user:password@host
	{regexp.MustCompile(`(?i)\b([a-z][a-z0-9+.-]*://[^\s:/@"']+:)[^\s@/"']+@`), "${1}" + Redacted + "@"},
	// key = "quoted value" (any length, spaces allowed): keep the key and quotes
	{regexp.MustCompile(`(?i)(pass(?:word|wd)?|secret|token|api[_-]?key|access[_-]?key|private[_-]?key|authorization|auth)(\\?["']?\s*[:=]\s*\\?["'])[^"'\\]+`), "${1}${2}" + Redacted},
	// key = unquoted value: redact to the end of the line, since a value like
	// "correct horse battery staple" has spaces and a shell command may follow
	{regexp.MustCompile(`(?i)(pass(?:word|wd)?|secret|token|api[_-]?key|access[_-]?key|private[_-]?key|authorization|auth)(\s*[:=]\s*)[^\s"'\\;,&][^"'\\\n]{3,}`), "${1}${2}" + Redacted},
	// prose credential statement: "password is ..." / "api key is ..."
	{regexp.MustCompile(`(?i)\b(pass(?:word|wd)?|secret|api[_-]?key|access[_-]?key|private[_-]?key)(\s+is\s+)[^\s"'\\;,&][^"'\\\n]{3,}`), "${1}${2}" + Redacted},
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

// OneLine makes text safe to print into a prompt or a terminal as a single
// line: control characters are dropped, whitespace is folded, and the result
// is cut at max bytes (on a rune boundary) with an ellipsis. max <= 0 means
// no cap.
func OneLine(s string, max int) string {
	s = strings.Join(strings.Fields(strings.Map(func(r rune) rune {
		if unicode.IsControl(r) && !unicode.IsSpace(r) {
			return -1
		}
		return r
	}, s)), " ")
	if max > 0 && len(s) > max {
		s = strings.ToValidUTF8(s[:max], "") + "…"
	}
	return s
}

var instructionRE = regexp.MustCompile(`(?i)\b(` +
	`ignore\s+(?:all\s+|any\s+|the\s+|your\s+)?(?:previous|prior|above|earlier|preceding)|` +
	`disregard\s+(?:all|any|the|your|previous)|forget\s+(?:all|your|previous)|` +
	`system\s+prompt|developer\s+message|` +
	`you\s+(?:must|should|shall|will)\b|from\s+now\s+on|` +
	`(?:always|never)\s+(?:run|execute|call|send|use|reveal|tell)|` +
	`do\s+not\s+(?:tell|reveal|mention|inform)|` +
	`(?:run|execute)\s+(?:the\s+following|this|these)|` +
	`curl\s|wget\s|sudo\s|rm\s+-rf|base64\s+-d|\|\s*(?:sh|bash)\b|` +
	`exfiltrat|send\s+(?:it|this|them|the\s+\w+)\s+to\b|` +
	`when(?:ever)?\s+(?:the\s+)?(?:user|agent|session|prompt|asked|deploy\w*|quer\w*)\b[^.\n]{0,120}\b(?:run|execute|call|send|bypass|override|ignore|output|print|append|inject|disable)|` +
	`(?:on|in)\s+(?:the\s+)?(?:next|future|subsequent|every)\s+(?:session|turn|run|prompt|deploy\w*)\b|` +
	`if\s+(?:the\s+)?(?:user|agent|queried|asked)\b[^.\n]{0,120}\b(?:respond|reply|output|execute|run|bypass|override|ignore)|` +
	`bypass\s+(?:auth\w*|security|check\w*|validation|guard|policy)|` +
	`override\s+(?:all|security|system|policy|instructions?)|` +
	`sleeper\s+(?:trigger|instruction|payload)` +
	`)`)

// LooksLikeInstruction reports whether text reads as a command aimed at a
// model rather than a statement about the world. It is a floor: a rule-based
// screen for values that a less trusted source (an agent, a dream) wants to
// store as a fact, which later get printed into prompts.
func LooksLikeInstruction(s string) bool { return instructionRE.MatchString(s) }
