package recall

import "unicode/utf8"

// Tokenizer counts prompt tokens. The default is an estimate; swap in a real
// tokenizer through Options when a model-exact budget matters.
type Tokenizer interface {
	Count(s string) int
}

// Approx estimates one token per four characters, rounded up.
type Approx struct{}

func (Approx) Count(s string) int { return (utf8.RuneCountInString(s) + 3) / 4 }
