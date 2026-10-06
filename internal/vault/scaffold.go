// SPDX-License-Identifier: LicenseRef-Mesh-Sustainable-Use-License
// Copyright (C) 2026 Bright Interaction AB

package vault

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
	"unicode"

	"github.com/bright-interaction/mesh/internal/latency"
	"golang.org/x/text/unicode/norm"
	"gopkg.in/yaml.v3"
)

// Now returns the current time and is overridable in tests.
var Now = func() time.Time { return time.Now() }

// NewNoteSpec is the irreducible, judgment-only input an author provides. Mesh
// derives everything else: id, timestamps, placement, filename, skeleton.
type NewNoteSpec struct {
	Type            NoteType          `json:"type,omitempty"`
	Title           string            `json:"title,omitempty"`
	Template        string            `json:"template,omitempty"`
	TemplateVersion int               `json:"template_version,omitempty"`
	Summary         string            `json:"summary,omitempty"`
	Sections        map[string]string `json:"sections,omitempty"`
	Blocks          []BlockSpec       `json:"blocks,omitempty"`
	Collections     []string          `json:"collections,omitempty"`
	Supersedes      []string          `json:"supersedes,omitempty"`
	VerifiedAt      string            `json:"verified_at,omitempty"`
	DraftPath       string            `json:"-"` // internal authorized vault-relative target
	DraftID         string            `json:"draft_id,omitempty"`
	DraftRevision   string            `json:"draft_revision,omitempty"`
	UpdatePath      string            `json:"-"` // internal authorized vault-relative target
	UpdateID        string            `json:"update_id,omitempty"`
	UpdateRevision  string            `json:"update_revision,omitempty"`
	// Deprecated source aliases keep old callers compiling. The modern writer
	// rejects them; historical content is read through ReadLegacy.
	Do       string   `json:"do,omitempty"`
	Dont     string   `json:"dont,omitempty"`
	Why      string   `json:"why,omitempty"`
	Related  []string `json:"related,omitempty"`
	Tags     []string `json:"tags,omitempty"`
	Status   string   `json:"status,omitempty"`
	Severity string   `json:"severity,omitempty"`
	By       string   `json:"by,omitempty"`
	// Provenance (all optional; recorded in the note's frontmatter).
	Author     string `json:"author,omitempty"`
	Agent      string `json:"agent,omitempty"`
	Source     string `json:"source,omitempty"`
	SourceURL  string `json:"source_url,omitempty"`
	Confidence string `json:"confidence,omitempty"`
	ReviewBy   string `json:"review_by,omitempty"`
	ImportedAt string `json:"imported_at,omitempty"`
	// Scope is the access-control partition(s) to stamp on the new note. Empty leaves
	// the note unlabeled (= dev-only by the EffectiveScopes default).
	Scope []string `json:"scope,omitempty"`
}

// CreateResult reports what Mesh filled in and what the author still must.
type CreateResult struct {
	Revision string
	Path     string
	ID       string
	When     string
	TODOs    []string
}

// PreparedNote is a fully rendered note that has not yet been published. It exists for
// callers such as the team hub that must put note creation inside a larger durable
// transaction (the hub's Git commit). Content is ready to write at Result.Path.
//
// Preparing does NOT reserve the id. The caller must serialize PrepareNoteContext and
// publication against every other writer for this vault. Ordinary callers must use
// CreateNoteContext, whose O_EXCL publication supplies that serialization itself.
type PreparedNote struct {
	Result  CreateResult
	Content []byte
	// PreviousPath is populated when resuming or promoting an existing draft.
	PreviousPath string
	// OriginalContent is retained before a published-note replacement. Hosted
	// publishers retain it in Git history; the local publisher archives exact bytes.
	OriginalContent []byte
}

type notePlan struct {
	base    string
	dir     string
	date    string
	fm      *Frontmatter
	claimed map[string]string
	todos   []string
}

// DirForType maps a note type to its vault subdirectory.
func DirForType(t NoteType) string {
	switch t {
	case TypeDecision:
		return "decisions"
	case TypeGotcha:
		return "gotchas"
	case TypePostMortem:
		return "post-mortems"
	case TypeEntity:
		return "entities"
	case TypeConcept:
		return "concepts"
	case TypeMap:
		return "maps"
	case TypeStatus:
		return "statuses"
	default:
		return "notes"
	}
}

// slugFold maps the Latin-1 and Scandinavian letters a real vault actually contains
// onto their ASCII base. It exists because the [a-z0-9] filter below DELETED them
// instead: the Swedish heading "Atgarder" (a-ring, a-umlaut) slugged to "tg-rder", an
// anchor no caller can guess, and a Swedish title minted a note id nobody can type.
// Folding runs before the filter, so the output is still pure ASCII and every
// all-ASCII input slugs byte-identically to what it always did.
// The keys are written as escapes on purpose: a literal diacritic in this source file
// is one careless editor round trip away from being normalised to plain ASCII, which
// would turn an entry into a silent duplicate of 'a' and change nothing at runtime.
var slugFold = map[rune]string{
	'\u00e0': "a", '\u00e1': "a", '\u00e2': "a", '\u00e3': "a", '\u00e4': "a", '\u00e5': "a", // a grave acute circumflex tilde diaeresis ring
	'\u00e6': "ae", '\u00e7': "c",
	'\u00e8': "e", '\u00e9': "e", '\u00ea': "e", '\u00eb': "e", // e grave acute circumflex diaeresis
	'\u00ec': "i", '\u00ed': "i", '\u00ee': "i", '\u00ef': "i", // i grave acute circumflex diaeresis
	'\u00f0': "d", '\u00f1': "n",
	'\u00f2': "o", '\u00f3': "o", '\u00f4': "o", '\u00f5': "o", '\u00f6': "o", '\u00f8': "o", // o grave acute circumflex tilde diaeresis stroke
	'\u00f9': "u", '\u00fa': "u", '\u00fb': "u", '\u00fc': "u", // u grave acute circumflex diaeresis
	'\u00fd': "y", '\u00ff': "y",
	'\u00fe': "th", '\u00df': "ss", '\u0153': "oe",
}

// Slugify turns arbitrary text into a kebab-case Unicode slug. Used for ids, filenames,
// and heading anchors. Familiar Latin diacritics keep their established ASCII folds,
// while scripts that cannot be transliterated losslessly remain addressable instead of
// collapsing to an empty anchor.
func Slugify(s string) string {
	var b strings.Builder
	prevDash := false
	for _, r := range norm.NFC.String(strings.ToLower(s)) {
		if folded, ok := slugFold[r]; ok {
			b.WriteString(folded)
			prevDash = false
			continue
		}
		switch {
		case unicode.IsLetter(r), unicode.IsDigit(r), unicode.IsMark(r) && b.Len() > 0:
			b.WriteRune(r)
			prevDash = false
		default:
			if !prevDash && b.Len() > 0 {
				b.WriteByte('-')
				prevDash = true
			}
		}
	}
	return strings.Trim(b.String(), "-")
}

// ErrInvalidSpec marks the errors CreateNote raises about the CALLER's input, as
// opposed to the ones the filesystem raises. Every message wrapping it is authored
// here, names only caller-supplied values, and never contains a server path, so a
// remote surface can hand it back verbatim instead of guessing what is safe to echo.
var ErrInvalidSpec = errors.New("invalid note spec")

// maxSlugLen bounds the id, and therefore the filename, so an over-long title is
// refused with an actionable message instead of an ENAMETOOLONG from the kernel whose
// text carries the server's ABSOLUTE vault path. The budget is NAME_MAX (255 bytes on
// APFS and ext4) minus the ".md" extension and the widest collision suffix the loop
// below can append ("-1000"): 255 - 3 - 5 = 247. Measured on APFS, a 247-byte base
// writes and a 248-byte one fails, and the longest real filename in the reference
// vault is 250 bytes (a 247-byte base plus ".md"), so this refuses only what the
// filesystem would refuse anyway.
const maxSlugLen = 247

// CreateNote writes a new note with everything derivable already filled: id from
// the title (collision-suffixed), when/created auto-stamped, placed in the
// type's subdirectory, with a type-specific body skeleton. The author only fills
// the judgment fields. Returns the path and any flywheel fields still to fill.
func CreateNote(root string, spec NewNoteSpec) (*CreateResult, error) {
	return CreateNoteContext(context.Background(), root, spec)
}

// CreateNoteContext is CreateNote with caller-controlled cancellation for the
// pre-publication work. In particular, the vault-global id scan can be expensive in a
// large vault, so it observes ctx while walking and reading note heads.
//
// The successful O_EXCL open below is the publication boundary. Once a path has been
// claimed, cancellation is deliberately not consulted until that attempt has either
// durably completed or removed its claim: returning early in between would strand an
// empty or partially-written markdown file that the indexer could mistake for a note.
func CreateNoteContext(ctx context.Context, root string, spec NewNoteSpec) (*CreateResult, error) {
	if err := RequireAuthoringWrites(); err != nil {
		return nil, err
	}
	if spec.UpdateID != "" {
		return updateNoteContext(ctx, root, spec)
	}
	if spec.DraftID != "" {
		return resumeDraftContext(ctx, root, spec)
	}
	return createNoteContext(ctx, root, spec, ClaimedIDsContext, os.OpenFile)
}

// createNoteContext keeps the two filesystem boundaries injectable without mutable
// package globals. Production always supplies the real scanner and O_EXCL open above;
// focused tests can cancel on either side of publication deterministically, including
// under the race detector.
func createNoteContext(
	ctx context.Context,
	root string,
	spec NewNoteSpec,
	claimedIDs func(context.Context, string) (map[string]string, error),
	openFile func(string, int, os.FileMode) (*os.File, error),
) (*CreateResult, error) {
	trace := latency.Start("note_create", "plan")
	defer trace.End()
	plan, err := planNoteContext(ctx, root, spec, claimedIDs)
	if err != nil {
		return nil, err
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	// Do not create even an empty type directory until the cancellable whole-vault
	// scan has completed. That keeps pre-publication cancellation side-effect free.
	trace.Phase("mkdir")
	if err := os.MkdirAll(plan.dir, 0o755); err != nil {
		return nil, err
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	// Claim the filename by CREATING it, and let the id follow the claim. The old shape
	// picked a free path with os.Stat and then wrote it with os.WriteFile, which is
	// check-then-act: two agents writing back the same title concurrently both saw the
	// base path free, both rendered `id: <base>`, and the second write TRUNCATED the
	// first. Both got a success receipt naming a note that no longer existed - silent
	// loss of exactly the write-back the flywheel exists to capture, and reachable from
	// any concurrent caller (mesh mcp --http, the hub's /mcp, internal/web/pending_api).
	// O_EXCL makes the claim atomic, so a loser sees ErrExist and takes the next suffix.
	for n := 1; n <= maxIDAttempts; n++ {
		trace.Phase("render_validate")
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		// Shared with the in-place rewriter in IDClaims.Claim: the two writers must mint
		// the same candidate sequence or each reads the ids the other reserved as free.
		id := candidateID(plan.base, n)
		if _, taken := plan.claimed[id]; taken {
			continue // held by a note somewhere in the vault, in this directory or another
		}
		path := filepath.Join(plan.dir, id+".md")
		plan.fm.ID = id

		content, err := renderNote(plan.fm, spec.By)
		if err != nil {
			return nil, err
		}
		// Guard: a note whose frontmatter does not re-parse would be silently dropped
		// from the index (invalid YAML removes it from search and the graph with no
		// warning). yaml.Marshal quotes values correctly today, so this should never
		// fire, but it makes that a hard invariant: Mesh's own tools never write a note
		// that would vanish.
		if err := validateRoundTrip(content, id); err != nil {
			return nil, err
		}
		// Last cancellation point before publication. After OpenFile succeeds, this
		// attempt owns a visible path and must finish or remove it before returning.
		if err := ctx.Err(); err != nil {
			return nil, err
		}

		trace.Phase("claim_open")
		f, err := openFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o644)
		if errors.Is(err, os.ErrExist) {
			continue // taken, by an earlier note or by a concurrent writer
		}
		if err != nil {
			return nil, err
		}
		trace.Phase("write")
		if _, err := f.Write([]byte(content)); err != nil {
			trace.Phase("claim_cleanup")
			closeErr := f.Close()
			return nil, errors.Join(err, closeErr, removeNoteClaim(path, plan.dir))
		}
		// Flush the note's bytes to the device before this returns a CreateResult naming
		// it. Everything downstream treats that receipt as "the note exists": the index
		// records its hash, sync.json records it as the base for the next round, and the
		// agent that wrote it moves on. Without the fsync a power cut leaves the receipt's
		// path holding a short or empty file, and the next sync round pushes THAT as the
		// note's content. This is the primary authoring path for the whole flywheel
		// (`mesh new`, MCP mesh_append_note and mesh_write_entity, the hub's /mcp,
		// internal/web/pending_api), so it is the one that must not be lossy.
		//
		// The temp+rename shape used elsewhere is deliberately NOT used here: the O_EXCL
		// open is what makes claiming the id atomic, and a rename would hand the race back
		// (two writers rendering the same id, the second overwriting the first, both
		// getting a success receipt). Durability is added around that claim, not instead
		// of it. A failed write or fsync releases the claim rather than leaving a
		// half-written note behind at an id the caller was told nothing about.
		trace.Phase("file_sync")
		if err := f.Sync(); err != nil {
			trace.Phase("claim_cleanup")
			closeErr := f.Close()
			return nil, errors.Join(err, closeErr, removeNoteClaim(path, plan.dir))
		}
		trace.Phase("file_close")
		if err := f.Close(); err != nil {
			trace.Phase("claim_cleanup")
			return nil, errors.Join(err, removeNoteClaim(path, plan.dir))
		}
		// The note is a NEW directory entry, so the data fsync alone does not make it
		// reachable after a power cut. Fsync the directory too, after the file.
		trace.Phase("directory_sync")
		syncDir(plan.dir)
		// Re-check for a racer in another directory now that our own file exists. The
		// vault scan above is check-then-act ACROSS directories: two CreateNote calls for
		// the same title with different types can both scan, both find the id free, and
		// both create, because their O_EXCL claims are in different directories and never
		// meet. Both would then return success for one id, and the indexer would have to
		// quarantine one of the two notes the caller was told it had written.
		//
		// Whoever sees the other backs off and takes the next suffix, rather than one
		// keeping the id by some ordering rule: backing off is what preserves the promise
		// this function makes, that a receipt names a note that exists and is retrievable.
		// Both racers backing off at once is possible and harmless (each moves to the next
		// suffix and the loop is bounded); it needs the two creates to land inside the
		// microseconds between the other's create and its stat scan, and every retry
		// re-rolls that timing.
		trace.Phase("collision_check")
		other, checkErr := otherFileNamedForIDContext(ctx, root, id, path)
		if checkErr != nil {
			trace.Phase("claim_cleanup")
			// Cancellation after publication cannot return while our visible path is
			// unresolved. The file is already closed and valid; remove it synchronously
			// before surfacing cancellation, so no writer continues after this call.
			return nil, errors.Join(checkErr, removeNoteClaim(path, plan.dir))
		}
		if other != "" {
			trace.Phase("claim_cleanup")
			if err := removeNoteClaim(path, plan.dir); err != nil {
				return nil, err
			}
			plan.claimed[id] = other
			continue
		}
		return &CreateResult{Path: path, ID: id, When: plan.date, TODOs: plan.todos, Revision: ContentRevision([]byte(content))}, nil
	}
	return nil, fmt.Errorf("%w: could not claim a free note id for %q after %d attempts; %d notes in this vault already hold ids starting with that slug, so give this note a more specific title",
		ErrInvalidSpec, plan.base, maxIDAttempts, maxIDAttempts)
}

// PrepareNoteContext renders the exact note CreateNoteContext would create without
// touching the filesystem. See PreparedNote: callers must serialize this with the
// durable publication that follows it.
func PrepareNoteContext(ctx context.Context, root string, spec NewNoteSpec) (*PreparedNote, error) {
	if spec.UpdateID != "" {
		return prepareUpdateContext(ctx, root, spec)
	}
	if spec.DraftID != "" {
		return prepareDraftContext(ctx, root, spec)
	}
	if ctx == nil {
		ctx = context.Background()
	}
	plan, err := planNoteContext(ctx, root, spec, ClaimedIDsContext)
	if err != nil {
		return nil, err
	}
	for n := 1; n <= maxIDAttempts; n++ {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		id := candidateID(plan.base, n)
		if _, taken := plan.claimed[id]; taken {
			continue
		}
		path := filepath.Join(plan.dir, id+".md")
		if _, err := os.Lstat(path); err == nil {
			continue
		} else if !errors.Is(err, os.ErrNotExist) {
			return nil, err
		}
		plan.fm.ID = id
		content, err := renderNote(plan.fm, spec.By)
		if err != nil {
			return nil, err
		}
		if err := validateRoundTrip(content, id); err != nil {
			return nil, err
		}
		return &PreparedNote{
			Result:  CreateResult{Path: path, ID: id, When: plan.date, TODOs: plan.todos, Revision: ContentRevision([]byte(content))},
			Content: []byte(content),
		}, nil
	}
	return nil, fmt.Errorf("%w: could not claim a free note id for %q after %d attempts; %d notes in this vault already hold ids starting with that slug, so give this note a more specific title",
		ErrInvalidSpec, plan.base, maxIDAttempts, maxIDAttempts)
}

func planNoteContext(ctx context.Context, root string, spec NewNoteSpec, claimedIDs func(context.Context, string) (map[string]string, error)) (*notePlan, error) {
	trace := latency.Start("note_plan", "validate")
	defer trace.End()
	if ctx == nil {
		ctx = context.Background()
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if err := RequireRoot(root); err != nil {
		return nil, err
	}
	spec, err := NormalizeSpec(spec)
	if err != nil {
		return nil, err
	}
	todos := MissingContent(spec)
	if spec.Status != "draft" && len(todos) > 0 {
		return nil, fmt.Errorf("%w: missing authored content: %s; save an incomplete note as a draft", ErrInvalidSpec, strings.Join(todos, ", "))
	}
	title := strings.TrimSpace(spec.Title)
	if title == "" {
		return nil, fmt.Errorf("%w: title is required", ErrInvalidSpec)
	}
	base := Slugify(title)
	if base == "" {
		base = fallbackIDBase
	}
	if len(base) > maxSlugLen {
		return nil, fmt.Errorf("%w: title too long: it slugs to %d characters and the filename limit is %d, so shorten the title by at least %d characters",
			ErrInvalidSpec, len(base), maxSlugLen, len(base)-maxSlugLen)
	}
	now := Now()
	date := now.Format("2006-01-02")
	fm := &Frontmatter{
		Type: spec.Type, Title: title, When: date, Created: now.UTC().Format(time.RFC3339),
		Updated:  now.UTC().Format(time.RFC3339),
		Template: spec.Template, TemplateVersion: spec.TemplateVersion,
		BodySummary: spec.Summary, Sections: spec.Sections, BlockContents: spec.Blocks,
		Collections: StringList(spec.Collections), Supersedes: normalizeLinks(spec.Supersedes),
		VerifiedAt: spec.VerifiedAt,
		Related:    normalizeLinks(spec.Related), Tags: normalizeTags(spec.Tags),
		Status: spec.Status, Severity: spec.Severity, Author: strings.TrimSpace(spec.Author),
		Agent: strings.TrimSpace(spec.Agent), Source: strings.TrimSpace(spec.Source),
		SourceURL: strings.TrimSpace(spec.SourceURL), Confidence: strings.TrimSpace(spec.Confidence),
		ReviewBy: strings.TrimSpace(spec.ReviewBy), ImportedAt: strings.TrimSpace(spec.ImportedAt),
		Scope: normalizeTags(spec.Scope),
	}
	for _, block := range spec.Blocks {
		fm.Blocks = append(fm.Blocks, BlockMetadata{Template: block.Template, Version: block.Version, ID: block.ID})
	}
	if fm.Agent == "" {
		fm.Agent = strings.TrimSpace(spec.By)
	}
	if fm.Agent == "" {
		fm.Agent = "mesh"
	}
	if fm.Source == "" {
		fm.Source = "manual"
		if spec.Agent != "" || spec.By != "" {
			fm.Source = "agent"
		}
	}
	trace.Phase("id_scan")
	claimed, err := claimedIDs(ctx, root)
	if err != nil {
		return nil, err
	}
	dir := DirForType(spec.Type)
	if IsDraft(fm) {
		dir = "inbox"
	}
	return &notePlan{base: base, dir: filepath.Join(root, dir), date: date, fm: fm, claimed: claimed, todos: todos}, nil
}

// removeNoteClaim withdraws an O_EXCL publication only after its file has been closed.
// Callers never continue after a failed removal: doing so would leave a note holding an
// id for which no success receipt was returned. The directory sync mirrors publication's
// sync and makes the removal durable before cancellation reaches the caller.
func removeNoteClaim(path, dir string) error {
	err := os.Remove(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("remove unpublished note claim: %w", err)
	}
	syncDir(dir)
	return nil
}

// maxIDAttempts bounds the suffix search so a pathological directory cannot spin forever.
const maxIDAttempts = 1000

func orTODO(s string) string {
	if strings.TrimSpace(s) == "" {
		return "TODO"
	}
	return s
}

func normalizeLinks(in []string) StringList {
	var out StringList
	for _, s := range in {
		s = strings.TrimSpace(strings.Trim(strings.TrimSpace(s), "[]"))
		if s != "" {
			out = append(out, s)
		}
	}
	return out
}

func normalizeTags(in []string) StringList {
	var out StringList
	seen := map[string]bool{}
	for _, s := range in {
		s = strings.ToLower(strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(s), "#")))
		if s != "" && !seen[s] {
			seen[s] = true
			out = append(out, s)
		}
	}
	return out
}

// validateRoundTrip re-parses a freshly rendered note to prove its frontmatter is
// valid YAML and the id survives the round trip. Broken frontmatter would make the
// note invisible to search and the graph with no warning, so a note that fails this
// is never written to disk.
func validateRoundTrip(content, wantID string) error {
	fmStr, _, had := SplitFrontmatter(content)
	if !had {
		return fmt.Errorf("rendered note has no frontmatter block")
	}
	parsed, _, err := ParseFrontmatter([]byte(fmStr))
	if err != nil {
		return fmt.Errorf("rendered note has invalid frontmatter (would be dropped by the index): %w", err)
	}
	if parsed.ID != wantID {
		return fmt.Errorf("rendered note id %q does not round-trip (want %q)", parsed.ID, wantID)
	}
	return nil
}

func renderNote(fm *Frontmatter, by string) (string, error) {
	y, err := yaml.Marshal(fm)
	if err != nil {
		return "", err
	}
	var b strings.Builder
	b.WriteString("---\n")
	b.Write(y)
	b.WriteString("---\n\n# ")
	b.WriteString(fm.Title)
	b.WriteString("\n\n")
	b.WriteString(renderBody(fm))
	if by != "" && fm.Template == "" {
		b.WriteString("\n<!-- authored by ")
		b.WriteString(by)
		b.WriteString(" -->\n")
	}
	return b.String(), nil
}
