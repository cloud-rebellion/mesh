// SPDX-License-Identifier: LicenseRef-Mesh-Sustainable-Use-License
// Copyright (C) 2026 Bright Interaction AB

package mcp

import (
	"path"
	"regexp"
	"strings"
	"unicode"

	"github.com/bright-interaction/mesh/internal/vault"
)

// This file is the INSTRUCTION BOUNDARY for the LLM sink.
//
// Connector-ingested notes (internal/ingest) carry text written by whoever could post
// to the upstream system: a GitHub issue comment, a Slack message, a Notion page. The
// HTML sink (internal/web) and the TTY sink (internal/textdiff) both already treat
// those bytes as untrusted for their own renderer. The agent is a sink too, and its
// injection is "text that reads like an instruction", so the same bytes need a marker
// here: an explicit envelope that says the enclosed span is DATA, plus the provenance
// that says where it came from. The contract (contract.go) tells the agent what the
// envelope means; this file decides what goes inside it.
// These are model-facing provenance cues, not a proof against prompt injection;
// authorization and data-access checks must remain independent of model behavior.
//
// It is deliberately cheap because it runs on the hot path of every search: a path
// prefix test per card and, for the notes that ARE imported, one wrapper around the
// snippet. A note that is not imported is untouched and costs zero extra tokens.

const (
	// untrustedOpenPrefix / untrustedClose delimit third-party content. The tag name
	// is spelled out (not a bare fence) so it survives truncation and is unambiguous
	// to the model even when a snippet lands mid-context. Brackets stay literal in
	// search/batch tool text even after nested JSON encoding with HTML escaping on.
	untrustedOpenPrefix = "[[untrusted-external-content"
	untrustedClose      = "[[/untrusted-external-content]]"

	// importedPathPrefix is where connector ingest writes third-party notes:
	// ingest.RenderDoc always renders to imported/<connector>/<id>.md, and it is the
	// only writer of that tree. It is the provenance signal available on a search
	// card, which carries a Path but no source field.
	importedPathPrefix = "imported/"

	// importSourcePrefix is what ingest.RenderDoc stamps into the note frontmatter
	// (source: import:<connector>). The fetch path reads the file, so it can use the
	// authoritative frontmatter instead of inferring from the path.
	importSourcePrefix = "import:"
)

// importedSource returns the provenance source for a vault-relative note path
// ("import:<connector>") and whether that path is third-party ingested content.
// Fail-safe by construction: an unrecognised path is simply not labelled, and a
// hand-written note that happens to live under imported/ is labelled untrusted,
// which is the harmless direction.
func importedSource(notePath string) (string, bool) {
	p := path.Clean(strings.ReplaceAll(strings.TrimSpace(notePath), "\\", "/"))
	p = strings.TrimPrefix(p, "./")
	if !strings.HasPrefix(p, importedPathPrefix) {
		return "", false
	}
	connector, rest, found := strings.Cut(strings.TrimPrefix(p, importedPathPrefix), "/")
	if !found || connector == "" || rest == "" {
		return "", false
	}
	return importSourcePrefix + connector, true
}

// wrapUntrusted puts text inside the data envelope, tagged with its provenance. url
// is optional (it is the upstream permalink when the frontmatter carried one).
func wrapUntrusted(source, url, text string) string {
	var b strings.Builder
	b.Grow(len(text) + 128)
	b.WriteString(untrustedOpenPrefix)
	b.WriteString(` source="`)
	b.WriteString(sanitizeAttr(source))
	b.WriteString(`"`)
	if u := strings.TrimSpace(url); u != "" {
		b.WriteString(` url="`)
		b.WriteString(sanitizeAttr(u))
		b.WriteString(`"`)
	}
	b.WriteString("]]\n")
	// Neutralize lookalike boundaries inside the data, including the legacy form.
	// Do not let source-authored tags compete with Mesh's own provenance cues.
	b.WriteString(stripEnvelopeTags(text))
	b.WriteString("\n")
	b.WriteString(untrustedClose)
	return b.String()
}

// sanitizeAttr keeps an attribute value on one line and free of the quote that would
// end it, so provenance can never break out of the opening tag.
func sanitizeAttr(v string) string {
	repl := strings.NewReplacer(`"`, "'", "<", "(", ">", ")", "[", "(", "]", ")")
	v = strings.Map(func(r rune) rune {
		if unicode.IsSpace(r) {
			return ' '
		}
		return r
	}, v)
	return strings.TrimSpace(repl.Replace(v))
}

// Match the recognizable prefix, not an entire well-formed tag: an excerpt may
// end mid-tag. Accept case/spacing variants of current and legacy markers, using
// the same whitespace repertoire as unicode.IsSpace. This is a linear-time RE2
// match, compiled once; it never parses or decodes arbitrary source markup.
const envelopeSpace = `[[:space:]\p{Z}\x{0085}]*`

var envelopePrefix = regexp.MustCompile(`(?i)(?:<|\[` + envelopeSpace + `\[)` + envelopeSpace + `(/?)` + envelopeSpace + `untrusted-external-content`)

// stripEnvelopeTags breaks marker prefixes while retaining the rest of the data.
// Do not decode HTML/JSON escapes here: that would silently change source content.
func stripEnvelopeTags(text string) string {
	if !strings.ContainsAny(text, "<[") {
		return text
	}
	return envelopePrefix.ReplaceAllString(text, `(${1}untrusted-external-content`)
}

// frontmatterProvenance reads current YAML using the same parser as the index.
// Folded scalars and quoted escapes must retain import provenance after a note
// moves outside imported/. Invalid or absent metadata leaves the path fallback
// authoritative; access checks remain independent of this provenance cue.
func frontmatterProvenance(body string) (source, sourceURL string) {
	header, _, had := vault.SplitFrontmatter(body)
	if !had {
		return "", ""
	}
	fm, _, err := vault.ParseFrontmatter([]byte(header))
	if err != nil {
		return "", ""
	}
	return fm.Source, fm.SourceURL
}
