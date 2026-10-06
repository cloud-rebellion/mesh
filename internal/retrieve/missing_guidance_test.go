// SPDX-License-Identifier: LicenseRef-Mesh-Sustainable-Use-License
// Copyright (C) 2026 Bright Interaction AB

package retrieve

import (
	"context"
	"encoding/json"
	"reflect"
	"strings"
	"testing"

	"github.com/bright-interaction/mesh/internal/index"
)

func guidanceSource(kind, fields, scope string) noteSrc {
	return noteSrc{"guidance.md", "---\nid: guidance\ntitle: Guidanceneedle\ntype: " + kind + "\nwhen: 2026-09-23\nscope: [" + scope + "]\n" + fields + "---\n# Guidanceneedle\nSome historical context remains.\n"}
}

func assertMissingGuidance(t *testing.T, value any, want []string) {
	t.Helper()
	b, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	var got struct{ MissingGuidance []string }
	if err := json.Unmarshal(b, &got); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got.MissingGuidance, want) {
		t.Fatalf("MissingGuidance = %v, want %v; wire: %s", got.MissingGuidance, want, b)
	}
	if want == nil && strings.Contains(string(b), `"MissingGuidance"`) {
		t.Fatalf("complete/ordinary card pays for empty warning: %s", b)
	}
}

func TestMissingGuidanceCurrentCardsAndDocuments(t *testing.T) {
	const complete = "do: Use current evidence\ndont: Infer deployment from a title\nwhy: History is not current verification\n"
	for _, tc := range []struct {
		name, kind, fields string
		want               []string
	}{
		{"complete", "decision", complete, nil},
		{"empty historical note", "gotcha", "", nil},
		{"placeholders", "post-mortem", "do: '  todo: fill this  '\ndont: '\u2003'\nwhy: TODO\n", []string{"legacy do contains an unfilled placeholder", "legacy dont contains an unfilled placeholder", "legacy why contains an unfilled placeholder"}},
		{"partial", "decision", "do: todos are recorded\ndont: Avoid guesses\nwhy: TODO\n", []string{"legacy why contains an unfilled placeholder"}},
		{"comment only", "decision", "do: '<!-- pending -->'\ndont: Avoid guesses\nwhy: Evidence matters\n", []string{"legacy do contains an unfilled placeholder"}},
		{"ordinary note", "note", "", nil},
		{"entity", "entity", "", nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			// Keep the graph from the complete note while replacing the persisted
			// metadata. Both ordinary and rerank-document paths must see the edit.
			r := buildVaultFrom(t, []noteSrc{guidanceSource("decision", complete, "public")})
			src := guidanceSource(tc.kind, tc.fields, "public")
			pn, err := index.Parse(src.path, []byte(src.body))
			if err != nil {
				t.Fatal(err)
			}
			g, _ := index.BuildGraph([]*index.ParsedNote{pn}) // unfilled notices are expected
			if _, err := r.store.IndexVault([]*index.ParsedNote{pn}, g); err != nil {
				t.Fatal(err)
			}
			opt := scopedFTSOnlyOptions(map[string]bool{"public": true})
			opt.NoRerank = true
			cards, err := r.Retrieve(context.Background(), "guidanceneedle", opt)
			if err != nil || len(cards) != 1 {
				t.Fatalf("cards = %v, err = %v", cards, err)
			}
			assertMissingGuidance(t, cards[0], tc.want)
			docs, err := r.store.NoteDocuments(context.Background(), []string{"note:guidance"})
			if err != nil {
				t.Fatal(err)
			}
			current, ok := currentCardFromMetadata(docs["note:guidance"].NoteMetadata, opt)
			if !ok {
				t.Fatal("rerank metadata rejected readable note")
			}
			assertMissingGuidance(t, current, tc.want)
			// Repairing the same note clears the warning without a graph refresh
			// or restart, and cannot leave a cached incomplete classification.
			repairedSrc := guidanceSource("decision", complete, "public")
			repaired, err := index.Parse(repairedSrc.path, []byte(repairedSrc.body))
			if err != nil {
				t.Fatal(err)
			}
			repairedGraph, _ := index.BuildGraph([]*index.ParsedNote{repaired})
			if _, err := r.store.IndexVault([]*index.ParsedNote{repaired}, repairedGraph); err != nil {
				t.Fatal(err)
			}
			fixedCards, err := r.Retrieve(context.Background(), "guidanceneedle", opt)
			if err != nil || len(fixedCards) != 1 {
				t.Fatalf("repaired cards = %v, err = %v", fixedCards, err)
			}
			assertMissingGuidance(t, fixedCards[0], nil)
			opt.AllowedScopes = map[string]bool{"secret": true}
			if cards, err := r.Retrieve(context.Background(), "guidanceneedle", opt); err != nil || len(cards) != 0 {
				t.Fatalf("scope-fenced guidance leaked: %v, %v", cards, err)
			}
			opt.AllowedScopes = nil
			opt.AllowPath = func(string) bool { return false }
			if cards, err := r.Retrieve(context.Background(), "guidanceneedle", opt); err != nil || len(cards) != 0 {
				t.Fatalf("path-fenced guidance leaked: %v, %v", cards, err)
			}
		})
	}
}

func TestMissingGuidanceSurvivesRerankingAndCompactBudgets(t *testing.T) {
	r := buildVaultFrom(t, []noteSrc{
		guidanceSource("decision", "do: TODO\ndont: TODO\nwhy: TODO\n", "public"),
		{"other.md", "---\nid: other\ntype: note\n---\n# Guidanceneedle other\nUnrelated history\n"},
	})
	r.EnableRerank(fakeReranker{needle: "some historical"})
	cards, err := r.Retrieve(context.Background(), "guidanceneedle", Options{Limit: 10})
	if err != nil || len(cards) != 2 {
		t.Fatalf("cards = %v, err = %v", cards, err)
	}
	c := cards[0]
	if c.NoteID != "guidance" || !strings.Contains(c.Reason, "reranked") {
		t.Fatalf("did not exercise reranked incomplete head: %+v", cards)
	}
	want := []string{"legacy do contains an unfilled placeholder", "legacy dont contains an unfilled placeholder", "legacy why contains an unfilled placeholder"}
	assertMissingGuidance(t, c, want)
	c.Snippet = strings.Repeat("Long snippet with context. ", 100)
	c.SupersededBy = "replacement"
	for budget := 1; budget <= 400; budget++ {
		packed := PackToBudget([]Card{c}, budget, nil)
		// [] itself has a token cost; the existing budget contract returns it
		// when no card fits. Every returned card must retain both safety fields.
		if len(packed) == 0 {
			continue
		}
		if TotalTokens(packed) > budget {
			t.Fatalf("budget %d exceeded: %d", budget, TotalTokens(packed))
		}
		assertMissingGuidance(t, packed[0], want)
		if packed[0].SupersededBy != "replacement" {
			t.Fatal("packing removed supersession")
		}
	}
	budget := TotalTokens([]Card{compact(c)})
	packed := PackToBudget([]Card{c}, budget, nil)
	if len(packed) != 1 || packed[0].Snippet != "" {
		t.Fatalf("compact control did not return one no-snippet card: %+v", packed)
	}
	if got, wantCost := cardTokensNoMarshal(compact(c)), cardTokens(compact(c)); got < wantCost-2 || got > wantCost+wantCost/5+16 {
		t.Fatalf("fallback counter %d diverged from %d", got, wantCost)
	}
}
