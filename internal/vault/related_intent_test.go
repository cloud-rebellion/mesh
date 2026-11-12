// SPDX-License-Identifier: LicenseRef-Mesh-Sustainable-Use-License
// Copyright (C) 2026 Bright Interaction AB

package vault

import (
	"context"
	"os"
	"testing"
)

func TestRelatedIntentEmptySurvivesPublicationUpdateAndRepair(t *testing.T) {
	t.Setenv("MESH_AUTHORING_MODE", "enabled")
	root := t.TempDir()
	target, err := CreateNoteContext(context.Background(), root, completeSpec(t, TypeNote, "Known target"))
	if err != nil {
		t.Fatal(err)
	}
	spec := completeSpec(t, TypeNote, "Deliberately unlinked note")
	spec.Related = []string{}
	result, err := CreateNoteContext(context.Background(), root, spec)
	if err != nil {
		t.Fatal(err)
	}
	check := func(path string) {
		t.Helper()
		data, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		header, _, _ := SplitFrontmatter(string(data))
		fm, raw, err := ParseFrontmatter([]byte(header))
		if err != nil || !declaredKey(raw, "related") || fm.Related == nil || len(fm.Related) != 0 {
			t.Fatal("explicit empty relation intent was erased", string(data), err)
		}
		repair, err := BackfillRelatedFile(path, []string{target.ID}, false)
		if err != nil || repair.Changed {
			t.Fatal("bulk repair overrode explicit none", repair, err)
		}
		after, err := os.ReadFile(path)
		if err != nil || string(after) != string(data) {
			t.Fatal("repair changed explicit relation intent", err)
		}
	}
	check(result.Path)
	snapshot, err := NoteSnapshotContext(context.Background(), root, result.ID)
	if err != nil {
		t.Fatal(err)
	}
	update := snapshot.Spec
	update.Summary += " Updated substantive fixture detail."
	updated, err := CreateNoteContext(context.Background(), root, update)
	if err != nil {
		t.Fatal(err)
	}
	check(updated.Path)
}

func TestRelatedIntentOmittedRemainsEligibleForCorroboratedRepair(t *testing.T) {
	t.Setenv("MESH_AUTHORING_MODE", "enabled")
	root := t.TempDir()
	target, err := CreateNoteContext(context.Background(), root, completeSpec(t, TypeNote, "Known repair target"))
	if err != nil {
		t.Fatal(err)
	}
	spec := completeSpec(t, TypeNote, "Note without supplied relations")
	result, err := CreateNoteContext(context.Background(), root, spec)
	if err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(result.Path)
	if err != nil {
		t.Fatal(err)
	}
	header, _, _ := SplitFrontmatter(string(data))
	_, raw, err := ParseFrontmatter([]byte(header))
	if err != nil || declaredKey(raw, "related") {
		t.Fatal("omission was converted into an explicit no-links decision", err)
	}
	res, err := BackfillRelatedFile(result.Path, []string{target.ID}, false)
	if err != nil || !res.Changed {
		t.Fatal("omitted relation could not be repaired", res, err)
	}
}
