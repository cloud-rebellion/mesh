// SPDX-License-Identifier: LicenseRef-Mesh-Sustainable-Use-License
// Copyright (C) 2026 Bright Interaction AB

package vault

import (
	"errors"
	"fmt"
	"strings"

	"gopkg.in/yaml.v3"
)

// NoteType is the kind of a note. The first five are the canonical Mesh types.
// concept and map are accepted so `mesh migrate` can ingest a legacy-style vault
// losslessly (Open Decision 4 default: extend the enum rather than remap).
type NoteType string

const (
	TypeNote       NoteType = "note"
	TypePostMortem NoteType = "post-mortem"
	TypeDecision   NoteType = "decision"
	TypeGotcha     NoteType = "gotcha"
	TypeEntity     NoteType = "entity"
	TypeConcept    NoteType = "concept"
	TypeMap        NoteType = "map"
	TypeStatus     NoteType = "status"
)

var validTypes = map[NoteType]bool{
	TypeNote: true, TypePostMortem: true, TypeDecision: true,
	TypeGotcha: true, TypeEntity: true, TypeConcept: true, TypeMap: true, TypeStatus: true,
}

func (t NoteType) Valid() bool { return validTypes[t] }

// RequiresFlywheel identifies legacy institutional-memory layouts.
// Deprecated: modern completeness is defined by a versioned template.
func (t NoteType) RequiresFlywheel() bool {
	return t == TypeDecision || t == TypeGotcha || t == TypePostMortem
}

// StringList decodes either a YAML scalar or a sequence into []string, so
// `related: foo` and `related: [foo, bar]` both work.
type StringList []string

func (s *StringList) UnmarshalYAML(value *yaml.Node) error {
	if value.Kind == yaml.ScalarNode {
		if value.Value == "" {
			*s = nil
			return nil
		}
		*s = StringList{value.Value}
		return nil
	}
	var xs []string
	if err := value.Decode(&xs); err != nil {
		return err
	}
	*s = xs
	return nil
}

// ExemptsDeadRef reports whether ref is one of the paths a note declared synthetic in
// `expect_dead_ref_paths`. Both sides get the same "./" trim the dead-ref scanner applies to
// what it finds, so an author can paste a reported path straight back into the list.
func ExemptsDeadRef(paths []string, ref string) bool {
	ref = strings.TrimLeft(strings.TrimSpace(ref), "./")
	for _, p := range paths {
		if strings.TrimLeft(strings.TrimSpace(p), "./") == ref {
			return true
		}
	}
	return false
}

// Frontmatter is the whitelisted view of a note's YAML header. Raw YAML is
// never spread into storage; only known keys are kept (the JSONB house rule).
type Frontmatter struct {
	ID      string   `yaml:"id"`
	Type    NoteType `yaml:"type"`
	Title   string   `yaml:"title"`
	When    string   `yaml:"when"`
	Created string   `yaml:"created,omitempty"`
	Updated string   `yaml:"updated,omitempty"`
	// Authoring identity and discovery metadata. Prose is stored only in the body.
	Template        string          `yaml:"template,omitempty"`
	TemplateVersion int             `yaml:"template_version,omitempty"`
	Summary         string          `yaml:"summary,omitempty"`
	Collections     StringList      `yaml:"collections,omitempty"`
	Blocks          []BlockMetadata `yaml:"blocks,omitempty"`
	// VerifiedAt never follows Updated automatically. It requires explicit evidence.
	VerifiedAt    string            `yaml:"verified_at,omitempty"`
	BodySummary   string            `yaml:"-"`
	Sections      map[string]string `yaml:"-"`
	BlockContents []BlockSpec       `yaml:"-"`
	Related       StringList        `yaml:"related,omitempty"`
	Tags          StringList        `yaml:"tags,omitempty"`
	Do            string            `yaml:"do,omitempty"`
	Dont          string            `yaml:"dont,omitempty"`
	Why           string            `yaml:"why,omitempty"`
	Status        string            `yaml:"status,omitempty"`
	// ExpectDeadRefs marks a note whose subject IS a deletion: the file paths it cites
	// are meant to be gone, so dead_ref would flag it forever and correctly. Six such
	// notes (retired services, an old audit) were permanently red, and a health check
	// with permanent known-good findings is one people learn to skim past.
	ExpectDeadRefs bool `yaml:"expect_dead_refs,omitempty"`
	// ExpectDeadRefPaths exempts individual paths instead of the whole note, for a note that
	// merely QUOTES a path that never existed: a fabricated stack frame inside a test
	// narrative, an illustrative filename in an example. Nothing in such a path tells it
	// apart from a real file that got deleted, so the author is the only one who can say.
	// Both live cases were a single synthetic frame in prose on a page citing dozens of real
	// files, so ExpectDeadRefs would have switched the check off exactly where it earns the
	// most, while leaving them meant the count could never read zero and a genuine new dead
	// ref had to be spotted against a permanently non-zero baseline.
	//
	// This is a SEPARATE key rather than a list shape on expect_dead_refs, and that is the
	// whole point: frontmatter is read by long-running daemons and a synced hub that are
	// routinely a binary behind, and yaml.v3 ignores a key it does not know but REFUSES a
	// known key of the wrong type. Overloading expect_dead_refs was tried first and made
	// both notes unparseable to every reader still on the old binary, which drops a note
	// from search and the graph entirely: `[[flare]]` stopped resolving across 88 links
	// until the shape was moved here. An unknown key degrades to a stale finding instead.
	ExpectDeadRefPaths StringList `yaml:"expect_dead_ref_paths,omitempty"`
	Supersedes         StringList `yaml:"supersedes,omitempty"`
	Severity           string     `yaml:"severity,omitempty"`
	Role               string     `yaml:"role,omitempty"`
	Stack              StringList `yaml:"stack,omitempty"`
	RepoPath           string     `yaml:"repo_path,omitempty"`
	// Provenance: who/what wrote this note, where it came from, when to recheck.
	// Feeds the audit trail, the knowledge-lifecycle health checks, and the
	// contributor/ROI views. All optional.
	Author     string `yaml:"author,omitempty"`     // human who authored it
	Agent      string `yaml:"agent,omitempty"`      // tool that wrote it, e.g. "claude-code"
	Source     string `yaml:"source,omitempty"`     // manual | agent | import:<connector>
	SourceURL  string `yaml:"source_url,omitempty"` // upstream link for imported notes
	Confidence string `yaml:"confidence,omitempty"` // low | med | high
	ReviewBy   string `yaml:"review_by,omitempty"`  // YYYY-MM-DD or RFC3339; lifecycle re-check deadline
	ImportedAt string `yaml:"imported_at,omitempty"`
	// Scope is the access-control partition(s) this note belongs to (dev, sales, ...).
	// A note may carry several. ABSENCE means dev-only (the fail-safe): an unlabeled
	// note is never accidentally exposed to or writable by a non-dev scope. Read
	// EffectiveScopes() rather than this field directly so the default lives in one place.
	Scope StringList `yaml:"scope,omitempty"`
}

// DefaultScope is the scope an unlabeled note belongs to. Unlabeled = dev-only, so a
// note that predates scoping (or forgets the field) is never leaked to a non-dev scope.
const DefaultScope = "dev"

// EffectiveScopes returns the note's access scopes, defaulting to {DefaultScope} when
// none are declared. This is the single source of the fail-safe default; every read
// and write check must go through it.
func (f *Frontmatter) EffectiveScopes() []string {
	var out []string
	for _, s := range f.Scope {
		if t := strings.TrimSpace(s); t != "" {
			out = append(out, t)
		}
	}
	if len(out) == 0 {
		return []string{DefaultScope}
	}
	return out
}

// ScopeAllows reports whether a note carrying noteScopes is readable given the caller's
// allowed-read set. nil allowed = unrestricted (no scoping configured). Absent/empty
// noteScopes falls back to DefaultScope, the fail-safe every read check must honor.
//
// This is the ONE scope-intersect predicate. It used to be hand-copied in the MCP,
// retrieve, and web layers (with subtly different shapes), which is exactly how the
// changed_since / health / code_* leaks happened: a surface reimplemented the check and
// got it wrong. All read surfaces now call this (or ScopeAllowsCSV) so the logic cannot
// drift per surface.
func ScopeAllows(noteScopes []string, allowed map[string]bool) bool {
	if allowed == nil {
		return true // unrestricted: scoping not configured
	}
	labeled := false
	for _, s := range noteScopes {
		if t := strings.TrimSpace(s); t != "" {
			labeled = true
			if allowed[t] {
				return true
			}
		}
	}
	if !labeled {
		return allowed[DefaultScope] // unlabeled note = dev-only fail-safe
	}
	return false
}

// ScopeAllowsCSV is ScopeAllows for a comma-joined scope string, the shape the index
// stores (notes.scope, graph node Attrs["scope"]).
func ScopeAllowsCSV(csv string, allowed map[string]bool) bool {
	if allowed == nil {
		return true
	}
	return ScopeAllows(strings.Split(csv, ","), allowed)
}

// ParseFrontmatter decodes a YAML frontmatter block into the whitelisted struct
// and a raw map. Empty input yields a zero Frontmatter, not an error.
func ParseFrontmatter(b []byte) (*Frontmatter, map[string]any, error) {
	fm := &Frontmatter{}
	raw := map[string]any{}
	if len(b) > 0 {
		if err := yaml.Unmarshal(b, fm); err != nil {
			return nil, nil, fmt.Errorf("frontmatter: %w", err)
		}
		if err := yaml.Unmarshal(b, &raw); err != nil {
			return nil, nil, fmt.Errorf("frontmatter raw: %w", err)
		}
	}
	if fm.Type == "" {
		fm.Type = TypeNote
	}
	return fm, raw, nil
}

// Validate returns the lint problems for this frontmatter. An empty slice means
// it satisfies the schema for its type.
func (f *Frontmatter) Validate() []string {
	var errs []string
	if f.ID == "" {
		errs = append(errs, "missing id")
	}
	if !f.Type.Valid() {
		errs = append(errs, fmt.Sprintf("invalid type %q", f.Type))
	}
	if f.When == "" {
		errs = append(errs, "missing when")
	}
	if f.Template != "" || f.TemplateVersion != 0 {
		t, err := TemplateFor(f.Template, f.TemplateVersion)
		if err != nil {
			errs = append(errs, err.Error())
		} else if t.Type != f.Type {
			errs = append(errs, "template does not match note type")
		}
	} else {
		// Historical placeholders are reported by the isolated legacy adapter.
		// This does not impose a triad contract on modern notes.
		errs = append(errs, ReadLegacy(f).Missing()...)
	}
	return errs
}

// unfilled reports whether a flywheel field is still empty or a TODO placeholder
// (mesh new leaves "TODO" sentinels for the author to replace).
// Unfilled reports whether a flywheel field is still a placeholder rather than authored
// guidance. Exported because the INDEXER needs the same answer lint does: `mesh new` writes
// the literal "TODO" into do/dont/why, and indexing that made every unfilled note match a
// search for "TODO" with its own placeholder as the excerpt, while the embedding header
// (prepended to EVERY chunk of a note) read "TODO\nTODO\nTODO" and pulled unfilled notes
// toward each other in vector space on a token carrying no meaning. One definition, shared,
// so a field can never be nagged about and indexed at the same time.
func Unfilled(s string) bool {
	s = strings.TrimSpace(s)
	if s == "" {
		return true
	}
	u := strings.ToUpper(s)
	if !strings.HasPrefix(u, "TODO") {
		return false
	}
	// Match TODO as a WORD, not as a prefix. "todos are tracked in the tracker" is authored
	// guidance that happens to start with those four letters; treating it as a placeholder
	// would nag the author AND, now that the indexer shares this, drop real content out of
	// search.
	rest := u[len("TODO"):]
	if rest == "" {
		return true
	}
	c := rest[0]
	return !(c >= 'A' && c <= 'Z' || c >= '0' && c <= '9')
}

func unfilled(s string) bool { return Unfilled(s) }

// ErrUnterminatedFrontmatter is returned for a note that opens a frontmatter block
// with --- and never closes it. That file is not "a note without frontmatter": every
// field the author declared (type, title, tags, related, do/dont/why) silently becomes
// body prose, so the note indexes with fabricated defaults - type note, no tags, no
// edges, an id-as-prose title - and pollutes the graph with a record that contradicts
// what the file plainly says. Invalid YAML inside a closed block has always been a hard
// error; this is the same class and is now treated the same way.
var ErrUnterminatedFrontmatter = errors.New("frontmatter opened with --- but is never closed, so every declared field (type, title, tags, related) would be read as body prose; add a closing --- line")

// splitFM is the single scanner behind SplitFrontmatter and UnterminatedFrontmatter.
// They share one pass on purpose: this vault already paid for three readers of the same
// markdown disagreeing about where a construct begins and ends, and a second scanner that
// answered "is this block closed" independently could drift from the one that splits it.
func splitFM(content string) (fm, body string, had, unterminated bool) {
	lines := strings.Split(content, "\n")
	if len(lines) == 0 || !frontmatterDelimiter(lines[0], true) {
		return "", content, false, false
	}
	for i := 1; i < len(lines); i++ {
		if frontmatterDelimiter(lines[i], false) {
			return strings.Join(lines[1:i], "\n"), strings.Join(lines[i+1:], "\n"), true, false
		}
	}
	return "", content, false, true
}

// frontmatterDelimiter accepts the byte-order mark some editors write before the
// first UTF-8 character and harmless horizontal whitespace after the marker. It stays
// deliberately stricter than TrimSpace: leading indentation would make --- ordinary
// Markdown/YAML content, not a document delimiter.
func frontmatterDelimiter(line string, opener bool) bool {
	line = strings.TrimSuffix(line, "\r")
	if opener {
		line = strings.TrimPrefix(line, "\ufeff")
	}
	return strings.TrimRight(line, " \t") == "---"
}

// SplitFrontmatter separates a leading YAML frontmatter block from the body. It
// returns the inner YAML (no --- markers), the body after the closing marker,
// and whether a block was present. An unterminated block reports had=false; callers
// that must tell that apart from "no frontmatter at all" ask UnterminatedFrontmatter.
func SplitFrontmatter(content string) (fm string, body string, had bool) {
	fm, body, had, _ = splitFM(content)
	return fm, body, had
}

// UnterminatedFrontmatter reports whether content opens a frontmatter block that is
// never closed. Callers that write or index a note must refuse it rather than treat it
// as an unlabeled note; see ErrUnterminatedFrontmatter.
func UnterminatedFrontmatter(content string) bool {
	_, _, _, unterminated := splitFM(content)
	return unterminated
}
