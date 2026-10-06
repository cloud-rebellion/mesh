// SPDX-License-Identifier: LicenseRef-Mesh-Sustainable-Use-License
// Copyright (C) 2026 Bright Interaction AB

package index

import (
	"strings"

	"github.com/bright-interaction/mesh/internal/vault"
)

// maxChunkChars caps a single chunk so a runaway section never blows past a
// small embedding model's context window (nomic-embed-text tops out ~8k tokens;
// ~6k chars stays comfortably inside while keeping sections whole in practice).
const maxChunkChars = 6000

// ChunkText splits a note into retrieval units. Chunk 0 is the header (title +
// authored summary + retained legacy guidance + tags); the rest are one chunk per heading section,
// each carrying the title as context so an isolated chunk is still
// self-describing. The default embed path joins these into one whole-note
// vector; `mesh embed --per-section` stores them separately and scores a note
// by its best-matching section (max-pool). Per-section showed no recall or
// answer@1 lift on a large production corpus at ~18x the cost, so whole-note is default
// (see the dogfood decision note), but the structured join is itself a better
// text representation than the collapsed search body.
func ChunkText(pn *ParsedNote) []string {
	title := titleOf(pn)

	header := title
	// Placeholders must not reach the embedding header: it is prepended to EVERY chunk of
	// the note, so "TODO" three times over dragged unfilled notes together in vector space
	// and diluted their real content.
	summary := pn.FM.Summary
	if authored, err := vault.ReadAuthoring(pn.FM, pn.Body); err == nil {
		summary = authored.Summary
	}
	for _, v := range append([]string{summary}, vault.LegacySearchText(pn.FM)...) {
		clean, _ := vault.StripComments(v)
		if !vault.Unfilled(clean) {
			header += "\n" + clean
		}
	}
	if len(pn.FM.Tags) > 0 {
		header += "\n" + strings.Join(pn.FM.Tags, " ")
	}
	chunks := []string{truncate(collapse(header))}

	// Same comment stripping as the parser and the FTS body, so a chunk never carries
	// text the rest of Mesh treats as hidden. Code stays: it is what people embed and
	// search for.
	body, _ := vault.StripComments(pn.Body)
	var cur []string
	flush := func() {
		seg := strings.TrimSpace(strings.Join(cur, "\n"))
		cur = cur[:0]
		if seg == "" {
			return
		}
		chunks = append(chunks, truncate(title+"\n"+seg))
	}
	for _, line := range strings.Split(body, "\n") {
		if _, ok := parseHeading(vault.StripCodeSpans(line)); ok && len(cur) > 0 {
			flush()
		}
		cur = append(cur, line)
	}
	flush()
	return chunks
}

// collapse squeezes runs of whitespace to single spaces (header chunk only; body
// sections keep their newlines so headings stay legible to the embedder).
func collapse(s string) string {
	return strings.Join(strings.Fields(s), " ")
}

func truncate(s string) string {
	if len(s) <= maxChunkChars {
		return s
	}
	return s[:maxChunkChars]
}
