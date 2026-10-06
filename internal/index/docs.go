// SPDX-License-Identifier: LicenseRef-Mesh-Sustainable-Use-License
// Copyright (C) 2026 Bright Interaction AB

package index

import (
	"context"
	"encoding/json"
	"strings"
	"unicode/utf8"

	"github.com/bright-interaction/mesh/internal/vault"
)

// maxDocChars caps the text sent per candidate to a reranker. A cross-encoder
// truncates internally (ms-marco-MiniLM ~512 tokens), so this only keeps HTTP
// payloads small; the leading title + first prose carry the signal.
const maxDocChars = 2000

// NoteMetadata is the current, persisted identity and read-boundary metadata for
// one note. Retrieval keeps an in-memory graph for ranking, but that graph can be
// one watcher generation behind the notes/search tables. Anything used for access
// control or returned to a caller must therefore come from this current snapshot.
type NoteMetadata struct {
	NodeID            string
	NoteID            string
	Path              string
	Type              string
	Title             string
	Scope             string
	SupersededBy      string
	SupersederPath    string
	SupersederScope   string
	SupersederState   string
	State             string
	Summary           string
	Template          string
	TemplateVersion   int
	Updated           string
	VerifiedAt        string
	Source            string
	SourceURL         string
	Sections          []SectionAddress
	SectionsTruncated bool
	MissingGuidance   []string
}

// Metadata and template completeness are read in the same identity/ACL snapshot.
// Indexed section addresses describe the current persisted body, not the separately
// refreshed ranking graph. A template mismatch is incomplete until reindexed.
func applyReaderMetadata(m *NoteMetadata, fmJSON, attrsJSON string) {
	if strings.EqualFold(strings.TrimSpace(m.SupersederState), "draft") {
		m.SupersededBy, m.SupersederPath, m.SupersederScope, m.SupersederState = "", "", "", ""
	}
	var fm vault.Frontmatter
	if err := json.Unmarshal([]byte(fmJSON), &fm); err != nil {
		fm.Type = vault.NoteType(m.Type)
		m.MissingGuidance = vault.ReadLegacy(&fm).Missing()
		return
	}
	m.State, m.Template, m.TemplateVersion = fm.Status, fm.Template, fm.TemplateVersion
	if strings.HasPrefix(fm.Source, "import:") {
		m.Source, m.SourceURL = fm.Source, fm.SourceURL
	}
	if summary, _ := vault.StripComments(fm.Summary); !vault.Unfilled(summary) {
		m.Summary = boundedText(summary, 320)
	}
	m.Updated, m.VerifiedAt = fm.Updated, fm.VerifiedAt
	if m.Updated == "" {
		m.Updated = fm.When
	}
	if fm.Template == "" && fm.TemplateVersion == 0 {
		m.MissingGuidance = vault.ReadLegacy(&fm).Missing()
		return
	}
	var attrs struct {
		Authoring *authoringMetadata `json:"reader_authoring"`
	}
	_ = json.Unmarshal([]byte(attrsJSON), &attrs)
	if attrs.Authoring != nil && attrs.Authoring.Template == fm.Template && attrs.Authoring.Version == fm.TemplateVersion {
		m.MissingGuidance, m.Sections = attrs.Authoring.Missing, attrs.Authoring.Sections
		m.SectionsTruncated = attrs.Authoring.SectionsTruncated
		m.Summary = attrs.Authoring.Summary
		return
	}
	// Old indexed metadata cannot attest to the presence of new sections.
	m.MissingGuidance = []string{"indexed template sections"}
}

// NoteDocument pairs rerankable text with the note metadata read in the SAME SQL
// statement. A reindex cannot therefore pair a new secret body with an old public
// path/scope between separate reads.
type NoteDocument struct {
	NoteMetadata
	Text string
}

// NoteMetadataFor returns current persisted metadata for the requested note node
// ids, keyed by node id. Missing/deleted ids are absent. Supersession fields come
// from the target graph row joined to the CURRENT superseding note in the same SQL
// statement, so callers never combine a stale relation with old scope/path metadata.
func (s *Store) NoteMetadataFor(ctx context.Context, ids []string) (map[string]NoteMetadata, error) {
	out := make(map[string]NoteMetadata, len(ids))
	// Keep comfortably below SQLite's host-parameter ceiling. Graph/vector candidate
	// validation can cover an entire large vault, unlike the <=30 reranker document read.
	const batchSize = 500
	for start := 0; start < len(ids); start += batchSize {
		end := start + batchSize
		if end > len(ids) {
			end = len(ids)
		}
		if err := s.noteMetadataBatch(ctx, ids[start:end], out); err != nil {
			return nil, err
		}
	}
	return out, nil
}

func (s *Store) noteMetadataBatch(ctx context.Context, ids []string, out map[string]NoteMetadata) error {
	placeholders, args := noteIDArgs(ids)
	rows, err := s.readDB.QueryContext(ctx, `
SELECT 'note:' || n.id, n.id, n.path, n.type, n.title, n.scope,
       COALESCE(sup.id, ''), COALESCE(sup.path, ''), COALESCE(sup.scope, ''),
       CASE WHEN json_valid(sup.frontmatter) THEN COALESCE(json_extract(sup.frontmatter, '$.Status'), '') ELSE '' END,
       n.frontmatter, COALESCE(gn.attrs, '{}')
FROM notes n
LEFT JOIN nodes gn ON gn.id = 'note:' || n.id
LEFT JOIN notes sup ON sup.id = CASE
  WHEN json_valid(gn.attrs) THEN json_extract(gn.attrs, '$.superseded_by')
  ELSE NULL
END
WHERE 'note:' || n.id IN (`+placeholders+`)`, args...)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var m NoteMetadata
		var fmJSON, attrsJSON string
		if err := rows.Scan(
			&m.NodeID, &m.NoteID, &m.Path, &m.Type, &m.Title, &m.Scope,
			&m.SupersededBy, &m.SupersederPath, &m.SupersederScope, &m.SupersederState,
			&fmJSON, &attrsJSON,
		); err != nil {
			return err
		}
		applyReaderMetadata(&m, fmJSON, attrsJSON)
		out[m.NodeID] = m
	}
	return rows.Err()
}

// NoteDocuments returns current rerankable text and its access-control metadata
// from one snapshot, keyed by node id. Missing/deleted ids are absent.
func (s *Store) NoteDocuments(ctx context.Context, ids []string) (map[string]NoteDocument, error) {
	out := make(map[string]NoteDocument, len(ids))
	if len(ids) == 0 {
		return out, nil
	}
	placeholders, args := noteIDArgs(ids)
	rows, err := s.readDB.QueryContext(ctx, `
SELECT si.node_id, n.id, n.path, n.type, n.title, n.scope,
       COALESCE(sup.id, ''), COALESCE(sup.path, ''), COALESCE(sup.scope, ''),
       CASE WHEN json_valid(sup.frontmatter) THEN COALESCE(json_extract(sup.frontmatter, '$.Status'), '') ELSE '' END,
       si.body, n.frontmatter, COALESCE(gn.attrs, '{}')
FROM search_index si
JOIN notes n ON si.node_id = 'note:' || n.id
LEFT JOIN nodes gn ON gn.id = si.node_id
LEFT JOIN notes sup ON sup.id = CASE
  WHEN json_valid(gn.attrs) THEN json_extract(gn.attrs, '$.superseded_by')
  ELSE NULL
END
WHERE si.node_id IN (`+placeholders+`)`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var d NoteDocument
		var body, fmJSON, attrsJSON string
		if err := rows.Scan(
			&d.NodeID, &d.NoteID, &d.Path, &d.Type, &d.Title, &d.Scope,
			&d.SupersededBy, &d.SupersederPath, &d.SupersederScope, &d.SupersederState,
			&body, &fmJSON, &attrsJSON,
		); err != nil {
			return nil, err
		}
		applyReaderMetadata(&d.NoteMetadata, fmJSON, attrsJSON)
		d.Text = boundedDocument(d.Title, body)
		out[d.NodeID] = d
	}
	return out, rows.Err()
}

// NoteDocs returns the rerankable text (title + indexed body) for the given
// note node ids, keyed by node id. Missing ids are simply absent from the map.
// The text is the same searchText Mesh indexes into FTS, so the reranker scores
// the query against exactly what made the note a candidate.
func (s *Store) NoteDocs(ids []string) (map[string]string, error) {
	docs, err := s.NoteDocuments(context.Background(), ids)
	if err != nil {
		return nil, err
	}
	out := make(map[string]string, len(docs))
	for id, doc := range docs {
		out[id] = doc.Text
	}
	return out, nil
}

func noteIDArgs(ids []string) (string, []any) {
	placeholders := strings.TrimSuffix(strings.Repeat("?,", len(ids)), ",")
	args := make([]any, len(ids))
	for i, id := range ids {
		args[i] = id
	}
	return placeholders, args
}

func boundedDocument(title, body string) string {
	doc := strings.TrimSpace(title + "\n" + body)
	if len(doc) > maxDocChars {
		// Cut on a rune boundary so Swedish (and any multibyte) text is not
		// sliced mid-rune into a garbage byte.
		cut := maxDocChars
		for cut > 0 && !utf8.RuneStart(doc[cut]) {
			cut--
		}
		doc = doc[:cut]
	}
	return doc
}
