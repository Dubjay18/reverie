// Package ontology declares what facts may say and how conflicts resolve.
// Cardinality is what makes resolution mechanical: a new value on a "one"
// relation replaces the old one, on a "many" relation it is added.
package ontology

import (
	"bytes"
	_ "embed"
	"errors"
	"fmt"
	"io"
	"os"
	"regexp"
	"sort"
	"strings"

	"gopkg.in/yaml.v3"
)

type Cardinality string

const (
	One  Cardinality = "one"
	Many Cardinality = "many"
)

type Relation struct {
	Name        string
	Cardinality Cardinality
	Subject     string // entity type the relation applies to; empty = any
	TTLDays     int    // 0 = never expires
}

type Ontology struct {
	entities  map[string]struct{}
	relations map[string]Relation
}

var ErrUnknownRelation = errors.New("ontology: unknown relation")

var nameRE = regexp.MustCompile(`^[a-z][a-z0-9_]*$`)

type file struct {
	Entities  map[string]map[string]any `yaml:"entities"`
	Relations map[string]struct {
		Cardinality string `yaml:"cardinality"`
		Subject     string `yaml:"subject"`
		TTLDays     int    `yaml:"ttl_days"`
	} `yaml:"relations"`
}

//go:embed default.yaml
var defaultYAML []byte

// Default returns the built-in ontology.
func Default() *Ontology {
	o, err := Parse(bytes.NewReader(defaultYAML))
	if err != nil {
		panic("ontology: built-in default is invalid: " + err.Error())
	}
	return o
}

// Load reads and validates a schema.yaml.
func Load(path string) (*Ontology, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	o, err := Parse(f)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	return o, nil
}

// Parse validates strictly: unknown keys, bad names, an unknown cardinality,
// a relation whose subject type is not declared, and an empty relation set
// are all errors, so a typo in schema.yaml fails at startup, not at 2 a.m.
func Parse(r io.Reader) (*Ontology, error) {
	var f file
	dec := yaml.NewDecoder(r)
	dec.KnownFields(true)
	if err := dec.Decode(&f); err != nil {
		return nil, fmt.Errorf("ontology: %w", err)
	}
	o := &Ontology{entities: map[string]struct{}{}, relations: map[string]Relation{}}
	for name := range f.Entities {
		if !nameRE.MatchString(name) {
			return nil, fmt.Errorf("ontology: entity type %q: names are lower_snake_case", name)
		}
		o.entities[name] = struct{}{}
	}
	for name, r := range f.Relations {
		if !nameRE.MatchString(name) {
			return nil, fmt.Errorf("ontology: relation %q: names are lower_snake_case", name)
		}
		c := Cardinality(r.Cardinality)
		if c != One && c != Many {
			return nil, fmt.Errorf("ontology: relation %q: cardinality must be \"one\" or \"many\", got %q", name, r.Cardinality)
		}
		if r.Subject != "" {
			if _, ok := o.entities[r.Subject]; !ok {
				return nil, fmt.Errorf("ontology: relation %q: subject %q is not a declared entity type", name, r.Subject)
			}
		}
		if r.TTLDays < 0 {
			return nil, fmt.Errorf("ontology: relation %q: ttl_days cannot be negative", name)
		}
		o.relations[name] = Relation{Name: name, Cardinality: c, Subject: r.Subject, TTLDays: r.TTLDays}
	}
	if len(o.relations) == 0 {
		return nil, errors.New("ontology: no relations declared")
	}
	return o, nil
}

func (o *Ontology) Relation(name string) (Relation, error) {
	r, ok := o.relations[name]
	if !ok {
		return Relation{}, fmt.Errorf("%w %q (known: %s)", ErrUnknownRelation, name, strings.Join(o.Relations(), ", "))
	}
	return r, nil
}

func (o *Ontology) Has(name string) bool { _, ok := o.relations[name]; return ok }

func (o *Ontology) Relations() []string {
	names := make([]string, 0, len(o.relations))
	for n := range o.relations {
		names = append(names, n)
	}
	sort.Strings(names)
	return names
}
