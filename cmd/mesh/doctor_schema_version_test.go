// SPDX-License-Identifier: LicenseRef-Mesh-Sustainable-Use-License
// Copyright (C) 2026 Bright Interaction AB

package main

import (
	"database/sql"
	"fmt"
	"path/filepath"
	"strings"
	"testing"

	"github.com/bright-interaction/mesh/internal/shellpath"
)

// ageIndexForTest rewrites an index into the shape a previously released Mesh left behind:
// an older schema_version stamp, minus the tables that version did not have. The sqlite
// driver is registered by internal/index, which this package already imports.
func ageIndexForTest(t *testing.T, vaultRoot string, version int, dropTables ...string) {
	t.Helper()
	db, err := sql.Open("sqlite", filepath.Join(vaultRoot, ".mesh", "mesh.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	for _, tbl := range dropTables {
		if _, err := db.Exec("DROP TABLE IF EXISTS " + tbl); err != nil {
			t.Fatalf("drop %s: %v", tbl, err)
		}
	}
	if _, err := db.Exec(`UPDATE meta SET value=? WHERE key='schema_version'`, fmt.Sprint(version)); err != nil {
		t.Fatal(err)
	}
}

// TestDoctorReportsAnIndexFromAnOlderMesh: doctor is the command a stranger runs to find
// out what is wrong, so it is the one command that must not lie. Against an index written
// by an older Mesh (schema_version 5, no dropped_notes table, which is exactly what
// `go install @latest` left on every existing user's disk) it printed
//
//	status: OK (index fresh)   rc=0
//
// because the version comparison lived only on the WRITABLE open path and doctor, like
// every other shipped surface, opens read-only. It must name the mismatch and the rebuild.
func TestDoctorReportsAnIndexFromAnOlderMesh(t *testing.T) {
	dir := t.TempDir()
	writeNote(t, dir, "good.md", goodNote)
	if _, err := runCLI(t, indexCmd(), dir); err != nil {
		t.Fatalf("mesh index: %v", err)
	}
	ageIndexForTest(t, dir, 5, "dropped_notes")

	out, err := runCLI(t, doctorCmd(), dir)
	if err == nil {
		t.Errorf("doctor exited 0 against an index it cannot read correctly\n%s", out)
	}
	for _, want := range []string{"status: BROKEN", "mesh index " + shellpath.Quote(dir)} {
		if !strings.Contains(out, want) {
			t.Errorf("doctor output missing %q:\n%s", want, out)
		}
	}
	for _, unwanted := range []string{"status: OK", "status: healthy"} {
		if strings.Contains(out, unwanted) {
			t.Errorf("doctor still reports %q over an unreadable index:\n%s", unwanted, out)
		}
	}
}
