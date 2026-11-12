// SPDX-License-Identifier: LicenseRef-Mesh-Sustainable-Use-License
// Copyright (C) 2026 Bright Interaction AB

package mcp

import (
	"context"
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/bright-interaction/mesh/internal/vault"
)

func TestAuthoringRelationReceiptMatchesPublishedNoteAndGraph(t *testing.T) {
	s := newTestServer(t)
	startOwner(t, s.vaultRoot)
	ctx := WithLocalOperator(context.Background())
	publish := func(title string, tags []string, explicit bool) map[string]any {
		t.Helper()
		input := fixtureNote(title)
		input["tags"] = tags
		if explicit {
			input["related"] = []string{}
		}
		result, err := s.toolAuthorNote(ctx, mustJSON(map[string]any{"action": "publish", "note": input}))
		if err != nil {
			t.Fatal(err)
		}
		receipt := toolJSON(t, result)
		if receipt["index_stale"] == true {
			t.Fatalf("publication did not reach the existing owner: %v", receipt)
		}
		return receipt
	}
	read := func(receipt map[string]any) *vault.Frontmatter {
		t.Helper()
		data, err := os.ReadFile(filepath.Join(s.vaultRoot, receipt["path"].(string)))
		if err != nil {
			t.Fatal(err)
		}
		header, _, _ := vault.SplitFrontmatter(string(data))
		fm, _, err := vault.ParseFrontmatter([]byte(header))
		if err != nil || fm.ID != receipt["id"] {
			t.Fatalf("saved identity differs: %v %v", fm, err)
		}
		return fm
	}
	seed := publish("Mesh relation receipt synthetic seed", []string{"relation-receipt-fixture"}, true)
	created := publish("Mesh relation receipt synthetic neighbour", []string{"relation-receipt-fixture"}, false)
	want := []string{seed["id"].(string)}
	if created["relation_state"] != "derived" || !reflect.DeepEqual([]string(read(created).Related), want) {
		t.Fatalf("derived receipt and durable note disagree: %v", created)
	}
	items, ok := created["related"].([]any)
	if !ok || len(items) != 1 || items[0] != want[0] {
		t.Fatalf("confirmed reference missing from receipt: %v", created)
	}
	g, _ := s.snapshot()
	found := false
	for _, edge := range g.Neighbors("note:" + created["id"].(string)) {
		if edge.Target == "note:"+want[0] && edge.Relation == "references" {
			found = true
		}
	}
	if !found {
		t.Fatal("confirmed durable relation absent from acknowledged graph")
	}
	preparedResult, rpcErr := s.toolPrepareUpdate(ctx, mustJSON(map[string]any{"id": created["id"]}))
	if rpcErr != nil {
		t.Fatal(rpcErr)
	}
	update := toolJSON(t, preparedResult)["note"].(map[string]any)
	update["related"] = []string{}
	clearedResult, rpcErr := s.toolAuthorNote(ctx, mustJSON(map[string]any{"action": "publish", "note": update}))
	if rpcErr != nil {
		t.Fatal(rpcErr)
	}
	cleared := toolJSON(t, clearedResult)
	if cleared["relation_state"] != "explicit" || read(cleared).Related == nil || len(read(cleared).Related) != 0 {
		t.Fatalf("explicit clearing through authoring API was not durable: %v", cleared)
	}
	g, _ = s.snapshot()
	for _, edge := range g.Neighbors("note:" + created["id"].(string)) {
		if edge.Target == "note:"+want[0] && edge.Relation == "references" {
			t.Fatal("cleared derived relation remains in acknowledged graph")
		}
	}
	explicit := publish("Mesh relation receipt synthetic explicit empty", []string{"relation-receipt-fixture"}, true)
	if explicit["relation_state"] != "explicit" || len(read(explicit).Related) != 0 {
		t.Fatalf("explicit empty choice was changed: %v", explicit)
	}
	unconfirmed := publish("Tungsten helicopter ledger synthetic fixture", nil, false)
	if unconfirmed["relation_state"] != "unconfirmed" || unconfirmed["relation_notice"] == nil || len(read(unconfirmed).Related) != 0 {
		t.Fatalf("unconfirmed relation was misreported or fabricated: %v", unconfirmed)
	}
}

func TestAuthoringRelationReceiptDraftRoundTripPreservesIntent(t *testing.T) {
	for _, explicit := range []bool{false, true} {
		name := "omitted"
		if explicit {
			name = "explicit-empty"
		}
		t.Run(name, func(t *testing.T) {
			s := newTestServer(t)
			startOwner(t, s.vaultRoot)
			ctx := WithLocalOperator(context.Background())
			seedInput := fixtureNote("Mesh draft relation intent seed")
			seedInput["tags"] = []string{"draft-relation-intent-fixture"}
			seedInput["related"] = []string{}
			seedResult, rpcErr := s.toolAuthorNote(ctx, mustJSON(map[string]any{"action": "publish", "note": seedInput}))
			if rpcErr != nil {
				t.Fatal(rpcErr)
			}
			seed := toolJSON(t, seedResult)
			input := fixtureNote("Mesh draft relation intent round trip")
			input["tags"] = []string{"draft-relation-intent-fixture"}
			if explicit {
				input["related"] = []string{}
			}
			draftedResult, rpcErr := s.toolAuthorNote(ctx, mustJSON(map[string]any{"action": "draft", "note": input}))
			if rpcErr != nil {
				t.Fatal(rpcErr)
			}
			drafted := toolJSON(t, draftedResult)
			preparedResult, rpcErr := s.toolPrepareUpdate(ctx, mustJSON(map[string]any{"id": drafted["id"], "draft": true}))
			if rpcErr != nil {
				t.Fatal(rpcErr)
			}
			prepared := toolJSON(t, preparedResult)
			note := prepared["note"].(map[string]any)
			value, present := note["related"]
			if explicit {
				list, ok := value.([]any)
				if !present || !ok || len(list) != 0 {
					t.Fatalf("prepared draft lost explicit empty choice: %v", note)
				}
			} else if present {
				t.Fatalf("omitted relation intent was replaced with a value: %v", value)
			}
			publishedResult, rpcErr := s.toolAuthorNote(ctx, mustJSON(map[string]any{"action": "publish", "note": note}))
			if rpcErr != nil {
				t.Fatal(rpcErr)
			}
			published := toolJSON(t, publishedResult)
			data, err := os.ReadFile(filepath.Join(s.vaultRoot, published["path"].(string)))
			if err != nil {
				t.Fatal(err)
			}
			header, _, _ := vault.SplitFrontmatter(string(data))
			fm, _, err := vault.ParseFrontmatter([]byte(header))
			if err != nil {
				t.Fatal(err)
			}
			if explicit {
				if fm.Related == nil || len(fm.Related) != 0 || published["relation_state"] != "explicit" {
					t.Fatalf("draft opt-out changed on publication: %v %v", fm.Related, published)
				}
			} else {
				found := false
				for _, id := range fm.Related {
					if id == seed["id"].(string) {
						found = true
					}
				}
				if !found || published["relation_state"] != "derived" {
					t.Fatalf("omitted draft did not derive its corroborated seed: %v %v", fm.Related, published)
				}
			}
		})
	}
}
