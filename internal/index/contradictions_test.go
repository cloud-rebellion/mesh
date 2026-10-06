// SPDX-License-Identifier: LicenseRef-Mesh-Sustainable-Use-License
// Copyright (C) 2026 Bright Interaction AB

package index

import (
	"fmt"
	"math/rand"
	"reflect"
	"strings"
	"testing"
)

// Keep the original evaluator as an independent oracle, including direction,
// ordering, threshold, case folding, and unordered-pair deduplication.
func legacyContradictionFindings(notes []guidanceRow) []HealthFinding {
	var findings []HealthFinding
	seen := map[string]bool{}
	for i := range notes {
		for j := range notes {
			if i == j || !shareTag(notes[i].tags, notes[j].tags) {
				continue
			}
			if jaccard(tokenSet(notes[i].recommended), tokenSet(notes[j].forbidden)) < 0.6 {
				continue
			}
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
	return findings
}

func TestContradictionFindingsMatchesLegacy(t *testing.T) {
	fixtures := [][]guidanceRow{
		nil,
		{{id: "a", recommended: "alpha beta gamma", forbidden: "alpha beta gamma", tags: []string{"x"}}},
		{
			{id: "a", path: "a.md", recommended: "alpha beta gamma", forbidden: "other guidance", tags: []string{"Mesh", "mesh", ""}},
			{id: "b", path: "b.md", recommended: "other guidance", forbidden: "alpha beta gamma delta epsilon", tags: []string{"MESH", "", "MESH"}}, // exactly .6
			{id: "c", forbidden: "alpha beta gamma delta epsilon zeta", tags: []string{"mesh"}},                                                     // below .6
			{id: "d", forbidden: "alpha beta gamma"}, // no shared tag
			{id: "e", recommended: "the and for", forbidden: "alpha beta gamma", tags: []string{"mesh"}},
			{id: "f", recommended: "'ALPHA' beta, gamma!", forbidden: "other guidance", tags: []string{""}},
		},
	}
	rng := rand.New(rand.NewSource(19))
	phrases := []string{"", "the and for", "alpha beta gamma", "ALPHA, beta gamma delta epsilon", "alpha beta gamma delta epsilon zeta", "always test before deploy", "test before deploy", "keep atomic snapshots", "atomic snapshots"}
	tags := []string{"", "mesh", "Mesh", "deploy", "testing", " mesh "}
	for round := 0; round < 40; round++ {
		notes := make([]guidanceRow, rng.Intn(65))
		for i := range notes {
			n := &notes[i]
			n.id, n.path = fmt.Sprintf("note-%d", i), fmt.Sprintf("%d.md", i)
			n.recommended, n.forbidden = phrases[rng.Intn(len(phrases))], phrases[rng.Intn(len(phrases))]
			for j := rng.Intn(5); j > 0; j-- {
				n.tags = append(n.tags, tags[rng.Intn(len(tags))])
			}
		}
		fixtures = append(fixtures, notes)
	}
	for i, notes := range fixtures {
		if got, want := contradictionFindings(notes), legacyContradictionFindings(notes); !reflect.DeepEqual(got, want) {
			t.Fatalf("fixture %d: got %#v, want %#v", i, got, want)
		}
	}
}

func BenchmarkContradictionFindings(b *testing.B) {
	for _, n := range []int{100, 1000, 3000} {
		notes := make([]guidanceRow, n)
		for i := range notes {
			notes[i] = guidanceRow{id: fmt.Sprint(i), recommended: strings.Repeat("preserve committed snapshots and validate note versions before acknowledgement ", 8), forbidden: strings.Repeat("overwrite user files or silently skip verification and report successful completion ", 8), tags: []string{"mesh", fmt.Sprintf("group-%d", i%16)}}
		}
		for _, impl := range []struct {
			name string
			run  func([]guidanceRow) []HealthFinding
		}{{"legacy", legacyContradictionFindings}, {"cached", contradictionFindings}} {
			b.Run(fmt.Sprintf("%s/n%d", impl.name, n), func(b *testing.B) {
				b.ReportAllocs()
				for i := 0; i < b.N; i++ {
					impl.run(notes)
				}
			})
		}
	}
}
