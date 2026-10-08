// SPDX-License-Identifier: LicenseRef-Mesh-Sustainable-Use-License
// Copyright (C) 2026 Bright Interaction AB

package vault

import (
	"fmt"
	"regexp"
	"slices"
)

// Only plain dotted historical labels are compatible. Paths, wiki links,
// anchors, whitespace and other malformed reference syntax remain invalid.
var historicalTagLabel = regexp.MustCompile(`^[a-z0-9]+(?:[-.][a-z0-9]+)*$`)

type historicalTagFence struct {
	tags      []string
	id        string
	revision  string
	migration bool
}

func (s NewNoteSpec) retainsHistoricalTags() bool {
	f := s.historicalTags
	if f == nil || !slices.Equal(s.Tags, f.tags) {
		return false
	}
	if f.migration {
		return s.UpdateID == "" && s.UpdateRevision == "" && s.DraftID == "" && s.DraftRevision == ""
	}
	return s.UpdateID != "" && s.UpdateID == f.id && s.UpdateRevision == f.revision
}

func historicalTagID(id string) bool {
	return len(id) <= maxSlugLen && historicalTagLabel.MatchString(id)
}

func normalizeHistoricalSpec(spec NewNoteSpec, tags []string, id, revision string, migration bool) (NewNoteSpec, error) {
	// Replace any prior in-memory fence. An edit cannot carry compatibility from
	// an older snapshot, another identity, or a changed tag list.
	spec.historicalTags = &historicalTagFence{tags: slices.Clone(tags), id: id, revision: revision, migration: migration}
	return NormalizeSpec(spec)
}

// NormalizeUpdateSpec validates an edit against an already-authorized snapshot.
// This is not an access grant: callers must obtain current source bytes with
// their usual access checks. The storage publication path repeats this check
// under its existing lock before rendering or writing.
func NormalizeUpdateSpec(spec NewNoteSpec, before *NoteSnapshot) (NewNoteSpec, error) {
	if before == nil || before.Frontmatter == nil || spec.UpdateID == "" || spec.UpdateID != before.Frontmatter.ID ||
		spec.UpdateRevision == "" || spec.UpdateRevision != before.Revision || ContentRevision(before.Content) != before.Revision {
		return spec, fmt.Errorf("%w: note changed or revision missing; prepare the current note again and reconcile edits", ErrInvalidSpec)
	}
	header, _, had := SplitFrontmatter(string(before.Content))
	original, _, err := ParseFrontmatter([]byte(header))
	if err != nil || !had || original.ID != spec.UpdateID || !slices.Equal(original.Tags, before.Frontmatter.Tags) {
		return spec, fmt.Errorf("%w: update snapshot does not match its stored metadata", ErrInvalidSpec)
	}
	return normalizeHistoricalSpec(spec, original.Tags, spec.UpdateID, before.Revision, false)
}
