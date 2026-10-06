// SPDX-License-Identifier: LicenseRef-Mesh-Sustainable-Use-License
// Copyright (C) 2026 Bright Interaction AB
package index

import (
	"fmt"
	"strings"
)

// HasLegacy identifies historical pending content without assigning it a modern
// template meaning. Its presence requires an explicit authored replacement.
func (p PendingNote) HasLegacy() bool { return p.Do != "" || p.Dont != "" || p.Why != "" }

// LegacyReviewText preserves original labels inside a read-only historical view.
// A prohibition must not become incident impact, nor an action become a finding.
func (p PendingNote) LegacyReviewText() string {
	var out strings.Builder
	for _, field := range []struct{ label, text string }{{"do", p.Do}, {"dont", p.Dont}, {"why", p.Why}} {
		if field.text != "" {
			fmt.Fprintf(&out, "Historical %s:\n%s\n\n", field.label, field.text)
		}
	}
	return strings.TrimSpace(out.String())
}
