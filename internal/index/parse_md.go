// SPDX-License-Identifier: LicenseRef-Mesh-Sustainable-Use-License
// Copyright (C) 2026 Bright Interaction AB

package index

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"unicode"

	"github.com/bright-interaction/mesh/internal/graph"
	"github.com/bright-interaction/mesh/internal/vault"
	"golang.org/x/text/unicode/norm"
)

type Heading struct {
	Level  int
	Text   string
	Anchor string
	Line   int
}

type Link struct {
	Target string
	Alias  string
	Line   int
}

type Tag struct {
	Name string
	Line int
}

// ParsedNote is the deterministic structure extracted from a single markdown
// file: frontmatter, headings, wikilinks, and tags. No reasoning AI involved.
type ParsedNote struct {
	Path     string
	Key      string // lowercased basename without extension; the wikilink key
	FM       *vault.Frontmatter
	Raw      map[string]any
	Body     string
	Headings []Heading
	Links    []Link
	Tags     []Tag
	Mtime    int64 // file modification time (unix seconds); set by ParseFile from the on-disk file, 0 for byte-only Parse
	// Issues found while reading this one file, folded into BuildGraph's list so lint,
	// doctor and health surface them next to the graph-level ones.
	Issues []Issue
}

// Issue is a non-fatal problem found while parsing or building the graph.
type Issue struct {
	Path string
	Kind string // missing-id|duplicate-id|broken-link|broken-anchor|duplicate-anchor|ambiguous-id-key|ambiguous-path-key|ambiguous-link-key|ambiguous-link|unterminated-comment|unterminated-fence
	Msg  string
}

// supersedeClaim is one source note's bid to be recorded as the superseded_by
// winner on a target that more than one note supersedes. See the resolution pass
// in BuildGraph for how ties are broken.
type supersedeClaim struct {
	srcID string
	when  string
}

func noteKey(path string) string {
	b := filepath.Base(path)
	b = strings.TrimSuffix(b, filepath.Ext(b))
	return canonicalLinkText(b)
}

func canonicalLinkText(s string) string {
	return norm.NFC.String(strings.ToLower(strings.TrimSpace(s)))
}

// linkKey normalizes a wikilink target to its lookup key: strips [[ ]], a .md
// extension, and any #heading anchor, then lowercases.
func linkKey(target string) string {
	t := strings.TrimSpace(target)
	t = strings.Trim(t, "[]")
	// A markdown-escaped closing bracket, [[note\]], leaves a trailing backslash on the
	// target, so the link resolved to nothing and showed up as a dangling reference to a
	// note whose name ended in "\". Authors escape brackets when a renderer would
	// otherwise eat them, and they mean the note, not a different one.
	t = strings.TrimRight(t, `\`)
	t = strings.TrimSpace(t)
	if i := strings.IndexByte(t, '#'); i >= 0 {
		t = t[:i]
	}
	t = canonicalLinkText(t)
	t = strings.TrimSuffix(t, ".md")
	return strings.TrimSpace(t)
}

// pathLinkKey normalizes a note's vault-relative path into the key a slash-bearing
// wikilink resolves against: forward slashes, no .md extension, lowercased. It mirrors
// linkKey so [[decisions/deploy]] and decisions/deploy.md meet on the same string.
func pathLinkKey(p string) string {
	p = strings.ReplaceAll(p, `\`, "/")
	p = canonicalLinkText(p)
	p = strings.TrimSuffix(p, ".md")
	return p
}

func ParseFile(path string) (*ParsedNote, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	pn, err := parseFileData(path, data)
	if err != nil {
		return nil, err
	}
	setParsedNoteMtime(pn, statFile(path))
	return pn, nil
}

// ParseFileContext is ParseFile with a caller-owned filesystem wait. The parsing work
// is local and deterministic; cancellation is checked again before and after it so a
// cancelled indexing pass never receives a partially parsed note.
func ParseFileContext(ctx context.Context, path string) (*ParsedNote, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	data, err := vault.ReadFileContext(ctx, path)
	if err != nil {
		return nil, err
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	pn, err := parseFileData(path, data)
	if err != nil {
		return nil, err
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	info, statErr := vault.StatContext(ctx, path)
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if statErr == nil {
		pn.Mtime = info.ModTime().Unix()
	}
	return pn, nil
}

func parseFileData(path string, data []byte) (*ParsedNote, error) {
	return Parse(path, data)
}

func statFile(path string) os.FileInfo {
	info, _ := os.Stat(path)
	return info
}

func setParsedNoteMtime(pn *ParsedNote, info os.FileInfo) {
	// Capture mtime from the file we just read (path is the real, usually absolute,
	// path here), so the stored mtime is correct regardless of the process CWD. The
	// old fileMtime(pn.Path) stat'd the vault-relative path and returned 0 whenever
	// mesh ran outside the vault root - which is the normal MCP case (an agent spawns
	// `mesh mcp --vault <abs>` from its own dir) - silently breaking mesh_changed_since.
	if info != nil {
		pn.Mtime = info.ModTime().Unix()
	}
}

// Parse extracts the deterministic structure of a markdown document. Wikilinks
// and tags inside fenced code blocks, inline code spans, and HTML comments are
// ignored.
func Parse(path string, data []byte) (*ParsedNote, error) {
	// Refuse an unclosed block instead of indexing the note with defaults. Left alone,
	// SplitFrontmatter hands back the whole file as body, ParseFrontmatter stamps
	// type: note onto an empty struct, and Parse returns no error at all - so the note
	// never reaches FileError, never reaches DroppedNotes, and never reaches mesh health.
	// A decision indexed as an untagged, unlinked note is worse than one that refuses to
	// index, because nothing anywhere says it went wrong.
	if vault.UnterminatedFrontmatter(string(data)) {
		return nil, fmt.Errorf("frontmatter: %w", vault.ErrUnterminatedFrontmatter)
	}
	fmText, body, _ := vault.SplitFrontmatter(string(data))
	fm, raw, err := vault.ParseFrontmatter([]byte(fmText))
	if err != nil {
		return nil, err
	}
	if strings.ContainsRune(fm.ID, '#') {
		return nil, fmt.Errorf("frontmatter: invalid id %q: # is reserved for heading anchors", fm.ID)
	}
	pn := &ParsedNote{Path: path, Key: noteKey(path), FM: fm, Raw: raw, Body: body}

	// An HTML comment is not content: what it hides is invisible to every reader, so the
	// wikilinks and tags inside one are not real. Fences, code spans and comments are all
	// resolved in one pass (vault.StripNonContent) because they can only be told apart
	// together, and the same pass feeds the FTS body, the embedding chunks and migrate,
	// so every part of Mesh agrees on what a note says.
	clean, openComment := vault.StripNonContent(body)
	headingText, _ := vault.StripFencesAndComments(body)
	if openComment > 0 {
		// Everything below an unterminated marker drops out of the graph AND out of
		// search. That used to happen silently, which is the worst version of it: the note
		// looks indexed and half of it simply is not there.
		pn.Issues = append(pn.Issues, Issue{path, "unterminated-comment",
			"<!-- on line " + strconv.Itoa(openComment) + " is never closed, so the rest of the note is hidden from the graph and from search; close it with -->"})
	}
	if vault.UnterminatedFence(body) {
		pn.Issues = append(pn.Issues, Issue{path, "unterminated-fence",
			"a code fence is opened and never closed in the body, so everything below it is hidden from the graph and from search; close the fence"})
	}
	for _, name := range []string{"do", "dont", "why"} {
		field := struct{ name, text string }{name, vault.ReadLegacy(fm).Values()[name]}
		if field.text == "" {
			continue
		}
		if _, open := vault.StripNonContent(field.text); open > 0 {
			pn.Issues = append(pn.Issues, Issue{path, "unterminated-comment",
				"<!-- in frontmatter " + field.name + " is never closed, so the rest of that field is hidden from the graph and from search; close it with -->"})
		}
		if vault.UnterminatedFence(field.text) {
			pn.Issues = append(pn.Issues, Issue{path, "unterminated-fence",
				"a code fence is opened and never closed in the " + field.name + ": frontmatter field" +
					", so everything below it is hidden from the graph and from search; close the fence"})
		}
	}
	cleanLines := strings.Split(clean, "\n")
	headingLines := strings.Split(headingText, "\n")
	blockAnchors := vault.AuthoredHeadingAnchors(pn.FM, pn.Body)
	for i, line := range cleanLines {
		ln := i + 1
		if h, ok := parseHeadingViews(line, headingLines[i]); ok {
			h.Line = ln
			if address := blockAnchors[ln]; address != "" {
				h.Anchor = address
			}
			pn.Headings = append(pn.Headings, h)
			pn.appendLinks(line, ln)
			continue
		}
		pn.appendLinks(line, ln)
		pn.appendTags(line, ln)
	}
	return pn, nil
}

func (pn *ParsedNote) appendLinks(line string, ln int) {
	for searchFrom := 0; searchFrom < len(line); {
		relOpen := strings.Index(line[searchFrom:], "[[")
		if relOpen < 0 {
			return
		}
		open := searchFrom + relOpen
		if markdownEscaped(line, open) {
			searchFrom = open + 2
			continue
		}
		relClose := strings.Index(line[open+2:], "]]")
		if relClose < 0 {
			return
		}
		close := open + 2 + relClose
		inner := line[open+2 : close]
		target, alias := inner, ""
		if p := strings.IndexByte(inner, '|'); p >= 0 {
			target, alias = inner[:p], inner[p+1:]
		}
		if t := strings.TrimSpace(target); t != "" {
			pn.Links = append(pn.Links, Link{Target: t, Alias: strings.TrimSpace(alias), Line: ln})
		}
		searchFrom = close + 2
	}
}

// markdownEscaped reports whether the punctuation at pos is preceded by an odd run of
// backslashes. Markdown consumes backslash escapes in pairs: \[[x]] is literal syntax,
// while \\[[x]] leaves one literal backslash followed by a real wikilink.
func markdownEscaped(line string, pos int) bool {
	backslashes := 0
	for pos > 0 && line[pos-1] == '\\' {
		backslashes++
		pos--
	}
	return backslashes%2 == 1
}

func (pn *ParsedNote) appendTags(line string, ln int) {
	runes := []rune(line)
	for i := 0; i < len(runes); i++ {
		if runes[i] != '#' {
			continue
		}
		if i > 0 && !isSpace(runes[i-1]) {
			continue
		}
		j := i + 1
		for j < len(runes) && isTagRune(runes[j]) {
			j++
		}
		if j > i+1 && isLetter(runes[i+1]) {
			pn.Tags = append(pn.Tags, Tag{Name: canonicalLinkText(string(runes[i+1 : j])), Line: ln})
		}
		i = j
	}
}

func parseHeading(line string) (Heading, bool) {
	return parseHeadingViews(line, line)
}

func parseHeadingViews(markerLine, textLine string) (Heading, bool) {
	h, ok := vault.ParseATXHeading(markerLine, textLine)
	if !ok {
		return Heading{}, false
	}
	return Heading{Level: h.Level, Text: h.Text, Anchor: h.Anchor}, true
}

func isSpace(r rune) bool  { return unicode.IsSpace(r) }
func isLetter(r rune) bool { return unicode.IsLetter(r) }
func isTagRune(r rune) bool {
	return unicode.IsLetter(r) || unicode.IsDigit(r) || unicode.IsMark(r) || r == '-' || r == '_' || r == '/'
}

// BuildGraph resolves a set of parsed notes into the in-memory graph. Node
// identity is the frontmatter id (falling back to the filename, with an issue
// raised so `mesh migrate` can fix it). Wikilinks resolve by basename to the
// target note's id, so edges survive a file rename (spec section 3.6).
func BuildGraph(notes []*ParsedNote) (*graph.Graph, []Issue) {
	g, issues, _ := BuildGraphContext(context.Background(), notes)
	return g, issues
}

// BuildGraphContext is BuildGraph with cooperative cancellation. The graph remains
// private until this function returns, so cancellation discards it rather than exposing
// a partially-resolved graph to a caller that might persist or serve it.
func BuildGraphContext(ctx context.Context, notes []*ParsedNote) (*graph.Graph, []Issue, error) {
	if err := buildGraphContextCause(ctx); err != nil {
		return nil, nil, err
	}
	g := graph.NewSized(len(notes))
	var issues []Issue

	idByKey := make(map[string]string, len(notes))
	// A note's frontmatter id is the stable fallback when its basename changes. The
	// normal basename/path lookup still wins, preserving wikilink semantics; the id
	// fallback is what makes already-resolved citations survive a rename as promised.
	idByStableKey := make(map[string]string, len(notes))
	pathByID := make(map[string]string, len(notes))
	// A wikilink key is the lowercased basename, so two notes in different folders that
	// share a basename (with distinct frontmatter ids) collide on it. A plain
	// last-wins assignment handed the key to whichever note happened to be walked last,
	// which then absorbed EVERY [[basename]] and related: edge in the vault while the
	// other note silently received none, and no Issue was raised so nothing surfaced it.
	// Track the owning path per key so a genuine collision is reported and refused
	// rather than resolved to an arbitrary winner.
	pathByKey := make(map[string]string, len(notes))
	ambiguousKeys := map[string][]string{}
	pathByStableKey := make(map[string]string, len(notes))
	ambiguousStableKeys := map[string][]string{}
	anchorsByID := make(map[string]map[string]bool, len(notes))
	// idByPath resolves a wikilink that names a path (contains a slash): the author has
	// disambiguated explicitly, so an exact vault-relative path match wins over the
	// basename key. This is also the escape hatch out of an ambiguous key.
	idByPath := make(map[string]string, len(notes))
	pathByPathKey := make(map[string]string, len(notes))
	ambiguousPathKeys := map[string][]string{}
	for _, n := range notes {
		if err := buildGraphContextCause(ctx); err != nil {
			return nil, nil, err
		}
		id := effectiveID(n)
		stableKey := canonicalLinkText(id)
		issues = append(issues, n.Issues...) // what Parse found in the file itself
		if n.FM.ID == "" {
			issues = append(issues, Issue{n.Path, "missing-id", "no frontmatter id; using filename (run mesh migrate)"})
		}
		if prev, ok := pathByID[id]; ok && prev != n.Path {
			issues = append(issues, Issue{n.Path, "duplicate-id", "id " + id + " already used by " + prev})
		}
		// Only a collision between DIFFERENT ids is ambiguous. Two un-id'd files sharing
		// a basename resolve to the same effectiveID, which is the duplicate-id case
		// above and collapses to one node, so the key still points somewhere correct.
		if prevID, ok := idByKey[n.Key]; ok && prevID != id {
			if len(ambiguousKeys[n.Key]) == 0 {
				ambiguousKeys[n.Key] = []string{pathByKey[n.Key]}
			}
			ambiguousKeys[n.Key] = append(ambiguousKeys[n.Key], n.Path)
			issues = append(issues, Issue{n.Path, "ambiguous-link-key",
				"[[" + n.Key + "]] matches both " + pathByKey[n.Key] + " and " + n.Path + "; links using this name resolve to neither"})
		}
		if prevID, ok := idByStableKey[stableKey]; ok && prevID != id {
			if len(ambiguousStableKeys[stableKey]) == 0 {
				ambiguousStableKeys[stableKey] = []string{pathByStableKey[stableKey]}
			}
			ambiguousStableKeys[stableKey] = append(ambiguousStableKeys[stableKey], n.Path)
			delete(idByStableKey, stableKey)
			issues = append(issues, Issue{n.Path, "ambiguous-id-key",
				"ids " + prevID + " and " + id + " differ only by case or Unicode normalization; stable-id links using this name resolve to neither"})
		} else if len(ambiguousStableKeys[stableKey]) == 0 {
			idByStableKey[stableKey] = id
			pathByStableKey[stableKey] = n.Path
		}
		anchors := make(map[string]bool, len(n.Headings))
		rawAnchorCounts := map[string]int{}
		for _, h := range n.Headings {
			if err := buildGraphContextCause(ctx); err != nil {
				return nil, nil, err
			}
			if h.Anchor != "" {
				anchors[h.Anchor] = true
			}
			rawAnchorCounts[vault.Slugify(h.Text)]++
		}
		// Retain unique Markdown aliases. Repeated block fields are available
		// through their scoped addresses; an ambiguous alias never picks one.
		for alias, count := range rawAnchorCounts {
			if alias != "" && count == 1 {
				anchors[alias] = true
			}
		}
		anchorsByID[id] = anchors
		if n.FM.Template != "" {
			if authored, err := vault.ReadAuthoring(n.FM, n.Body); err == nil {
				for _, section := range authored.OrderedSections {
					if strings.TrimSpace(section.Text) != "" {
						anchors[vault.Slugify(section.Key)] = true
					}
				}
			}
		}
		pathByID[id] = n.Path
		pathByKey[n.Key] = n.Path
		idByKey[n.Key] = id
		pathKey := pathLinkKey(n.Path)
		switch {
		case len(ambiguousPathKeys[pathKey]) > 0:
			ambiguousPathKeys[pathKey] = append(ambiguousPathKeys[pathKey], n.Path)
			issues = append(issues, Issue{n.Path, "ambiguous-path-key",
				"path " + n.Path + " differs from another note path only by case or Unicode normalization; qualified links using this path resolve to neither"})
		case idByPath[pathKey] != "" && idByPath[pathKey] != id:
			ambiguousPathKeys[pathKey] = []string{pathByPathKey[pathKey], n.Path}
			delete(idByPath, pathKey)
			issues = append(issues, Issue{n.Path, "ambiguous-path-key",
				"paths " + pathByPathKey[pathKey] + " and " + n.Path + " differ only by case or Unicode normalization; qualified links using this path resolve to neither"})
		default:
			idByPath[pathKey] = id
			pathByPathKey[pathKey] = n.Path
		}
	}

	// resolveTarget is the one note-target namespace for wikilinks, related, and
	// supersedes. Keeping this lookup shared is load-bearing: a stable frontmatter id
	// can differ from the filename after a rename, and case/Unicode aliases can make
	// basenames, stable ids, or qualified paths ambiguous. A second, narrower resolver
	// for supersedes could silently retire a different note than [[the same target]].
	// problem is empty only for an ordinary not-found result; a non-empty value is an
	// ambiguity explanation that the caller prefixes with its own field syntax.
	resolveTarget := func(key string) (id, problem string, ok bool) {
		if strings.Contains(key, "/") {
			if owners, amb := ambiguousPathKeys[key]; amb {
				return "", "matches normalized paths " + strings.Join(owners, " and ") + "; rename one path so it is unique", false
			}
			if pathID, found := idByPath[key]; found {
				return pathID, "", true
			}
		}

		// Basename and stable id share the shorthand namespace. After a stable id's
		// file is renamed, a new file can claim its old basename; choosing either
		// silently retargets the relation.
		basenameID, hasBasename := idByKey[key]
		stableID, hasStable := idByStableKey[key]
		if hasBasename && hasStable && basenameID != stableID {
			return "", "matches note " + basenameID + " by filename and note " + stableID + " by stable id; use the exact vault path", false
		}
		if owners, amb := ambiguousKeys[key]; amb {
			return "", "matches " + strings.Join(owners, " and ") + "; qualify it with the folder path", false
		}
		if hasBasename {
			return basenameID, "", true
		}
		if owners, amb := ambiguousStableKeys[key]; amb {
			return "", "matches stable ids in " + strings.Join(owners, " and ") + "; use the exact vault path", false
		}
		if hasStable {
			return stableID, "", true
		}
		return "", "", false
	}

	for _, n := range notes {
		if err := buildGraphContextCause(ctx); err != nil {
			return nil, nil, err
		}
		id := effectiveID(n)
		noteNode := "note:" + id
		title := n.FM.Title
		if title == "" {
			title = n.Key
		}
		attrs := map[string]any{"type": string(n.FM.Type), "scope": strings.Join(n.FM.EffectiveScopes(), ",")}
		if n.FM.When != "" {
			attrs["when"] = n.FM.When
		}
		for k, v := range vault.ReadLegacy(n.FM).Values() {
			if err := buildGraphContextCause(ctx); err != nil {
				return nil, nil, err
			}
			clean, _ := vault.StripComments(v)
			clean = strings.TrimSpace(clean)
			if !vault.Unfilled(clean) {
				attrs[k] = clean
			}
		}
		authored, authoringErr := vault.ReadAuthoring(n.FM, n.Body)
		reader := readerAuthoringFrom(n, authored, authoringErr)
		attrs["status"] = n.FM.Status
		attrs["summary"] = reader.Summary
		attrs["template"] = n.FM.Template
		attrs["template_version"] = n.FM.TemplateVersion
		attrs["verified_at"] = n.FM.VerifiedAt
		attrs["collections"] = []string(n.FM.Collections)
		attrs["reader_authoring"] = reader
		attrs["reader_behavior"] = readerBehaviorFrom(n, authored, authoringErr)
		g.AddNode(&graph.Node{ID: noteNode, Kind: "note", Label: title, NoteID: id, NotePath: n.Path, Attrs: attrs})

		seenHeading := map[string]int{}
		for _, h := range n.Headings {
			if err := buildGraphContextCause(ctx); err != nil {
				return nil, nil, err
			}
			if h.Anchor == "" {
				continue
			}
			if firstLine, exists := seenHeading[h.Anchor]; exists {
				issues = append(issues, Issue{n.Path, "duplicate-anchor",
					"heading anchor #" + h.Anchor + " on line " + strconv.Itoa(h.Line) +
						" duplicates line " + strconv.Itoa(firstLine) + "; rename one heading so both sections are addressable"})
				continue
			}
			seenHeading[h.Anchor] = h.Line
			hid := noteNode + "#" + h.Anchor
			g.AddNode(&graph.Node{ID: hid, Kind: "heading", Label: h.Text, NoteID: id, NotePath: n.Path, Anchor: h.Anchor, SourceLoc: locStr(h.Line)})
			g.AddEdge(graph.Edge{Source: noteNode, Target: hid, Relation: "contains", Confidence: graph.ConfExtracted, ConfidenceScore: 1, Weight: 1})
		}

		seenTag := map[string]bool{}
		addTag := func(name string) {
			name = canonicalLinkText(strings.TrimPrefix(name, "#"))
			if name == "" || seenTag[name] {
				return
			}
			seenTag[name] = true
			tid := "tag:" + name
			g.AddNode(&graph.Node{ID: tid, Kind: "tag", Label: name})
			g.AddEdge(graph.Edge{Source: noteNode, Target: tid, Relation: "tagged", Confidence: graph.ConfExtracted, ConfidenceScore: 1, Weight: 1})
		}
		for _, t := range n.Tags {
			if err := buildGraphContextCause(ctx); err != nil {
				return nil, nil, err
			}
			addTag(t.Name)
		}
		for _, t := range n.FM.Tags {
			if err := buildGraphContextCause(ctx); err != nil {
				return nil, nil, err
			}
			addTag(t)
		}

		addRef := func(rawTarget string, line int) {
			displayTarget := strings.TrimSpace(rawTarget)
			target, anchor := displayTarget, ""
			if hash := strings.IndexByte(target, '#'); hash >= 0 {
				anchor = strings.TrimSpace(target[hash+1:])
				target = target[:hash]
			}
			key := linkKey(target)
			if key == "" && anchor == "" {
				return
			}
			tid, resolved := "", false
			if key == "" {
				// [[#heading]] is a local section link.
				tid, resolved = id, true
			}
			if !resolved {
				var problem string
				tid, problem, resolved = resolveTarget(key)
				if problem != "" {
					issues = append(issues, Issue{n.Path, "ambiguous-link", "[[" + displayTarget + "]] " + problem})
					return
				}
			}
			if !resolved {
				issues = append(issues, Issue{n.Path, "broken-link",
					"[[" + displayTarget + "]] " + brokenLinkReason(displayTarget)})
				return
			}
			if anchor != "" {
				anchorKey := vault.Slugify(anchor)
				if anchorKey == "" || !anchorsByID[tid][anchorKey] {
					issues = append(issues, Issue{n.Path, "broken-anchor",
						"[[" + displayTarget + "]] names missing heading #" + anchor + " in note " + tid})
				}
			}
			g.AddEdge(graph.Edge{Source: noteNode, Target: "note:" + tid, Relation: "references", Confidence: graph.ConfExtracted, ConfidenceScore: 1, Weight: 1, SourceLoc: locStr(line)})
		}
		for _, l := range n.Links {
			if err := buildGraphContextCause(ctx); err != nil {
				return nil, nil, err
			}
			addRef(l.Target, l.Line)
		}
		for _, r := range n.FM.Related {
			if err := buildGraphContextCause(ctx); err != nil {
				return nil, nil, err
			}
			addRef(r, 0)
		}
		// Preserve links authored in historical frontmatter prose through the legacy
		// adapter. Use the body's markup scanner so code and syntax examples remain
		// non-content rather than becoming semantic references.
		for _, name := range []string{"do", "dont", "why"} {
			field := vault.ReadLegacy(n.FM).Values()[name]
			if err := buildGraphContextCause(ctx); err != nil {
				return nil, nil, err
			}
			if field == "" {
				continue
			}
			clean, _ := vault.StripNonContent(field)
			p := &ParsedNote{}
			for _, line := range strings.Split(clean, "\n") {
				if err := buildGraphContextCause(ctx); err != nil {
					return nil, nil, err
				}
				p.appendLinks(line, 0)
			}
			for _, l := range p.Links {
				if err := buildGraphContextCause(ctx); err != nil {
					return nil, nil, err
				}
				addRef(l.Target, 0)
			}
		}
	}
	// supersedes: the note-level UPDATE path. Until this pass existed the field was
	// parsed and hashed as "retrieval-critical" and then consumed by nothing, so a
	// corrected diagnosis became a NEW note while the one it corrected kept its rank and
	// kept being retrieved first. Authors had been hand-encoding the relation into
	// filenames instead ("correction-...", "root-cause-found-...", "supersedes-the-..."),
	// which reads to a human and is invisible to ranking.
	//
	// A SECOND PASS, deliberately: the attr has to land on the TARGET node, and the
	// target may be any note in the corpus, including one built after the superseding
	// note in the loop above. Resolving here also means every id/key/path is already
	// registered, so a forward reference resolves exactly like a backward one.
	//
	// A target can be claimed by more than one source (two authors independently
	// correct the same wrong diagnosis), and superseded_by is a scalar, so only one
	// claim can win the attr. The naive "last SetNodeAttr call wins" made the winner a
	// function of `notes` order, which ReconcileIncremental does NOT hold constant: it
	// rebuilds from NoteCache.Snapshot(), which ranges a Go map, so the winner could
	// re-flip on every unrelated incremental edit for the life of the watcher process.
	// Collect every claim per target here and resolve the winner in one deterministic
	// pass below, once every note has been walked.
	claims := map[string][]supersedeClaim{}
	for _, n := range notes {
		if err := buildGraphContextCause(ctx); err != nil {
			return nil, nil, err
		}
		if len(n.FM.Supersedes) == 0 || vault.IsDraft(n.FM) {
			continue
		}
		srcID := effectiveID(n)
		for _, raw := range n.FM.Supersedes {
			if err := buildGraphContextCause(ctx); err != nil {
				return nil, nil, err
			}
			key := linkKey(raw)
			if key == "" {
				continue
			}
			tid, problem, ok := resolveTarget(key)
			if problem != "" {
				// Same refusal-to-guess rule as references: binding an ambiguous name to
				// an arbitrary winner would retire the wrong note, which is strictly worse
				// than retiring none.
				issues = append(issues, Issue{n.Path, "ambiguous-link",
					"supersedes: " + strings.TrimSpace(raw) + " " + problem})
				continue
			}
			if !ok {
				issues = append(issues, Issue{n.Path, "broken-link",
					"supersedes: " + strings.TrimSpace(raw) + " " + brokenLinkReason(strings.TrimSpace(raw))})
				continue
			}
			// A note superseding itself would demote the very note the author is trying to
			// promote, and it is always a typo (usually a copied id).
			if tid == srcID {
				issues = append(issues, Issue{n.Path, "broken-link",
					"supersedes: " + strings.TrimSpace(raw) + " is this note itself"})
				continue
			}
			g.AddEdge(graph.Edge{Source: "note:" + srcID, Target: "note:" + tid, Relation: "supersedes", Confidence: graph.ConfExtracted, ConfidenceScore: 1, Weight: 1})
			// The edge above is recorded regardless of whether the target node exists (an
			// edge to a bare reference node is still useful), but the attr write below
			// needs a real node, so check that here and queue the claim rather than
			// stamping immediately: the winner is picked once, after every note has been
			// walked, not by whichever claim happens to run first.
			if _, ok := g.Node("note:" + tid); !ok {
				issues = append(issues, Issue{n.Path, "broken-link",
					"supersedes: " + strings.TrimSpace(raw) + " resolved to " + tid + " but that note has no graph node"})
				continue
			}
			claims[tid] = append(claims[tid], supersedeClaim{srcID: srcID, when: n.FM.When})
		}
	}
	// Resolve one winner per contested target: most recent `when` first, ties broken
	// by id (lexicographically greater), so the result is a TOTAL order over the
	// claimants themselves and never depends on the order claims arrived in. This is
	// the mirror image of the ambiguous-TARGET refusal above, and deliberately does NOT
	// refuse the way that one does: binding an ambiguous NAME to a node risks retiring
	// the wrong note entirely, a mistake nothing else in the graph can catch. Here every
	// claimant already resolved to a real, distinct edge, so picking a SOURCE winner
	// only decides which one gets to be the headline "read this instead" on the demoted
	// card; a reader who wants the full picture still has every supersedes edge to walk,
	// so an arbitrary-but-stable pick costs a hint, not an unrecoverable retraction. A
	// note with no `when` sorts as older than any dated claimant, since a dateless
	// correction cannot out-rank one an author bothered to date.
	for tid, claimants := range claims {
		if err := buildGraphContextCause(ctx); err != nil {
			return nil, nil, err
		}
		sort.Slice(claimants, func(i, j int) bool {
			if claimants[i].when != claimants[j].when {
				return claimants[i].when > claimants[j].when
			}
			return claimants[i].srcID > claimants[j].srcID
		})
		if err := buildGraphContextCause(ctx); err != nil {
			return nil, nil, err
		}
		g.SetNodeAttr("note:"+tid, "superseded_by", claimants[0].srcID)
	}
	// Degrees are computed in a final pass so they do not depend on the interleaved
	// AddNode/AddEdge order above (an edge to a later note would otherwise undercount
	// that note's inbound degree). This keeps BuildGraph's degrees identical to
	// LoadGraph's, so the MCP (BuildGraph) and CLI (LoadGraph) retrieval paths agree.
	if err := g.RecomputeDegreesContext(ctx); err != nil {
		return nil, nil, err
	}
	if err := buildGraphContextCause(ctx); err != nil {
		return nil, nil, err
	}
	return g, issues, nil
}

// buildGraphContextCause is the cheap cancellation probe for graph assembly. Cause
// preserves a caller-supplied WithCancelCause error rather than reducing it to the
// generic context.Canceled sentinel.
func buildGraphContextCause(ctx context.Context) error {
	select {
	case <-ctx.Done():
		if err := context.Cause(ctx); err != nil {
			return err
		}
		return ctx.Err()
	default:
		return nil
	}
}

// foreignStoreID matches the filename shape of a Claude MEMORY file: a type prefix,
// then snake_case. Vault ids are kebab-case, so the two never collide.
var foreignStoreID = regexp.MustCompile(`^(feedback|project|reference|user)_[a-z0-9]+(_[a-z0-9]+)+$`)

// brokenLinkReason explains a dangling link, naming the cause when the shape gives it away.
//
// "resolves to nothing" is true but leaves the author guessing, and the single most common
// cause here is not a typo: an agent writes a link to one of its own Claude memory files
// ([[feedback_verify_before_shipping]], [[project_mesh_open_core]]). Those live in a
// DIFFERENT store and no note in this vault will ever carry that id, so the advice is to
// stop linking it, not to go hunting for the note. 27 of one vault's dangling links were a
// single memory slug, and the pattern kept coming back because the message never said so.
func brokenLinkReason(target string) string {
	if foreignStoreID.MatchString(target) {
		return "looks like a Claude memory filename, not a note id in this vault. " +
			"Those live in a different store and will never resolve here: drop the brackets " +
			"and leave it as `" + target + "` in the prose, or link the vault note that covers " +
			"the same ground."
	}
	return "resolves to nothing"
}

func effectiveID(n *ParsedNote) string {
	if n.FM.ID != "" {
		return n.FM.ID
	}
	return n.Key
}

func locStr(line int) string {
	if line <= 0 {
		return ""
	}
	return "L" + strconv.Itoa(line)
}
