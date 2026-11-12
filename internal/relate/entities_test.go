// SPDX-License-Identifier: LicenseRef-Mesh-Sustainable-Use-License
// Copyright (C) 2026 Bright Interaction AB

package relate

import (
	"testing"

	"github.com/bright-interaction/mesh/internal/graph"
)

func TestEntityEvidenceUsesCurrentReadableUnambiguousNamedEntities(t *testing.T) {
	g := graph.New()
	for id, title := range map[string]string{"crm": "CRM", "workspace": "Google Workspace", "mesh": "Mesh", "private": "PrivateProduct"} {
		g.AddNode(&graph.Node{ID: "note:" + id, Kind: "note", NoteID: id, Label: title, Attrs: map[string]any{"type": "entity"}})
	}
	read := func(n *graph.Node) (string, bool) { return n.Label, n.NoteID != "private" }
	evidence := EntityEvidence("CRM integrates with Google Workspace. PrivateProduct is mentioned. Mesh is discussed.", g, read)
	for _, target := range []struct {
		id, text string
		want     bool
	}{
		{"crm", "", true},
		{"peer", "A CRM integration decision", true},
		{"peer", "Google Workspace mail delivery", true},
		{"peer", "NotCRM is a different identifier", false},
		{"peer", "OtherProduct CRM implementation", false},
		{"private", "PrivateProduct", false},
		{"mesh", "Mesh", false},
	} {
		if got := evidence(target.id, target.text); got != target.want {
			t.Errorf("%q/%q evidence=%v want %v", target.id, target.text, got, target.want)
		}
	}
	// An inaccessible duplicate still makes the shared display name ambiguous;
	// it must never silently choose the remaining record as canonical.
	g.AddNode(&graph.Node{ID: "note:duplicate", Kind: "note", NoteID: "duplicate", Label: "CRM", Attrs: map[string]any{"type": "entity"}})
	if EntityEvidence("CRM", g, read)("crm", "") {
		t.Fatal("ambiguous canonical entity name accepted")
	}
	if EntityEvidence("Google Workspace", g, func(n *graph.Node) (string, bool) { return "Changed Title", true })("workspace", "") {
		t.Fatal("stale entity title became evidence")
	}
}
