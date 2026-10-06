// SPDX-License-Identifier: LicenseRef-Mesh-Sustainable-Use-License
// Copyright (C) 2026 Bright Interaction AB

package index

import (
	"context"
	"database/sql"
	"encoding/json"
	"io/fs"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/bright-interaction/mesh/internal/vault"
)

// Knowledge-lifecycle health. A note's value rots silently: it cites a file that
// was deleted, or it asked to be re-checked by a date that has passed. ComputeHealth
// finds these and records them so mesh_health + the dashboard can surface a vault
// that needs tending. Contradiction findings are written by the curator (C3) into
// the same table via RecordHealth.

// HealthFinding is one lifecycle issue against a note.
type HealthFinding struct {
	NoteID string `json:"note_id"`
	Path   string `json:"path"`
	Issue  string `json:"issue"`  // dead_ref | overdue | contradiction
	Detail string `json:"detail"` // the missing ref / overdue date / partner note
}

// codePathRe matches a source-file path token (high precision: a real extension).
var codePathRe = regexp.MustCompile(`[A-Za-z0-9_./-]+\.(?:go|ts|tsx|js|jsx|svelte|astro|py)\b`)

// isChangelogNote reports whether a note is an append-only history log (the vault's
// `*-log` entities and the root `log`). Such notes deliberately record file paths as
// they were at the time, so their references going dead is expected history, not rot;
// dead_ref detection skips them so the finding stays actionable.
func isChangelogNote(id string) bool {
	id = strings.ToLower(strings.TrimSpace(id))
	if id == "log" || strings.HasSuffix(id, "-log") {
		return true
	}
	// Large vaults split append-only entity logs into numbered pages (for example,
	// `dockyard-log-p01`). Their source paths are historical by definition too, but
	// the page suffix means the old exact `-log` check missed them.
	marker := strings.LastIndex(id, "-log-p")
	if marker < 0 || marker+len("-log-p") == len(id) {
		return false
	}
	for _, r := range id[marker+len("-log-p"):] {
		if r < '0' || r > '9' {
			return false
		}
	}
	return true
}

// ComputeHealth runs ScanHealth and replaces the note_health rows for the two issue
// types it owns (it leaves contradiction rows, which the curator owns). Returns the
// findings it wrote. vaultRoot is the notes vault.
func (s *Store) ComputeHealth(vaultRoot string, now time.Time) ([]HealthFinding, error) {
	findings, err := s.ScanHealth(vaultRoot, now)
	if err != nil {
		return nil, err
	}
	// Replace dead_ref + overdue rows atomically (keep contradiction rows).
	at := now.Unix()
	err = s.Write(func(tx *sql.Tx) error {
		if _, err := tx.Exec(`DELETE FROM note_health WHERE issue IN ('dead_ref','overdue')`); err != nil {
			return err
		}
		ins, err := tx.Prepare(`INSERT INTO note_health(note_id,path,issue,detail,detected_at) VALUES(?,?,?,?,?)`)
		if err != nil {
			return err
		}
		defer ins.Close()
		for _, f := range findings {
			if _, err := ins.Exec(f.NoteID, f.Path, f.Issue, f.Detail, at); err != nil {
				return err
			}
		}
		return nil
	})
	return findings, err
}

// ScanHealth is the pass itself: the dead-ref + overdue analysis over the vault,
// returning the findings and writing NOTHING.
//
// It is separate from ComputeHealth because computing health and persisting it are two
// different rights. The analysis is a pure read (the vault's markdown plus this index's
// read side), so a read-only server serves mesh_health straight from here; recording the
// result for the dashboard stays with the single owning writer. Answering from the
// persisted rows instead would serve whatever the owner last wrote, and on a vault whose
// owner never runs the pass that is nothing at all.
func (s *Store) ScanHealth(vaultRoot string, now time.Time) ([]HealthFinding, error) {
	return s.scanHealthContext(context.Background(), vaultRoot, now)
}

func (s *Store) scanHealthContext(ctx context.Context, vaultRoot string, now time.Time) ([]HealthFinding, error) {
	codeFiles, err := s.codeFilePathsContext(ctx)
	if err != nil {
		return nil, err
	}
	notes, err := s.noteListContext(ctx)
	if err != nil {
		return nil, err
	}
	// Directories we actually index. We only call a path dead when we index its
	// directory but not the file (it moved/was deleted). A reference into a folder we
	// do not index can't be judged and must NOT be flagged, or every cross-repo or
	// illustrative filename ("Next.js", "components/X.svelte") cries wolf.
	indexedDirs := indexedDirSet(codeFiles)
	var findings []HealthFinding
	var candidates []HealthFinding
	for _, n := range notes {
		raw, err := vault.ReadFileContext(ctx, filepath.Join(vaultRoot, n.path))
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		if err != nil {
			continue
		}
		// Fences and comments only, NOT inline spans. A path inside a fenced block is a
		// command you could run ("npx vitest src/lib/utils.test.ts" in a testing guide), so
		// it is an example and not a claim about the tree. A path in backticks mid-sentence
		// is just how a filename is written in prose, and suppressing those loses the real
		// findings: doing so briefly hid a procedure step pointing at a deleted script.
		cleanBody, _ := vault.StripFencesAndComments(string(raw))
		body := []byte(cleanBody)
		// Dead source-file references (only meaningful once the code index exists).
		// Changelogs are exempt: an append-only history log records file paths as they
		// were at the time, so a reference that later points at a moved/deleted file is
		// expected history, not rot. Flagging every changelog line buries the genuine
		// cases (a live note claiming a current file that is gone), so skip them here,
		// the same high-precision spirit as the "don't judge dirs we don't index" guard.
		if len(codeFiles) > 0 && !isChangelogNote(n.id) && !n.expectDeadRefs {
			seen := map[string]bool{}
			for _, m := range codePathRe.FindAllString(string(body), -1) {
				ref := strings.TrimLeft(m, "./")
				slash := strings.LastIndexByte(ref, '/')
				if slash <= 0 { // bare filename / domain -> not a checkable path
					continue
				}
				if !dirIndexed(indexedDirs, ref[:slash]) { // a folder we don't index -> can't judge
					continue
				}
				if seen[ref] || ref == n.path {
					continue
				}
				// A path the author declared synthetic. Per-ref rather than per-note, so
				// the rest of this note's references stay checked.
				if vault.ExemptsDeadRef(n.expectDeadRefPaths, ref) {
					continue
				}
				seen[ref] = true
				if !codeFileKnown(codeFiles, ref) {
					// Candidate only. The index may simply be behind the tree, so this is
					// confirmed against the filesystem below before it becomes a finding.
					candidates = append(candidates, HealthFinding{NoteID: n.id, Path: n.path, Issue: "dead_ref", Detail: ref})
				}
			}
		}
		// Overdue review_by.
		if vault.ReviewOverdue(n.reviewBy, now) {
			findings = append(findings, HealthFinding{NoteID: n.id, Path: n.path, Issue: "overdue", Detail: "review_by " + n.reviewBy})
		}
	}
	// Confirm the candidates against the filesystem. A note citing a file the code index
	// has not seen yet is not rot: the index is built from a shared working tree whose
	// branch is whatever the last session left checked out, so it is routinely behind. On
	// the live vault this turned 25 findings into 0, because every referenced file was
	// present in main and only the index was stale. One walk, and only when there is
	// something to confirm.
	if len(candidates) > 0 {
		refs := make(map[string]bool, len(candidates))
		for _, c := range candidates {
			refs[c.Detail] = true
		}
		roots, err := s.codeRootsContext(ctx)
		if err != nil {
			return nil, err
		}
		onDisk, err := existsUnderRootsContext(ctx, roots, refs)
		if err != nil {
			return nil, err
		}
		for _, c := range candidates {
			if !onDisk[c.Detail] {
				findings = append(findings, c)
			}
		}
	}
	return findings, ctx.Err()
}

// tier0Health are the institutional types eligible for explicit guidance checks.
// Modern methods and procedures are included separately by their template.
var tier0Health = map[string]bool{"decision": true, "gotcha": true, "post-mortem": true}

// ComputeContradictions runs ScanContradictions and replaces the contradiction rows.
func (s *Store) ComputeContradictions(now time.Time) ([]HealthFinding, error) {
	findings, err := s.ScanContradictions()
	if err != nil {
		return nil, err
	}
	return findings, s.RecordHealth("contradiction", findings, now)
}

// ScanContradictions flags pairs of eligible notes that share a tag where one
// note's explicit recommendation strongly overlaps the other's explicit prohibition.
// Dependency-free heuristic (token Jaccard, high threshold to stay
// high-precision); the curator can later confirm with an LLM. Pure computation over the
// index's read side, writing nothing, for the same reason as ScanHealth.
func (s *Store) ScanContradictions() ([]HealthFinding, error) {
	return s.scanContradictionsContext(context.Background())
}

func (s *Store) scanContradictionsContext(ctx context.Context) ([]HealthFinding, error) {
	notes, err := s.tier0GuidanceContext(ctx)
	if err != nil {
		return nil, err
	}
	return contradictionFindingsContext(ctx, notes)
}

// contradictionFindings preserves directed comparison order and the one-finding
// per unordered pair policy. Tokenize each field once and visit only notes sharing
// a tag, instead of allocating token/tag maps for every pair on every health pass.
func contradictionFindings(notes []guidanceRow) []HealthFinding {
	findings, _ := contradictionFindingsContext(context.Background(), notes)
	return findings
}

func contradictionFindingsContext(ctx context.Context, notes []guidanceRow) ([]HealthFinding, error) {
	const threshold = 0.6
	recommendedTokens := make([]map[string]bool, len(notes))
	forbiddenTokens := make([]map[string]bool, len(notes))
	byTag := map[string][]int{}
	for i, n := range notes {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		recommendedTokens[i], forbiddenTokens[i] = tokenSet(n.recommended), tokenSet(n.forbidden)
		tags := map[string]bool{}
		for _, tag := range n.tags {
			tag = strings.ToLower(tag)
			if !tags[tag] {
				byTag[tag] = append(byTag[tag], i)
				tags[tag] = true
			}
		}
	}
	var findings []HealthFinding
	seen := map[string]bool{}
	visited := make([]int, len(notes))
	candidates := make([]int, 0, len(notes))
	for i := range notes {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		if len(recommendedTokens[i]) == 0 {
			continue
		}
		candidates = candidates[:0]
		for _, tag := range notes[i].tags {
			for _, j := range byTag[strings.ToLower(tag)] {
				if i != j && visited[j] != i+1 {
					visited[j] = i + 1
					candidates = append(candidates, j)
				}
			}
		}
		// Preserve which direction owns a finding when both directions qualify.
		sort.Ints(candidates)
		for _, j := range candidates {
			if ctx.Err() != nil {
				return nil, ctx.Err()
			}
			a, b := len(recommendedTokens[i]), len(forbiddenTokens[j])
			// Even complete containment cannot reach the threshold at this size ratio.
			if b == 0 || float64(min(a, b))/float64(max(a, b)) < threshold {
				continue
			}
			if jaccard(recommendedTokens[i], forbiddenTokens[j]) < threshold {
				continue
			}
			// One unordered finding per pair.
			key := pairKey(notes[i].id, notes[j].id)
			if seen[key] {
				continue
			}
			seen[key] = true
			findings = append(findings, HealthFinding{
				NoteID: notes[i].id, Path: notes[i].path, Issue: "contradiction",
				Detail: "guidance may conflict with [[" + notes[j].id + "]]",
			})
		}
	}
	return findings, ctx.Err()
}

type guidanceRow struct {
	id, path, recommended, forbidden string
	tags                             []string
}

func (s *Store) tier0Guidance() ([]guidanceRow, error) {
	return s.tier0GuidanceContext(context.Background())
}

func (s *Store) tier0GuidanceContext(ctx context.Context) ([]guidanceRow, error) {
	rows, err := s.readDB.QueryContext(ctx, `SELECT n.id, n.path, n.type, n.frontmatter, COALESCE(gn.attrs, '{}') FROM notes n LEFT JOIN nodes gn ON gn.id = 'note:' || n.id WHERE 1=1`+draftPredicate)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []guidanceRow
	for rows.Next() {
		var id, path, typ, fmJSON, attrsJSON string
		if err := rows.Scan(&id, &path, &typ, &fmJSON, &attrsJSON); err != nil {
			return nil, err
		}
		var fm vault.Frontmatter
		if json.Unmarshal([]byte(fmJSON), &fm) != nil {
			continue
		}
		if !tier0Health[typ] && fm.Template != "method" && fm.Template != "procedure" {
			continue
		}
		recommended, forbidden, _ := vault.LegacyGuidance(&fm)
		if fm.Template != "" || fm.TemplateVersion != 0 {
			var attrs struct {
				Behavior behaviorGuidance `json:"reader_behavior"`
			}
			_ = json.Unmarshal([]byte(attrsJSON), &attrs)
			recommended, forbidden = attrs.Behavior.Recommended, attrs.Behavior.Forbidden
		}
		recommended, forbidden = normalizeGuidance(recommended), normalizeGuidance(forbidden)
		if recommended == "" && forbidden == "" {
			continue
		}
		out = append(out, guidanceRow{
			id: id, path: path,
			recommended: recommended, forbidden: forbidden, tags: fm.Tags,
		})
	}
	return out, rows.Err()
}

func shareTag(a, b []string) bool {
	set := map[string]bool{}
	for _, t := range a {
		set[strings.ToLower(t)] = true
	}
	for _, t := range b {
		if set[strings.ToLower(t)] {
			return true
		}
	}
	return false
}

func tokenSet(s string) map[string]bool {
	out := map[string]bool{}
	for _, w := range strings.Fields(strings.ToLower(s)) {
		w = strings.Trim(w, ".,;:!?\"'()[]")
		if len(w) > 2 && !stopWord[w] {
			out[w] = true
		}
	}
	return out
}

// normalizeGuidance removes scaffold placeholders before contradiction analysis.
// Historical notes may still carry TODO/TBD values in their legacy guidance.
// Treating those literals as guidance would create spurious conflicts between
// otherwise unrelated incomplete notes sharing a tag.
func normalizeGuidance(s string) string {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "", "todo", "tbd", "n/a", "na", "none", "-":
		return ""
	default:
		return s
	}
}

var stopWord = map[string]bool{"the": true, "and": true, "for": true, "you": true, "use": true, "not": true, "with": true, "this": true, "that": true, "are": true, "but": true}

func jaccard(a, b map[string]bool) float64 {
	if len(a) == 0 || len(b) == 0 {
		return 0
	}
	inter := 0
	for k := range a {
		if b[k] {
			inter++
		}
	}
	union := len(a) + len(b) - inter
	if union == 0 {
		return 0
	}
	return float64(inter) / float64(union)
}

func pairKey(a, b string) string {
	if a > b {
		a, b = b, a
	}
	return a + "\x00" + b
}

// RecordHealth upserts contradiction (or any) findings without touching the
// dead_ref/overdue rows. Used by the curator's contradiction pass (C3).
func (s *Store) RecordHealth(issue string, findings []HealthFinding, now time.Time) error {
	at := now.Unix()
	return s.Write(func(tx *sql.Tx) error {
		if _, err := tx.Exec(`DELETE FROM note_health WHERE issue=?`, issue); err != nil {
			return err
		}
		ins, err := tx.Prepare(`INSERT INTO note_health(note_id,path,issue,detail,detected_at) VALUES(?,?,?,?,?)`)
		if err != nil {
			return err
		}
		defer ins.Close()
		for _, f := range findings {
			if _, err := ins.Exec(f.NoteID, f.Path, f.Issue, f.Detail, at); err != nil {
				return err
			}
		}
		return nil
	})
}

// ListHealth returns current findings (optionally filtered by issue, "" = all).
func (s *Store) ListHealth(issue string) ([]HealthFinding, error) {
	var rows *sql.Rows
	var err error
	if issue == "" {
		rows, err = s.readDB.Query(`SELECT note_id,path,issue,detail FROM note_health ORDER BY issue, note_id`)
	} else {
		rows, err = s.readDB.Query(`SELECT note_id,path,issue,detail FROM note_health WHERE issue=? ORDER BY note_id`, issue)
	}
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []HealthFinding
	for rows.Next() {
		var f HealthFinding
		if err := rows.Scan(&f.NoteID, &f.Path, &f.Issue, &f.Detail); err != nil {
			return nil, err
		}
		out = append(out, f)
	}
	return out, rows.Err()
}

// HealthCounts returns issue -> count for the dashboard.
func (s *Store) HealthCounts() (map[string]int, error) {
	return s.HealthCountsContext(context.Background())
}

// HealthCountsContext is HealthCounts with caller-controlled cancellation.
func (s *Store) HealthCountsContext(ctx context.Context) (map[string]int, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	rows, err := s.readDB.QueryContext(ctx, `SELECT issue, count(*) FROM note_health GROUP BY issue`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]int{}
	for rows.Next() {
		var issue string
		var n int
		if err := rows.Scan(&issue, &n); err != nil {
			return nil, err
		}
		out[issue] = n
	}
	return out, rows.Err()
}

type noteRow struct {
	id, path, reviewBy string
	expectDeadRefs     bool
	expectDeadRefPaths []string
}

func (s *Store) noteList() ([]noteRow, error) {
	return s.noteListContext(context.Background())
}

func (s *Store) noteListContext(ctx context.Context) ([]noteRow, error) {
	rows, err := s.readDB.QueryContext(ctx, `SELECT id, path, COALESCE(review_by,''), frontmatter FROM notes`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []noteRow
	for rows.Next() {
		var n noteRow
		var fmJSON string
		if err := rows.Scan(&n.id, &n.path, &n.reviewBy, &fmJSON); err != nil {
			return nil, err
		}
		// Frontmatter is stored as JSON, so read the one flag rather than unmarshalling
		// the whole struct for every note on every health run.
		var fmFlags struct {
			ExpectDeadRefs     bool     `json:"ExpectDeadRefs"`
			ExpectDeadRefPaths []string `json:"ExpectDeadRefPaths"`
		}
		if fmJSON != "" {
			_ = json.Unmarshal([]byte(fmJSON), &fmFlags)
		}
		n.expectDeadRefs = fmFlags.ExpectDeadRefs
		n.expectDeadRefPaths = fmFlags.ExpectDeadRefPaths
		out = append(out, n)
	}
	return out, rows.Err()
}

func (s *Store) codeFilePaths() (map[string]bool, error) {
	return s.codeFilePathsContext(context.Background())
}

func (s *Store) codeFilePathsContext(ctx context.Context) (map[string]bool, error) {
	rows, err := s.readDB.QueryContext(ctx, `SELECT path FROM code_files`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]bool{}
	for rows.Next() {
		var p string
		if err := rows.Scan(&p); err != nil {
			return nil, err
		}
		out[p] = true
	}
	return out, rows.Err()
}

// indexedDirSet returns the directory of every indexed code file (code paths may
// carry a root prefix, e.g. "<repo>/internal/foo/bar.go").
func indexedDirSet(codeFiles map[string]bool) map[string]bool {
	out := map[string]bool{}
	for p := range codeFiles {
		if i := strings.LastIndexByte(p, '/'); i > 0 {
			out[p[:i]] = true
		}
	}
	return out
}

// dirIndexed reports whether refDir names a directory we index. Because indexed
// paths may carry a root prefix the note omits, an indexed dir whose tail is the
// ref's dir counts (e.g. indexed "<repo>/internal/foo" matches ref dir "internal/foo").
func dirIndexed(indexedDirs map[string]bool, refDir string) bool {
	if indexedDirs[refDir] {
		return true
	}
	for d := range indexedDirs {
		if strings.HasSuffix(d, "/"+refDir) {
			return true
		}
	}
	return false
}

// codeFileKnown reports whether ref names a known source file. Code paths are
// indexed root-relative while a note may cite a shorter suffix, so a suffix match
// (on a path boundary) counts as known.
func codeFileKnown(codeFiles map[string]bool, ref string) bool {
	if codeFiles[ref] {
		return true
	}
	for p := range codeFiles {
		if strings.HasSuffix(p, "/"+ref) || strings.HasSuffix(ref, "/"+p) {
			return true
		}
	}
	return false
}

// setCodeRoots records the directories the code index was built from.
//
// Route this through Write like every other mutation so read-only stores and a
// preempted MCP ownership claim both fail with ErrReadOnly.
func (s *Store) setCodeRoots(roots []string) error {
	return s.Write(func(tx *sql.Tx) error {
		_, err := tx.Exec(`INSERT OR REPLACE INTO meta(key, value) VALUES('code_roots', ?)`, strings.Join(roots, "\n"))
		return err
	})
}

func (s *Store) codeRoots() []string {
	roots, _ := s.codeRootsContext(context.Background())
	return roots
}

func (s *Store) codeRootsContext(ctx context.Context) ([]string, error) {
	var v string
	if err := s.readDB.QueryRowContext(ctx, `SELECT value FROM meta WHERE key='code_roots'`).Scan(&v); err != nil {
		if err == sql.ErrNoRows {
			return nil, nil
		}
		return nil, err
	}
	var out []string
	for _, r := range strings.Split(v, "\n") {
		if r = strings.TrimSpace(r); r != "" {
			out = append(out, r)
		}
	}
	return out, nil
}

// existsUnderRoots reports which refs name a file that really exists, matched the same
// suffix way codeFileKnown matches indexed paths.
//
// This is the difference between "the file is gone" and "the index has not caught up".
// The code index is built from a shared working tree whose branch is whatever the last
// session checked out, so it is routinely behind: a note citing a file added an hour ago,
// or living on another branch, is not rot. Reporting it as rot cost 25 false findings in a
// 900-note vault, every one of which existed in main, and a health check that is wrong 25
// times out of 25 is one nobody reads.
//
// Walked lazily and once per health run, only when there is at least one candidate, so a
// clean vault pays nothing.
func existsUnderRoots(roots []string, refs map[string]bool) map[string]bool {
	found, _ := existsUnderRootsContext(context.Background(), roots, refs)
	return found
}

func existsUnderRootsContext(ctx context.Context, roots []string, refs map[string]bool) (map[string]bool, error) {
	if ctx.Err() != nil {
		return nil, ctx.Err()
	}
	if ctx.Done() == nil {
		return scanExistsUnderRoots(ctx, roots, refs)
	}
	// A stuck kernel directory read cannot be canceled. Isolate this purely
	// read-only scan, just like vault.ReadFileContext; late results are private
	// and never used to publish a partial (false dead-ref) report.
	type result struct {
		found map[string]bool
		err   error
	}
	out := make(chan result, 1)
	rootCopy := append([]string(nil), roots...)
	refCopy := make(map[string]bool, len(refs))
	for ref, value := range refs {
		refCopy[ref] = value
	}
	go func() { found, err := scanExistsUnderRoots(ctx, rootCopy, refCopy); out <- result{found, err} }()
	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	case r := <-out:
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		return r.found, r.err
	}
}

func scanExistsUnderRoots(ctx context.Context, roots []string, refs map[string]bool) (map[string]bool, error) {
	found := make(map[string]bool, len(refs))
	if len(refs) == 0 {
		return found, ctx.Err()
	}
	// Ask git as well as the filesystem. A code root is typically a SHARED working tree
	// whose branch is whatever the last session checked out, so "not on disk right now"
	// routinely means "on another branch", not "deleted". Checking the mainline too is
	// what separates the two; without it, a file added an hour ago on main reads as rot.
	for _, root := range roots {
		markGitKnownContext(ctx, root, refs, found)
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
	}
	for _, root := range roots {
		_ = filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			if err != nil {
				return nil //nolint:nilerr // an unreadable dir must not fail the health run
			}
			if d.IsDir() {
				switch d.Name() {
				case ".git", "node_modules", "vendor", "dist", "build":
					return filepath.SkipDir
				}
				return nil
			}
			slashed := filepath.ToSlash(path)
			for ref := range refs {
				if found[ref] {
					continue
				}
				if slashed == ref || strings.HasSuffix(slashed, "/"+ref) {
					found[ref] = true
				}
			}
			return nil
		})
	}
	if ctx.Err() != nil {
		return nil, ctx.Err()
	}
	return found, nil
}

// markGitKnown marks every ref that the repository at root knows about on its mainline.
//
// Consulted in addition to the working tree because a code root is usually shared between
// concurrent sessions: whichever branch was checked out last is what is on disk, and a
// file that lives on main but not on that branch is emphatically not rot. Falls back
// through the usual mainline names and does nothing at all outside a git repo, so a
// non-git code root keeps the plain filesystem behaviour.
func markGitKnown(root string, refs map[string]bool, found map[string]bool) {
	markGitKnownContext(context.Background(), root, refs, found)
}

func markGitKnownContext(ctx context.Context, root string, refs map[string]bool, found map[string]bool) {
	allFound := true
	for ref := range refs {
		if !found[ref] {
			allFound = false
			break
		}
	}
	if allFound {
		return
	}
	// Every plausible mainline, and the UNION of them, not the first that resolves. A repo
	// may have no remote (a fresh clone-less checkout), or a local main that is ahead of
	// origin, or be detached mid-rebase. Stopping at the first resolvable rev meant a repo
	// whose only mainline was a local `main` fell through to HEAD, which is exactly the
	// branch that may be missing the file.
	for _, rev := range []string{"origin/HEAD", "origin/main", "origin/master", "main", "master", "HEAD"} {
		if ctx.Err() != nil {
			return
		}
		cmd := exec.CommandContext(ctx, "git", "-C", root, "ls-tree", "-r", "--name-only", rev)
		cmd.WaitDelay = time.Second
		out, err := cmd.Output()
		if err != nil {
			continue
		}
		for _, line := range strings.Split(string(out), "\n") {
			line = strings.TrimSpace(line)
			if line == "" {
				continue
			}
			for ref := range refs {
				if found[ref] {
					continue
				}
				if line == ref || strings.HasSuffix(line, "/"+ref) {
					found[ref] = true
				}
			}
		}
	}
}
