// SPDX-License-Identifier: LicenseRef-Mesh-Sustainable-Use-License
// Copyright (C) 2026 Bright Interaction AB

package vault

import (
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

// MigrateResult records what a migration pass did to one file.
type MigrateResult struct {
	Path    string
	Changed bool
	Actions []string // changes applied (or that would be, in dry-run)
	Issues  []string // flywheel fields still needing human authoring (never fabricated)
}

var wikilinkTarget = regexp.MustCompile(`\[\[([^\]|#]+)`)

// Migration is one migration pass over one vault. It carries the vault-wide note id
// claims, which is what stops the pass minting the SAME id into two files.
//
// A note id is vault-global, so nothing about one file can decide whether its id is free.
// Migrating a file used to slug its basename and write that, which meant a vault holding
// two README.md (or two index.md, or a note copied as a template) came out of
// `mesh migrate --apply` with `id: readme` cemented into both. That is not a corner case:
// it is the exact vault `mesh doctor` reports the collision on, and migrate is the remedy
// doctor points at, so running the fix made the problem permanent and doctor went on
// printing STALE over an index no reindex could change.
//
// Getting the claims requires the vault ROOT, and requiring it here is deliberate: there
// is no way to migrate a file without saying which vault it belongs to, so a future caller
// cannot reintroduce the per-file guess by forgetting an optional argument.
type Migration struct {
	ids *IDClaims
}

// NewMigration scans root once for the ids its notes already hold and returns a pass that
// can migrate any file in it. Reuse the returned Migration for every file of the pass: the
// scan is per-vault, not per-file, and claims handed out during the pass accumulate in it
// so two files migrated in the same pass cannot mint the same id either.
func NewMigration(root string) (*Migration, error) {
	ids, err := LoadIDClaims(root)
	if err != nil {
		return nil, err
	}
	return &Migration{ids: ids}, nil
}

// MigrateFile migrates a single file as a one-file pass over the vault at root. It scans
// the vault for claimed ids on every call, so a caller migrating MANY files should build
// one Migration with NewMigration and call File instead of paying for a walk per file.
func MigrateFile(root, path string, dryRun bool) (*MigrateResult, error) {
	m, err := NewMigration(root)
	if err != nil {
		return nil, err
	}
	return m.File(path, dryRun)
}

// File brings one file up to the Mesh schema, idempotently:
//   - synthesize a stable id from the filename when missing, unique across the vault,
//   - add type: note when absent (concept/map are valid, so they are kept),
//   - map updated -> when (falling back to the file mtime),
//   - lift a "## Related" section's [[links]] into a related: array.
//
// It never fabricates do/dont/why; missing flywheel fields are reported in
// Issues. With dryRun it reports without writing. Re-running is a no-op.
//
// A dry run claims ids exactly as an --apply run does, so the ids it reports are the ids
// the apply would write. A preview that named different ids than the run it previews is
// worse than no preview.
func (m *Migration) File(path string, dryRun bool) (*MigrateResult, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	// Refuse an unclosed block. Migrating it takes the !had branch below and prepends a
	// SECOND, closed block declaring type: note, demoting the author's real declaration to
	// body prose and reporting success - which turns one missing line into a note that
	// authoritatively lies about itself. writeNoteChecked cannot catch it: the output it
	// inspects has perfectly valid frontmatter. This is exactly what `mesh lint` used to
	// recommend for such a note.
	if UnterminatedFrontmatter(string(data)) {
		return nil, fmt.Errorf("%s: %w", path, ErrUnterminatedFrontmatter)
	}
	fmText, body, had := SplitFrontmatter(string(data))
	fm, raw, err := ParseFrontmatter([]byte(fmText))
	if err != nil {
		return nil, err
	}
	res := &MigrateResult{Path: path}
	add := &yaml.Node{Kind: yaml.MappingNode}

	// declaredKey, not key presence: main's rule, and the stricter one. A bare `id:`
	// (or any null spelling) is a key that is present but declares no value, and such
	// a note needs an id written just as much as one with no key at all.
	if !declaredKey(raw, "id") {
		// The basename is only the STARTING point. Claim decides what actually gets
		// written, stepping to the next suffix when another note in the vault already
		// holds the slug, so the second README.md of a vault becomes readme-2 instead of
		// a second note claiming to be readme.
		id, cerr := m.ids.Claim(path, Slugify(strings.TrimSuffix(filepath.Base(path), filepath.Ext(path))))
		if cerr != nil {
			return nil, fmt.Errorf("%s: %w", path, cerr)
		}
		if err := addKey(add, "id", id); err != nil {
			return nil, err
		}
		res.Actions = append(res.Actions, "added id "+id)
	}
	if !declaredKey(raw, "type") {
		if err := addKey(add, "type", string(TypeNote)); err != nil {
			return nil, err
		}
		res.Actions = append(res.Actions, "added type note")
	}
	if !declaredKey(raw, "when") {
		when, src := strings.TrimSpace(fm.Updated), "updated"
		if when == "" {
			when, src = fileMtimeDate(path), "file mtime"
		}
		if err := addKey(add, "when", when); err != nil {
			return nil, err
		}
		res.Actions = append(res.Actions, "set when from "+src)
	}
	if !declaredKey(raw, "related") {
		if links := relatedLinks(body); len(links) > 0 {
			if err := addKey(add, "related", links); err != nil {
				return nil, err
			}
			res.Actions = append(res.Actions, "lifted "+strconv.Itoa(len(links))+" related links")
		}
	}

	res.Issues = append(res.Issues, ReadLegacy(fm).Missing()...)

	if len(add.Content) == 0 {
		return res, nil // already clean: idempotent no-op
	}
	res.Changed = true
	if dryRun {
		return res, nil
	}

	newFM, err := mergeFrontmatter(add, fmText)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	out := "---\n" + newFM + "\n---\n" + body
	if !had {
		out = "---\n" + newFM + "\n---\n\n" + body
	}
	if err := writeNoteChecked(path, matchEOL(out, string(data))); err != nil {
		return nil, err
	}
	return res, nil
}

// matchEOL rewrites out's line endings to the ones src is written with. Every block this
// file encodes is built with \n while the body keeps the author's bytes, so a CRLF note came
// back with LF markers and an LF frontmatter block glued onto CRLF prose: line endings no
// author wrote, on a shape the repo deliberately preserves (SPEC 6.5, internal/merge). It is
// also what fed the section bug below, since a half-converted note has some CRLF headings
// and some LF ones.
//
// An LF note is returned byte for byte, so a lone \r a note carries as content is never
// promoted into a line ending.
func matchEOL(out, src string) string {
	crlf := strings.Count(src, "\r\n")
	if crlf == 0 || crlf <= strings.Count(src, "\n")-crlf {
		return out
	}
	return strings.ReplaceAll(strings.ReplaceAll(out, "\r\n", "\n"), "\n", "\r\n")
}

// BackfillScopeFile stamps `scope: <scope>` onto a note that declares no scope yet,
// idempotently (a note that already has a scope is left untouched). Unlabeled notes
// already behave as dev by the EffectiveScopes fail-safe; this just makes that
// explicit so a note can be deliberately relabeled into another scope later.
func BackfillScopeFile(path, scope string, dryRun bool) (*MigrateResult, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	// Same refusal as MigrateFile: this writer carries the identical !had branch, so it
	// corrupts an unclosed-frontmatter note the same way. Fixing only one of the two is
	// the half-migration this repo keeps paying for.
	if UnterminatedFrontmatter(string(data)) {
		return nil, fmt.Errorf("%s: %w", path, ErrUnterminatedFrontmatter)
	}
	fmText, body, had := SplitFrontmatter(string(data))
	_, raw, err := ParseFrontmatter([]byte(fmText))
	if err != nil {
		return nil, err
	}
	res := &MigrateResult{Path: path}
	// declaredKey, not key presence: a bare `scope:` (or any null spelling, or `scope: ""`)
	// counted as labeled, so the backfill reported an idempotent no-op on a note that never
	// carried a scope at all and EffectiveScopes stayed on the dev fail-safe forever.
	if declaredKey(raw, "scope") {
		return res, nil // already labeled: idempotent no-op
	}
	if strings.TrimSpace(scope) == "" {
		scope = DefaultScope
	}
	res.Changed = true
	res.Actions = append(res.Actions, "added scope "+scope)
	if dryRun {
		return res, nil
	}
	add := &yaml.Node{Kind: yaml.MappingNode}
	if err := addKey(add, "scope", scope); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	newFM, err := mergeFrontmatter(add, fmText)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	out := "---\n" + newFM + "\n---\n" + body
	if !had {
		out = "---\n" + newFM + "\n---\n\n" + body
	}
	if err := writeNoteChecked(path, matchEOL(out, string(data))); err != nil {
		return nil, err
	}
	return res, nil
}

// BackfillRelatedFile adds `related:` entries to a note that carries none, so an
// orphaned note joins the graph instead of floating beside it. Idempotent in the way
// that matters: a note that already declares `related` is left alone, because an
// author's own links are the ground truth and a derived list must never overwrite them.
//
// It goes through writeNoteChecked like every other writer here: a rewritten block
// that will not parse removes the note from search and the graph silently, which is a
// strictly worse outcome than the disconnection this is trying to fix.
func BackfillRelatedFile(path string, related []string, dryRun bool) (*MigrateResult, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	fmText, body, had := SplitFrontmatter(string(data))
	if !had {
		return nil, noFrontmatterErr(path, string(data))
	}
	_, raw, err := ParseFrontmatter([]byte(fmText))
	if err != nil {
		return nil, err
	}
	res := &MigrateResult{Path: path}
	// Anything the author actually wrote under `related` is the ground truth and a derived
	// list must never overwrite it. That INCLUDES `related: []`, which is how a note records
	// that its links were considered and there are none. A bare `related:` is not that, and
	// declaredKey is where the two are told apart; 1105 of the live vault's 1366 notes carry
	// the bare key, so this is nearly every note `mesh structure --wire-orphans` exists to
	// reach.
	if declaredKey(raw, "related") {
		return res, nil
	}
	var clean []string
	seen := map[string]bool{}
	for _, r := range related {
		if r = strings.TrimSpace(r); r != "" && !seen[r] {
			seen[r] = true
			clean = append(clean, r)
		}
	}
	if len(clean) == 0 {
		return res, nil
	}
	res.Changed = true
	res.Actions = append(res.Actions, fmt.Sprintf("added %d related link(s)", len(clean)))
	if dryRun {
		return res, nil
	}
	add := &yaml.Node{Kind: yaml.MappingNode}
	if err := addKey(add, "related", clean); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	newFM, err := mergeFrontmatter(add, fmText)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	out := "---\n" + newFM + "\n---\n" + body
	if err := writeNoteChecked(path, matchEOL(out, string(data))); err != nil {
		return nil, err
	}
	return res, nil
}

// declaredKey reports whether a note actually declares a value under key. Key PRESENCE is
// not a declaration: a bare `related:` parses to nil, and so does every null spelling (~,
// null, Null, NULL), while `id: ""` parses to a blank string. Each of those is an empty
// scaffold line rather than the author saying something, and reading presence as a value got
// it backwards on four keys of MigrateFile, on BackfillScopeFile and on BackfillRelatedFile:
// the accidental empty key counted as a deliberate decision, while the deliberate
// `related: []` ("I looked, there are none") was the one shape a backfill would overwrite.
// An empty list or map IS a claim and stays declared, which is the whole distinction. Same
// nil-versus-empty trap as a zero-value pgtype.Text: absent and empty are different answers
// and only one of them is an answer.
//
// Every writer here goes through this one predicate. Four keys got the same question wrong
// independently and a fifth was fixed on its own, which is the shape of a rule that lives in
// five copies instead of one.
func declaredKey(raw map[string]any, key string) bool {
	v, ok := raw[key]
	if !ok || v == nil {
		return false
	}
	s, isString := v.(string)
	return !isString || strings.TrimSpace(s) != ""
}

// addKey queues a key/value pair for a frontmatter block, encoding the value through yaml
// instead of formatting it into a line. A hand-built line is an injection point: a
// "## Related" wikilink lifted verbatim turned `- [[Deploy: staging]]` into a nested mapping,
// `- [[*star]]` into an unknown alias and `- [[&amp]]` into an anchor whose value silently
// vanished, and a synthesized `when: "<value>"` closed early on any value carrying a quote.
// Every one of those made the whole block unparseable, so writeNoteChecked could only refuse
// the write and the note was reported as a migration failure on every run. The encoder quotes
// whatever needs quoting.
func addKey(m *yaml.Node, key string, value any) error {
	v := &yaml.Node{}
	if err := v.Encode(value); err != nil {
		return fmt.Errorf("frontmatter %s: %w", key, err)
	}
	m.Content = append(m.Content, &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: key}, v)
	return nil
}

// mergeFrontmatter renders the keys a writer is adding and puts them in front of the block
// the note already has, after DELETING any entry those keys already occupy.
//
// The delete goes through the mapping rather than through a line match, because a
// line-editing dropper and the nil test that decides to edit are two readers of the same
// YAML and they drifted: the test fired on `related: ~` while the dropper only ever removed
// a line with nothing after the colon, so the note came out declaring `related` twice,
// yaml.v3 refuses a duplicate mapping key outright, and the note could never be wired. There
// is only one reader now, so the two cannot disagree again.
func mergeFrontmatter(add *yaml.Node, fmText string) (string, error) {
	keys := make([]string, 0, len(add.Content)/2)
	for i := 0; i < len(add.Content); i += 2 {
		keys = append(keys, add.Content[i].Value)
	}
	rest, err := dropTopLevelKeys(fmText, keys...)
	if err != nil {
		return "", err
	}
	block, err := encodeFrontmatter(add)
	if err != nil {
		return "", err
	}
	if rest != "" {
		block += "\n" + rest
	}
	return block, nil
}

// dropTopLevelKeys removes each key from a frontmatter block's top-level mapping, whatever
// value shape it carries, and returns the block re-encoded. It returns the input untouched
// when it removes nothing, so a note that only GAINS keys keeps the author's own bytes,
// comments and formatting exactly as written.
func dropTopLevelKeys(fmText string, keys ...string) (string, error) {
	if strings.TrimSpace(fmText) == "" {
		return fmText, nil
	}
	var doc yaml.Node
	if err := yaml.Unmarshal([]byte(fmText), &doc); err != nil {
		return "", fmt.Errorf("frontmatter: %w", err)
	}
	if len(doc.Content) == 0 || doc.Content[0].Kind != yaml.MappingNode {
		return fmText, nil
	}
	m := doc.Content[0]
	drop := make(map[string]bool, len(keys))
	for _, k := range keys {
		drop[k] = true
	}
	kept := make([]*yaml.Node, 0, len(m.Content))
	for i := 0; i+1 < len(m.Content); i += 2 {
		if drop[m.Content[i].Value] {
			continue
		}
		kept = append(kept, m.Content[i], m.Content[i+1])
	}
	if len(kept) == len(m.Content) {
		return fmText, nil
	}
	if len(kept) == 0 {
		return "", nil
	}
	m.Content = kept
	return encodeFrontmatter(m)
}

// encodeFrontmatter renders a mapping node as a frontmatter block, at the four-space indent
// the vault's notes already use.
func encodeFrontmatter(m *yaml.Node) (string, error) {
	var b strings.Builder
	enc := yaml.NewEncoder(&b)
	enc.SetIndent(4)
	if err := enc.Encode(m); err != nil {
		return "", fmt.Errorf("frontmatter: %w", err)
	}
	if err := enc.Close(); err != nil {
		return "", fmt.Errorf("frontmatter: %w", err)
	}
	return strings.TrimRight(b.String(), "\n"), nil
}

// BackfillBodyFile is a retired compatibility entry point. Copying shorthand into
// semantic headings can turn a recommendation into an unsupported cause or impact.
// Conversion now requires a purpose-specific candidate and an exact reviewed preview.
func BackfillBodyFile(path string, dryRun bool) (*MigrateResult, error) {
	return &MigrateResult{Path: path}, fmt.Errorf("%w: automatic body backfill is retired; use mesh templates migration-preview and review the reconstructed content", ErrInvalidSpec)
}

// fillBodySections projects each section's mapped frontmatter field into the body, and
// reports which headings it touched. It is shared by the create path (renderBody) and the
// backfill (BackfillBodyFile) so a page written today and a page repaired years later end
// up byte-identical; they had no reason to agree before, and a projection that only ran at
// create time is exactly how 18 live pages kept a skeleton body forever.
func fillBodySections(body string, fm *Frontmatter, sections []bodySection) (string, []string) {
	newBody := body
	var filled []string
	for _, s := range sections {
		if s.field == nil {
			continue
		}
		v := strings.TrimSpace(s.field(fm))
		if Unfilled(v) {
			continue // frontmatter itself has nothing real to offer yet
		}
		// A section can be in one of three states, and only the first two may be touched.
		// Absent, so the note predates this heading existing (every gotcha and post-mortem
		// written before Related was added to their templates). Present but still holding a
		// placeholder, which includes placeholders this template used to emit and no longer
		// does. Or present and authored, which is judgment and is never overwritten.
		h, end, ok := findSection(newBody, s.heading)
		if !ok {
			newBody = appendSection(newBody, s.heading, v)
			filled = append(filled, s.heading+" (added)")
			continue
		}
		// A section holding none of its placeholders is authored, and authored is judgment:
		// it is left exactly as written, even where it disagrees with the frontmatter field.
		for _, ph := range append([]string{s.placeholder}, retiredPlaceholders[s.heading]...) {
			if !opensWithPlaceholder(newBody[h.content:end], ph) {
				continue
			}
			newBody = newBody[:h.content] + v + newBody[h.content+len(ph):]
			filled = append(filled, s.heading)
			break
		}
	}
	return newBody, filled
}

// Commit is one resolved commit behind an id a note mentions. Times and subjects come
// from the repository, never from the note, so a wrong id in the prose cannot invent a
// timeline entry: it simply does not resolve.
type Commit struct {
	ID      string
	When    time.Time
	Subject string
}

// CommitResolver looks an id up in a repository. It returns ok=false for anything that is
// not a real commit, which is also what filters the many hex-looking words in prose
// ("deadbeef", "0e5f4a1 in the log", a truncated hash that no longer exists).
type CommitResolver func(id string) (Commit, bool)

var (
	shaLike  = regexp.MustCompile(`\b[0-9a-f]{7,40}\b`)
	isoDate  = regexp.MustCompile(`\b20[0-9]{2}-[01][0-9]-[0-3][0-9]\b`)
	whatHapp = "## What happened\n" + whatHappenedPlaceholder
)

const whatHappenedPlaceholder = "<!-- TODO: the incident, plainly -->"

// BackfillTimelineFile fills a post-mortem's "What happened" section from evidence the
// note already carries, and from nothing else.
//
// That section is the one part of a post-mortem no frontmatter field feeds, so it stayed a
// TODO on 88 notes: do/dont/why carry judgments (a recommendation, a warning, a reason),
// not a chronology. Reconstructing the narrative from those three would be invention. What
// IS reconstructible, and verifiable, is when things happened: the note's own recorded
// date, the commit ids it names resolved against the repository for their real committer
// time and subject, and the other dates its prose refers to. That is what goes in.
//
// Only a section still holding its exact placeholder is touched, so an authored "What
// happened" is never overwritten, and the result no longer matches the placeholder, so
// re-running is a no-op. resolve may be nil, in which case the section is built from dates
// alone.
func BackfillTimelineFile(path string, resolve CommitResolver, dryRun bool) (*MigrateResult, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	fmText, body, had := SplitFrontmatter(string(data))
	if !had {
		return nil, noFrontmatterErr(path, string(data))
	}
	fm, _, err := ParseFrontmatter([]byte(fmText))
	if err != nil {
		return nil, err
	}
	res := &MigrateResult{Path: path}
	if fm.Type != TypePostMortem || !strings.Contains(body, whatHapp) {
		return res, nil
	}

	section := buildTimeline(string(data), fm, resolve)
	if section == "" {
		return res, nil
	}
	newBody := strings.Replace(body, whatHapp, "## What happened\n"+section, 1)
	res.Changed = true
	res.Actions = append(res.Actions, "built What happened from the note's own dates and commit ids")
	if dryRun {
		return res, nil
	}
	if err := writeNoteChecked(path, matchEOL("---\n"+fmText+"\n---\n"+newBody, string(data))); err != nil {
		return nil, err
	}
	return res, nil
}

// buildTimeline renders the section. Everything in it is either quoted from the note's
// frontmatter or read out of the repository; there is no sentence here that asserts
// anything the sources do not.
func buildTimeline(whole string, fm *Frontmatter, resolve CommitResolver) string {
	var commits []Commit
	if resolve != nil {
		seen := map[string]bool{}
		for _, id := range shaLike.FindAllString(whole, -1) {
			if seen[id] {
				continue
			}
			seen[id] = true
			if c, ok := resolve(id); ok {
				commits = append(commits, c)
			}
		}
		sort.Slice(commits, func(i, j int) bool { return commits[i].When.Before(commits[j].When) })
	}

	// Dedupe commits that resolve to the same object through a short and a long id.
	var uniq []Commit
	byFull := map[string]bool{}
	for _, c := range commits {
		if byFull[c.ID] {
			continue
		}
		byFull[c.ID] = true
		uniq = append(uniq, c)
	}
	commits = uniq

	recorded := strings.TrimSpace(fm.When)
	created := strings.TrimSpace(fm.Created)
	if recorded == "" {
		recorded = created
	}

	// A date is only worth listing if it is not already on screen: not the date this note
	// records, and not one a commit row already carries. Without this the thinnest notes
	// came out saying "Recorded 2026-07-28" and then "Other dates this note refers to:
	// 2026-07-28", which reads like a bug.
	covered := map[string]bool{recorded: true, created: true}
	for _, c := range commits {
		covered[c.When.Format("2006-01-02")] = true
	}
	var others []string
	seenDate := map[string]bool{}
	for _, d := range isoDate.FindAllString(whole, -1) {
		if seenDate[d] || covered[d] {
			continue
		}
		seenDate[d] = true
		others = append(others, d)
	}
	sort.Strings(others)

	var b strings.Builder
	if recorded != "" {
		b.WriteString("Recorded " + recorded)
		if created != "" && created != recorded {
			b.WriteString(" (note created " + created + ")")
		}
		if a := strings.TrimSpace(fm.Agent); a != "" {
			b.WriteString(" by " + a)
		}
		b.WriteString(".\n")
	}
	if len(commits) > 0 {
		b.WriteString("\nThe commits this note names, with the times and subjects git has for them:\n\n")
		b.WriteString("| committed | commit | subject |\n|---|---|---|\n")
		for _, c := range commits {
			b.WriteString("| " + c.When.Format("2006-01-02 15:04 -07:00") + " | `" + c.ID + "` | " + escapePipes(c.Subject) + " |\n")
		}
	}
	if len(others) > 0 {
		b.WriteString("\nOther dates this note refers to: " + strings.Join(others, ", ") + ".\n")
	}
	if b.Len() == 0 {
		return ""
	}
	if len(commits) == 0 && len(others) == 0 {
		b.WriteString("\nThis note names no commit ids and no other dates, so that one timestamp is the " +
			"whole of the recoverable chronology. The incident narrative was never written down here, " +
			"and it is not reconstructible from the note's own fields, so it is absent rather than " +
			"reconstructed.\n")
		return b.String()
	}
	b.WriteString("\nAssembled from this note's frontmatter, the commit ids in its own text resolved " +
		"against the repository, and the dates in its prose. The incident narrative is not " +
		"recoverable from those sources and is deliberately absent rather than reconstructed.\n")
	return b.String()
}

// escapePipes keeps a commit subject from breaking the markdown table it sits in.
func escapePipes(s string) string { return strings.ReplaceAll(s, "|", `\|`) }

// repairEntityPage finishes what fillBodySections starts on an entity page, for the two
// parts of tmplEntity the section machinery cannot reach.
//
// The lead gets the first sentence of `why`, which is extraction and not authoring: an
// entity's why opens by saying what the thing is, which is exactly what the one-liner
// asks for, and FirstSentence refuses anything that does not look like a clean sentence.
//
// "How it works" and "Key facts" are DROPPED when they still hold their prompt, because
// no field feeds them and nothing ever will on an agent-written page. Keeping a heading
// whose body is a TODO comment renders an empty section forever; writing one from
// inference would be worse. The template still emits both for `mesh new`.
func repairEntityPage(body string, fm *Frontmatter) (string, []string) {
	// Both repairs are for a page that HAS content: an agent wrote it, its substance is in
	// why, and nothing will ever revisit it. A page with no why is a fresh `mesh new`
	// scaffold a human is about to fill, and every prompt on it is still doing its job.
	if Unfilled(strings.TrimSpace(ReadLegacy(fm).Why)) {
		return body, nil
	}
	var did []string
	if strings.Contains(body, entityLeadPlaceholder) {
		if lead := FirstSentence(ReadLegacy(fm).Why); lead != "" {
			body = strings.Replace(body, entityLeadPlaceholder, "**One-liner.** "+lead, 1)
			did = append(did, "One-liner (from why)")
		}
	}
	for _, h := range unsourcedEntityHeadings {
		if trimmed, ok := dropPlaceholderSection(body, h); ok {
			body = trimmed
			did = append(did, h+" (dropped, nothing sources it)")
		}
	}
	return body, did
}

// dropPlaceholderSection removes a "## <heading>" section whose entire content is a single
// TODO comment. It refuses anything else, so a section a human has written is never
// touched, and it never removes a heading that has real prose or nested content under it.
func dropPlaceholderSection(body, heading string) (string, bool) {
	h, end, ok := findSection(body, heading)
	if !ok {
		return body, false
	}
	inner, tail := body[h.content:end], body[end:]
	if strings.TrimSpace(inner) == "" {
		return body, false
	}
	for _, line := range strings.Split(strings.TrimSpace(inner), "\n") {
		l := strings.TrimSpace(line)
		if l == "" {
			continue
		}
		if !strings.HasPrefix(l, "<!--") || !strings.HasSuffix(l, "-->") {
			return body, false // authored content: leave the whole section alone
		}
	}
	head := strings.TrimRight(body[:h.off], "\n")
	if tail == "" {
		return head + "\n", true
	}
	return head + "\n\n" + tail, true
}

// bodyHeading is one level-2 heading of a note body: where its line starts, its text, and
// where its content starts (the first byte past the heading line).
type bodyHeading struct {
	off     int
	text    string
	content int
}

// bodyHeadings returns every level-2 heading LINE of body, in reading order. It is the one
// section locator in this file: fillBodySections, dropPlaceholderSection and
// lastTrailingRelated all ask it where a section is.
//
// They used to ask strings.Contains for "## "+heading+"\n" instead, and that needle is
// wrong three ways at once. A CRLF note ends the line "\r\n", so EVERY section read as
// absent and the backfill bolted a second copy of each one onto a note that already had
// them, silently, reporting success; CRLF is a shape this repo deliberately preserves
// (SPEC 6.5). "### Cause" CONTAINS "## Cause", so an h3 satisfied the h2 test and the
// mapped field was never projected, on exactly the older hand-edited notes the backfill
// exists to reach. And the needle matched inside a fence, so a gotcha DOCUMENTING the
// scaffold had the example in its code block rewritten while its real section stayed
// missing. relatedLinks has always read the stripped body for that last reason.
func bodyHeadings(body string) []bodyHeading {
	// Fences and comments are blanked, not removed: the stripped copy has the same bytes
	// and lines as the input, so an offset found here indexes the raw body.
	clean, _ := StripFencesAndComments(body)
	var out []bodyHeading
	for off := 0; off < len(clean); {
		line, next := clean[off:], len(clean)
		if i := strings.IndexByte(line, '\n'); i >= 0 {
			line, next = line[:i], off+i+1
		}
		// Trailing whitespace and the \r of a CRLF note are not part of the heading; a
		// deeper level is a different heading, and "## " is a prefix no h3 can carry.
		if t := strings.TrimRight(line, " \t\r"); strings.HasPrefix(t, "## ") {
			out = append(out, bodyHeading{off: off, text: strings.TrimSpace(t[len("## "):]), content: next})
		}
		off = next
	}
	return out
}

// findSection locates the "## <heading>" section: its heading line, and the offset where
// the section ends (the next level-2 heading, or the end of the body).
func findSection(body, heading string) (h bodyHeading, end int, ok bool) {
	headings := bodyHeadings(body)
	for i, cur := range headings {
		if cur.text != heading {
			continue
		}
		end = len(body)
		if i+1 < len(headings) {
			end = headings[i+1].off
		}
		return cur, end, true
	}
	return bodyHeading{}, 0, false
}

// opensWithPlaceholder reports whether a section's content opens with exactly the
// placeholder LINE. The placeholder has to be the whole first line, so prose an author
// wrote next to it is never mistaken for the prompt, and the trailing bytes of the line
// (a CRLF note's \r) stay where they are when the caller splices the field in.
func opensWithPlaceholder(content, ph string) bool {
	if ph == "" || !strings.HasPrefix(content, ph) {
		return false
	}
	rest := content[len(ph):]
	if i := strings.IndexByte(rest, '\n'); i >= 0 {
		rest = rest[:i]
	}
	return strings.TrimRight(rest, " \t\r") == ""
}

// retiredPlaceholders are placeholder texts a template USED to emit for a heading. A note
// scaffolded before the wording changed still carries the old string, and without this it
// reads as authored prose and is skipped forever, so the backfill would quietly miss
// exactly the notes that need it most: the oldest ones.
var retiredPlaceholders = map[string][]string{
	"Related": {
		"<!-- linked notes from the related: field render in the graph -->",
		// The entity/concept templates word their Related prompt differently, so without
		// this every entity page kept the prompt instead of its links.
		"<!-- link the concepts it uses and the decisions that shaped it: `[[note-id]]` (also fill related: above) -->",
		"<!-- `[[note-id]]` -->",
	},
}

// appendSection adds a heading the note never had. It goes at the end of the body but
// BEFORE any trailing provenance comment (<!-- authored by ... -->), which every
// scaffolded note carries as its last line; appending past the signature would read as an
// afterthought bolted on rather than part of the note.
//
// It also lands before a trailing "## Related", because Related is the note's closing
// link list in every template and a section bolted on after it reads as an appendix to the
// links rather than part of the note. Without this a repaired plain note came out
// Overview, Related, Do, Don't.
func appendSection(body, heading, content string) string {
	section := "## " + heading + "\n" + content + "\n"
	trimmed := strings.TrimRight(body, "\n")

	// Match the provenance line specifically, not any trailing comment: a template's last
	// line is often a placeholder comment (tmplNote closes on the Related prompt), and
	// treating that as the signature tore the Related section in half.
	tail := ""
	if i := strings.LastIndex(trimmed, "\n"+provenancePrefix); i >= 0 && strings.HasSuffix(trimmed, "-->") {
		tail = trimmed[i+1:]
		trimmed = strings.TrimRight(trimmed[:i], "\n")
	}
	if i := lastTrailingRelated(trimmed); i >= 0 {
		head := strings.TrimRight(trimmed[:i], "\n")
		rel := strings.TrimRight(trimmed[i:], "\n")
		trimmed = head + "\n\n" + strings.TrimRight(section, "\n") + "\n\n" + rel
		section = ""
	}
	out := trimmed
	if section != "" {
		out = trimmed + "\n\n" + strings.TrimRight(section, "\n")
	}
	if tail != "" {
		return out + "\n\n" + tail + "\n"
	}
	return out + "\n"
}

// provenancePrefix opens the "<!-- authored by X -->" line renderNote closes every
// scaffolded note with.
const provenancePrefix = "<!-- authored by "

// lastTrailingRelated returns the byte offset of a "## Related" heading that is the LAST
// section of body, or -1. A Related section with other headings after it is not the
// closing link list, so it is left alone.
func lastTrailingRelated(body string) int {
	headings := bodyHeadings(body)
	if len(headings) == 0 {
		return -1
	}
	last := headings[len(headings)-1]
	if last.text != "Related" {
		return -1
	}
	return last.off
}

// noFrontmatterErr names which of the two shapes a had=false file actually is. They need
// different fixes and "no frontmatter block" describes only one of them: it sends an author
// off to add a block that is already there, when the real repair is one missing closing ---.
func noFrontmatterErr(path, content string) error {
	if UnterminatedFrontmatter(content) {
		return fmt.Errorf("%s: %w", path, ErrUnterminatedFrontmatter)
	}
	return fmt.Errorf("%s: no frontmatter block; refusing to rewrite", path)
}

// writeNoteChecked is the single guarded write path for a note Mesh authors itself. It
// re-parses the rendered content and refuses to write when the frontmatter would not
// come back out, because an unparseable frontmatter block removes the note from search
// and the graph with no warning at all. CreateNote has enforced this since the drop was
// diagnosed; the migrate writers did not, so `mesh migrate` could still prepend a block
// that broke the YAML (an unquoted value carrying a colon, an existing block it merges
// with) and leave the note silently invisible. Both writers now go through it, so the
// invariant is "Mesh never writes a note that would vanish", not "CreateNote does not".
func writeNoteChecked(path, content string) error {
	fmStr, _, had := SplitFrontmatter(content)
	if !had {
		return fmt.Errorf("%s: rendered note has no frontmatter block; refusing to write a note the index would drop", path)
	}
	if _, _, err := ParseFrontmatter([]byte(fmStr)); err != nil {
		return fmt.Errorf("%s: rewritten frontmatter is invalid YAML (the index would drop this note, invisible to search and the graph): %w", path, err)
	}
	return WriteNoteAtomic(path, []byte(content))
}

// WriteNoteAtomic replaces a note's bytes durably: the content goes to a temp file in
// the same directory, is fsynced, and is then renamed over the note, after which the
// directory entry is fsynced too.
//
// It replaces a plain os.WriteFile, which was neither atomic nor durable here. WriteFile
// opens with O_TRUNC, so the note is EMPTY from the truncate until the write lands: a
// crash in that window leaves nothing behind, and a concurrent reader (the indexer, the
// sync daemon, an editor) can observe the empty or half-written file even with no crash
// at all. That is the write path behind every bulk in-place rewrite of a whole vault
// (`mesh migrate --apply`, `mesh scope backfill --apply`, `mesh structure --fill-bodies
// --apply` and `--wire-orphans --apply`), none of which takes a backup first.
//
// The rename gives crash-atomicity and the two fsyncs give durability, in that order:
// the data must reach the device BEFORE the rename publishes the name, or the rename can
// point at blocks that were never written; the directory must be fsynced AFTER, or the
// new entry itself can be lost. Exported because internal/ingest lands imported notes
// through it too, so the whole module has ONE durable note writer instead of a sixth
// hand-rolled copy that the next durability pass has to find again.
//
// The destination's current permission bits are preserved when it already exists, since
// a rename installs the temp file's mode: without this a note the author chmod'd 0600
// would come back 0644.
func WriteNoteAtomic(path string, content []byte) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	perm := os.FileMode(0o644)
	if fi, err := os.Stat(path); err == nil {
		perm = fi.Mode().Perm()
		// A rename replaces the destination without ever opening it, so temp+rename would
		// cheerfully overwrite a note the author chmod'd read-only, which the os.WriteFile
		// this replaced refused with EACCES. That refusal is the only protection an author
		// has against a bulk `--apply` pass touching a note they locked, and losing it
		// silently would be a durability fix that costs data control. Ask the destination
		// the same question a direct write would, before anything is created.
		w, err := os.OpenFile(path, os.O_WRONLY, 0)
		if err != nil {
			return err
		}
		w.Close()
	}
	tmp, err := os.CreateTemp(dir, ".mesh-note-*")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	if _, err := tmp.Write(content); err != nil {
		tmp.Close()
		os.Remove(tmpName)
		return err
	}
	// CreateTemp makes the file 0600; match the mode the note is meant to land with.
	if err := tmp.Chmod(perm); err != nil {
		tmp.Close()
		os.Remove(tmpName)
		return err
	}
	// Flush the data to the device BEFORE the rename publishes the name.
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		os.Remove(tmpName)
		return err
	}
	if err := tmp.Close(); err != nil {
		os.Remove(tmpName)
		return err
	}
	if err := os.Rename(tmpName, path); err != nil {
		os.Remove(tmpName)
		return err
	}
	syncDir(dir)
	return nil
}

// syncDir fsyncs a directory so a rename (or a freshly created note) that just landed in
// it survives a power cut. Best effort on purpose: opening or fsyncing a directory handle
// is a no-op or an error on some platforms and network filesystems (Windows cannot open
// one at all), and that is not a failed write, so the bytes still count as written and
// the refusal is only logged. The data itself is already fsynced by the caller.
func syncDir(dir string) {
	d, err := os.Open(dir)
	if err != nil {
		slog.Debug("vault: cannot open the note directory to fsync it", "dir", dir, "err", err)
		return
	}
	if err := d.Sync(); err != nil {
		slog.Debug("vault: directory fsync not supported here", "dir", dir, "err", err)
	}
	d.Close()
}

func fileMtimeDate(path string) string {
	fi, err := os.Stat(path)
	if err != nil {
		return Now().Format("2006-01-02")
	}
	return fi.ModTime().Format("2006-01-02")
}

// relatedLinks collects the [[basenames]] under a "## Related" (or "## See also")
// section, deduped and in order.
//
// It reads the same stripped body the parser reads. A raw regex over the section lifted
// [[links]] out of HTML comments and code spans, and frontmatter related: is
// authoritative, so migrate promoted commented-out guidance into a real edge that no
// parser fix can suppress: `mesh new` followed by `mesh migrate` manufactured the exact
// broken link the scaffold had just stopped shipping.
func relatedLinks(body string) []string {
	clean, _ := StripNonContent(body)
	var out []string
	seen := map[string]bool{}
	inRelated := false
	for _, line := range strings.Split(clean, "\n") {
		t := strings.TrimSpace(line)
		if strings.HasPrefix(t, "## ") {
			h := strings.ToLower(strings.TrimSpace(t[3:]))
			inRelated = h == "related" || h == "see also"
			continue
		}
		if !inRelated {
			continue
		}
		for _, m := range wikilinkTarget.FindAllStringSubmatch(line, -1) {
			name := strings.TrimSuffix(strings.TrimSpace(m[1]), ".md")
			if name != "" && !seen[name] {
				seen[name] = true
				out = append(out, name)
			}
		}
	}
	return out
}
