// SPDX-License-Identifier: LicenseRef-Mesh-Sustainable-Use-License
// Copyright (C) 2026 Bright Interaction AB

package vault

import (
	"errors"
	"fmt"
	"strings"
	"time"
	"unicode/utf8"
)

const (
	MaxSummaryRunes   = 1200
	MaxAuthoringBytes = 64 << 10
	MaxCodeBytes      = 16 << 10
	MaxAuthoringList  = 32
	// Product overviews can retain many supported connections. Keep relationship
	// input bounded separately from compact scope, Topic and collection lists.
	MaxAuthoringRelated = 256
)

// NormalizeSpec resolves template/type/version and copies author-owned collections.
// It checks shape and boundedness, not completeness, so a draft can contain honest gaps.
func NormalizeSpec(in NewNoteSpec) (NewNoteSpec, error) {
	spec := in
	if in.UpdateID != "" {
		if !authoringID(in.UpdateID) || in.DraftID != "" || in.DraftRevision != "" {
			return spec, fmt.Errorf("%w: update_id must identify one published note, separately from draft completion", ErrInvalidSpec)
		}
	} else if in.UpdateRevision != "" {
		return spec, fmt.Errorf("%w: update_revision requires update_id", ErrInvalidSpec)
	}
	if in.Do != "" || in.Dont != "" || in.Why != "" {
		return spec, fmt.Errorf("%w: legacy do/dont/why authoring is unsupported; choose a template and body sections", ErrInvalidSpec)
	}
	spec.Title = strings.TrimSpace(in.Title)
	if spec.Title == "" || strings.ContainsAny(spec.Title, "\r\n") {
		return spec, fmt.Errorf("%w: a single-line title is required", ErrInvalidSpec)
	}
	if length := len(Slugify(spec.Title)); length > maxSlugLen {
		return spec, fmt.Errorf("%w: title too long: it slugs to %d bytes; shorten it to fit the %d-byte filename limit", ErrInvalidSpec, length, maxSlugLen)
	}
	if spec.Template == "" {
		spec.Template = DefaultTemplate(spec.Type)
	}
	t, err := TemplateFor(spec.Template, spec.TemplateVersion)
	if err != nil {
		return spec, err
	}
	if spec.Type != "" && spec.Type != t.Type {
		return spec, fmt.Errorf("%w: template %q requires type %q, got %q", ErrInvalidSpec, t.ID, t.Type, spec.Type)
	}
	spec.Type, spec.TemplateVersion = t.Type, t.Version
	spec.Summary = strings.TrimSpace(in.Summary)
	if utf8.RuneCountInString(spec.Summary) > MaxSummaryRunes {
		return spec, fmt.Errorf("%w: summary exceeds %d runes", ErrInvalidSpec, MaxSummaryRunes)
	}
	if err := authoredMarkup(spec.Summary, 2); err != nil {
		return spec, fmt.Errorf("%w: summary: %v", ErrInvalidSpec, err)
	}
	spec.Status = strings.ToLower(strings.TrimSpace(in.Status))
	if len(in.Related) > MaxAuthoringRelated {
		return spec, fmt.Errorf("%w: related exceeds %d entries", ErrInvalidSpec, MaxAuthoringRelated)
	}
	for name, list := range map[string][]string{"supersedes": in.Supersedes, "scope": in.Scope} {
		if len(list) > MaxAuthoringList {
			return spec, fmt.Errorf("%w: %s exceeds %d entries", ErrInvalidSpec, name, MaxAuthoringList)
		}
	}
	for name, list := range map[string][]string{"collections": in.Collections, "tags": in.Tags} {
		if len(list) > MaxAuthoringList {
			return spec, fmt.Errorf("%w: %s exceeds %d entries", ErrInvalidSpec, name, MaxAuthoringList)
		}
		for _, id := range list {
			if !authoringID(id) {
				return spec, fmt.Errorf("%w: invalid %s id %q; use an existing slug ID, not a path or wiki link", ErrInvalidSpec, name, id)
			}
		}
	}
	spec.Collections = uniqueStrings(in.Collections)
	spec.Tags = uniqueStrings(in.Tags)
	spec.Related = append([]string(nil), in.Related...)
	spec.Supersedes = append([]string(nil), in.Supersedes...)
	spec.Scope = append([]string(nil), in.Scope...)
	spec.Sections = make(map[string]string, len(in.Sections))
	known := map[string]bool{}
	for _, s := range t.Sections {
		known[s.Key] = true
	}
	total := len(spec.Summary)
	for key, text := range in.Sections {
		if !known[key] {
			return spec, fmt.Errorf("%w: unknown section %q for template %q", ErrInvalidSpec, key, t.ID)
		}
		if err := authoredMarkup(text, 2); err != nil {
			return spec, fmt.Errorf("%w: section %q: %v", ErrInvalidSpec, key, err)
		}
		spec.Sections[key] = text
		total += len(text)
	}
	if len(in.Blocks) > MaxAuthoringList {
		return spec, fmt.Errorf("%w: too many blocks (maximum %d)", ErrInvalidSpec, MaxAuthoringList)
	}
	seenBlocks := map[string]bool{}
	spec.Blocks = make([]BlockSpec, 0, len(in.Blocks))
	for _, block := range in.Blocks {
		bt, err := BlockTemplateFor(block.Template, block.Version)
		if err != nil {
			return spec, err
		}
		if !authoringID(block.ID) || seenBlocks[block.ID] {
			return spec, fmt.Errorf("%w: invalid or duplicate block id %q", ErrInvalidSpec, block.ID)
		}
		seenBlocks[block.ID] = true
		fields := map[string]bool{}
		for _, f := range bt.Fields {
			fields[f.Key] = true
		}
		copyBlock := BlockSpec{Template: bt.ID, Version: bt.Version, ID: block.ID, Fields: map[string]string{}}
		for key, text := range block.Fields {
			if !fields[key] {
				return spec, fmt.Errorf("%w: unknown field %q for block %q", ErrInvalidSpec, key, bt.ID)
			}
			if bt.ID == "code" && key == "code" {
				if len(text) > MaxCodeBytes {
					return spec, fmt.Errorf("%w: code exceeds %d bytes", ErrInvalidSpec, MaxCodeBytes)
				}
			} else if err := authoredMarkup(text, 3); err != nil {
				return spec, fmt.Errorf("%w: block %q field %q: %v", ErrInvalidSpec, block.ID, key, err)
			}
			copyBlock.Fields[key] = text
			total += len(text)
		}
		if bt.ID == "code" {
			if value := strings.TrimSpace(copyBlock.Fields["illustrative"]); value != "" && value != "true" && value != "false" && !Unfilled(value) {
				return spec, fmt.Errorf("%w: code illustrative must be true or false", ErrInvalidSpec)
			}
			if language := copyBlock.Fields["language"]; strings.ContainsAny(language, "\r\n~") || strings.ContainsRune(language, rune(96)) {
				return spec, fmt.Errorf("%w: code language must be a single safe fence label", ErrInvalidSpec)
			}
		}
		spec.Blocks = append(spec.Blocks, copyBlock)
	}
	if total > MaxAuthoringBytes {
		return spec, fmt.Errorf("%w: authored content exceeds %d bytes", ErrInvalidSpec, MaxAuthoringBytes)
	}
	if spec.VerifiedAt != "" {
		if _, err := time.Parse(time.RFC3339, spec.VerifiedAt); err != nil {
			if _, err := time.Parse("2006-01-02", spec.VerifiedAt); err != nil {
				return spec, fmt.Errorf("%w: verified_at must be a date or RFC3339 timestamp", ErrInvalidSpec)
			}
		}
		if !hasVerificationEvidence(spec) {
			return spec, fmt.Errorf("%w: verified_at requires an explicit verification block with authored checks, context, results and gaps", ErrInvalidSpec)
		}
	}
	return spec, nil
}

func authoringID(s string) bool {
	return s != "" && len(s) <= maxSlugLen && s == Slugify(s)
}

func uniqueStrings(in []string) []string {
	out := make([]string, 0, len(in))
	seen := map[string]bool{}
	for _, s := range in {
		if !seen[s] {
			out = append(out, s)
			seen[s] = true
		}
	}
	return out
}

func hasVerificationEvidence(spec NewNoteSpec) bool {
	for _, b := range spec.Blocks {
		if b.Template != "verification" {
			continue
		}
		complete := true
		for _, key := range []string{"checks", "context", "results", "gaps"} {
			if missingProse(b.Fields[key]) {
				complete = false
			}
		}
		if complete {
			return true
		}
	}
	return false
}

// authoredMarkup rejects ambiguous structural headings and control markers.
// Ordinary subheadings, balanced fences and code contents remain verbatim.
func authoredMarkup(text string, protectedLevel int) error {
	if UnterminatedFence(text) {
		return errors.New("unclosed code fence")
	}
	visible, open := StripNonContent(text)
	if open > 0 {
		return errors.New("unclosed HTML comment")
	}
	lines, masks := strings.Split(text, "\n"), strings.Split(visible, "\n")
	for i, mask := range masks {
		if h, ok := ParseATXHeading(mask, lines[i]); ok && h.Level <= protectedLevel {
			return errors.New("heading would escape its authored section")
		}
	}
	return nil
}

func missingProse(s string) bool {
	if Unfilled(s) {
		return true
	}
	visible, _ := StripComments(s)
	return Unfilled(visible)
}

// MissingContent always reports publication gaps, including for a saved draft.
func MissingContent(in NewNoteSpec) []string {
	spec, err := NormalizeSpec(in)
	if err != nil {
		return []string{err.Error()}
	}
	var missing []string
	if missingProse(spec.Summary) {
		missing = append(missing, "summary")
	}
	t, _ := TemplateFor(spec.Template, spec.TemplateVersion)
	for _, s := range t.Sections {
		if s.Required && missingProse(spec.Sections[s.Key]) {
			missing = append(missing, "section:"+s.Key)
		}
	}
	for _, b := range spec.Blocks {
		t, _ := BlockTemplateFor(b.Template, b.Version)
		for _, f := range t.Fields {
			if f.Required && missingProse(b.Fields[f.Key]) {
				missing = append(missing, "block:"+b.ID+":"+f.Key)
			}
		}
	}
	return missing
}

// ValidateSpec validates a publication, never silently treating a draft as complete.
func ValidateSpec(spec NewNoteSpec) error {
	if _, err := NormalizeSpec(spec); err != nil {
		return err
	}
	if missing := MissingContent(spec); len(missing) > 0 {
		return fmt.Errorf("%w: missing authored content: %s", ErrInvalidSpec, strings.Join(missing, ", "))
	}
	return nil
}

func IsDraft(fm *Frontmatter) bool {
	return fm != nil && strings.EqualFold(strings.TrimSpace(fm.Status), "draft")
}

type AuthoredSection struct {
	Key      string
	Heading  string
	Anchor   string
	Text     string
	Required bool
}

type ParsedBlock struct {
	BlockMetadata
	Fields map[string]string
	Body   string
	Anchor string
}

type AuthoringContent struct {
	Template        string
	Version         int
	Summary         string
	Sections        map[string]string
	OrderedSections []AuthoredSection
	Blocks          []ParsedBlock
	MissingSections []string
	Legacy          LegacyContent
}

// ReadAuthoring reads authored body sections using the same fence-aware heading
// scanner as Mesh. It does not fabricate modern meaning for legacy fields.
func ReadAuthoring(fm *Frontmatter, body string) (AuthoringContent, error) {
	out := AuthoringContent{Sections: map[string]string{}, Legacy: ReadLegacy(fm)}
	if fm == nil {
		return out, errors.New("missing frontmatter")
	}
	out.Template, out.Version = fm.Template, fm.TemplateVersion
	if fm.Template == "" && fm.TemplateVersion == 0 {
		out.Summary = fm.Summary
		return out, nil
	}
	t, err := TemplateFor(fm.Template, fm.TemplateVersion)
	if err != nil {
		return out, err
	}
	if t.Type != fm.Type {
		return out, fmt.Errorf("template %q does not match type %q", t.ID, fm.Type)
	}
	out.Version = t.Version
	spans := authoredSpans(body)
	summarySeen := false
	for _, span := range spans {
		if span.sectionKey == "summary" || (span.sectionKey == "" && span.heading == "Summary") {
			if summarySeen {
				return out, errors.New("duplicate summary section")
			}
			summarySeen = true
			if span.heading != "Summary" {
				return out, errors.New("summary heading differs from the fixed template heading")
			}
			out.Summary = span.text
		}
	}
	if missingProse(out.Summary) {
		out.MissingSections = append(out.MissingSections, "summary")
	}
	seen := map[string]bool{"summary": true}
	knownHeadings := map[string]bool{"Summary": true, "Related": true}
	blockMetadata := map[string]BlockMetadata{}
	for _, b := range fm.Blocks {
		if _, exists := blockMetadata[b.ID]; exists {
			return out, fmt.Errorf("duplicate block metadata %q", b.ID)
		}
		blockMetadata[b.ID] = b
	}
	for _, section := range t.Sections {
		knownHeadings[section.Heading] = true
		var matched *bodySpan
		for i := range spans {
			if spans[i].sectionKey == section.Key || (spans[i].sectionKey == "" && spans[i].block.ID == "" && spans[i].heading == section.Heading) {
				if matched != nil {
					return out, fmt.Errorf("duplicate section %q", section.Key)
				}
				matched = &spans[i]
			}
		}
		item := AuthoredSection{Key: section.Key, Heading: section.Heading, Anchor: Slugify(section.Heading), Required: section.Required}
		if matched != nil {
			if matched.heading != section.Heading {
				return out, fmt.Errorf("section %q differs from its fixed template heading", section.Key)
			}
			item.Heading, item.Anchor, item.Text = matched.heading, matched.anchor, matched.text
			out.Sections[section.Key] = matched.text
			seen[section.Key] = true
		}
		if section.Required && missingProse(item.Text) {
			out.MissingSections = append(out.MissingSections, section.Key)
		}
		out.OrderedSections = append(out.OrderedSections, item)
	}
	seenBlocks := map[string]bool{}
	for _, span := range spans {
		if span.sectionKey != "" && !seen[span.sectionKey] {
			return out, fmt.Errorf("unknown section marker %q", span.sectionKey)
		}
		if span.block.ID == "" {
			if span.sectionKey == "" && !knownHeadings[span.heading] {
				return out, fmt.Errorf("unknown template heading %q", span.heading)
			}
			continue
		}
		meta, ok := blockMetadata[span.block.ID]
		if !ok || meta != span.block {
			return out, fmt.Errorf("body block %q does not match frontmatter metadata", span.block.ID)
		}
		if seenBlocks[meta.ID] {
			return out, fmt.Errorf("duplicate body block %q", meta.ID)
		}
		if span.heading != "Block "+meta.ID {
			return out, fmt.Errorf("block %q differs from its fixed heading", meta.ID)
		}
		bt, err := BlockTemplateFor(meta.Template, meta.Version)
		if err != nil {
			return out, err
		}
		fields, err := blockFields(span.text, bt)
		if err != nil {
			return out, fmt.Errorf("block %q: %w", meta.ID, err)
		}
		for _, f := range bt.Fields {
			if f.Required && missingProse(fields[f.Key]) {
				out.MissingSections = append(out.MissingSections, "block:"+meta.ID+":"+f.Key)
			}
		}
		out.Blocks = append(out.Blocks, ParsedBlock{BlockMetadata: meta, Fields: fields, Body: span.text, Anchor: span.anchor})
		seenBlocks[meta.ID] = true
	}
	for id := range blockMetadata {
		if !seenBlocks[id] {
			return out, fmt.Errorf("block %q metadata has no body", id)
		}
	}
	return out, nil
}

func MissingSections(fm *Frontmatter, body string) []string {
	content, err := ReadAuthoring(fm, body)
	if err != nil {
		return []string{err.Error()}
	}
	return content.MissingSections
}

type bodySpan struct {
	heading, anchor, text, sectionKey string
	block                             BlockMetadata
}

func authoredSpans(body string) []bodySpan {
	visible, _ := StripNonContent(body)
	lines, masks := strings.Split(body, "\n"), strings.Split(visible, "\n")
	var out []bodySpan
	start := -1
	var current bodySpan
	finish := func(end int) {
		if start < 0 {
			return
		}
		text := strings.Join(lines[start:end], "\n")
		// A marker is metadata only immediately below an active H2 heading.
		if start < end {
			if key, ok := sectionMarker(lines[start]); ok {
				current.sectionKey = key
				text = strings.Join(lines[start+1:end], "\n")
			} else if block, ok := blockMarker(lines[start]); ok {
				current.block = block
				text = strings.Join(lines[start+1:end], "\n")
			}
		}
		current.text = strings.TrimSpace(text)
		out = append(out, current)
	}
	for i, mask := range masks {
		if heading, ok := ParseATXHeading(mask, lines[i]); ok && heading.Level <= 2 {
			finish(i)
			start = -1
			if heading.Level == 2 {
				current = bodySpan{heading: heading.Text, anchor: heading.Anchor}
				start = i + 1
			}
		}
	}
	finish(len(lines))
	return out
}

func sectionMarker(line string) (string, bool) {
	const prefix = "<!-- mesh:section "
	line = strings.TrimSpace(line)
	if !strings.HasPrefix(line, prefix) || !strings.HasSuffix(line, " -->") {
		return "", false
	}
	key := strings.TrimSuffix(strings.TrimPrefix(line, prefix), " -->")
	return key, key != "" && !strings.ContainsAny(key, " \t")
}

func blockMarker(line string) (BlockMetadata, bool) {
	const prefix = "<!-- mesh:block "
	line = strings.TrimSpace(line)
	if !strings.HasPrefix(line, prefix) || !strings.HasSuffix(line, " -->") {
		return BlockMetadata{}, false
	}
	parts := strings.Fields(strings.TrimSuffix(strings.TrimPrefix(line, prefix), " -->"))
	if len(parts) != 3 {
		return BlockMetadata{}, false
	}
	var version int
	if _, err := fmt.Sscanf(parts[1], "v%d", &version); err != nil {
		return BlockMetadata{}, false
	}
	return BlockMetadata{Template: parts[0], Version: version, ID: parts[2]}, true
}

func blockFields(body string, t BlockTemplate) (map[string]string, error) {
	out := map[string]string{}
	visible, _ := StripNonContent(body)
	lines, masks := strings.Split(body, "\n"), strings.Split(visible, "\n")
	key, start := "", 0
	finish := func(end int) {
		if key != "" {
			out[key] = strings.TrimSpace(strings.Join(lines[start:end], "\n"))
		}
	}
	for i, mask := range masks {
		if h, ok := ParseATXHeading(mask, lines[i]); ok && h.Level == 3 {
			finish(i)
			key = ""
			for _, f := range t.Fields {
				if h.Text == f.Heading {
					key = f.Key
					break
				}
			}
			if key == "" {
				return nil, fmt.Errorf("unknown field heading %q", h.Text)
			}
			if _, exists := out[key]; exists {
				return nil, fmt.Errorf("duplicate field heading %q", h.Text)
			}
			start = i + 1
		}
	}
	finish(len(lines))
	if t.ID == "code" {
		lines := strings.Split(out["code"], "\n")
		if len(lines) >= 2 {
			_, run, _ := fenceMarker(strings.TrimSpace(lines[0]))
			if run > 0 {
				out["code"] = strings.Join(lines[1:len(lines)-1], "\n")
			}
		}
	}
	return out, nil
}

// SectionText resolves the stable section key independently of its human heading.
func SectionText(body, key string) string {
	for _, span := range authoredSpans(body) {
		if span.sectionKey == key || span.anchor == key {
			return span.text
		}
	}
	return ""
}

func SectionContent(body, key string) string { return SectionText(body, key) }
