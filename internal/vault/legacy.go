// SPDX-License-Identifier: LicenseRef-Mesh-Sustainable-Use-License
// Copyright (C) 2026 Bright Interaction AB

package vault

// LegacyContent preserves historical fields under their original meanings.
// In particular, a prohibition is never converted into incident impact or cause.
type LegacyContent struct {
	Do   string
	Dont string
	Why  string
}

func ReadLegacy(fm *Frontmatter) LegacyContent {
	if fm == nil {
		return LegacyContent{}
	}
	return LegacyContent{Do: fm.Do, Dont: fm.Dont, Why: fm.Why}
}

func (l LegacyContent) Values() map[string]string {
	return map[string]string{"do": l.Do, "dont": l.Dont, "why": l.Why}
}

func (l LegacyContent) AuthoredText() []string {
	var out []string
	for _, s := range []string{l.Do, l.Dont, l.Why} {
		if !missingProse(s) {
			out = append(out, s)
		}
	}
	return out
}

// Missing reports explicit historical TODO substance. Absent legacy fields are
// not a universal requirement, and this helper is never modern validation.
func (l LegacyContent) Missing() []string {
	var out []string
	for _, item := range []struct{ key, value string }{{"do", l.Do}, {"dont", l.Dont}, {"why", l.Why}} {
		if item.value != "" && missingProse(item.value) {
			out = append(out, "legacy "+item.key+" contains an unfilled placeholder")
		}
	}
	return out
}

func LegacySearchText(fm *Frontmatter) []string { return ReadLegacy(fm).AuthoredText() }

// LegacyGuidance returns only explicitly authored historical normative fields.
func LegacyGuidance(fm *Frontmatter) (recommended, forbidden, rationale string) {
	l := ReadLegacy(fm)
	if !missingProse(l.Do) {
		recommended = l.Do
	}
	if !missingProse(l.Dont) {
		forbidden = l.Dont
	}
	if !missingProse(l.Why) {
		rationale = l.Why
	}
	return
}
