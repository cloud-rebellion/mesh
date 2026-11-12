// SPDX-License-Identifier: LicenseRef-Mesh-Sustainable-Use-License
// Copyright (C) 2026 Bright Interaction AB

package main

import (
	"database/sql"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/bright-interaction/mesh/internal/shellpath"

	"github.com/bright-interaction/mesh/internal/index"
)

func TestHealthRefusesOldSchemaWithoutEmptyingIt(t *testing.T) {
	dir := t.TempDir()
	writeNote(t, dir, "good.md", goodNote)
	if out, err := runCLI(t, indexCmd(), dir); err != nil {
		t.Fatalf("mesh index: %v\n%s", err, out)
	}
	ageIndexForTest(t, dir, index.SchemaVersion-1, "dropped_notes")

	out, err := runCLI(t, healthCmd(), dir)
	if err == nil {
		t.Fatalf("health exited 0 on an old schema:\n%s", out)
	}
	if strings.Contains(out, "vault healthy") {
		t.Fatalf("health called an old, unreadable index healthy:\n%s", out)
	}

	db, err := sql.Open("sqlite", filepath.Join(dir, ".mesh", "mesh.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	var version, notes, droppedTable int
	if err := db.QueryRow(`SELECT CAST(value AS INTEGER) FROM meta WHERE key='schema_version'`).Scan(&version); err != nil {
		t.Fatal(err)
	}
	if err := db.QueryRow(`SELECT count(*) FROM notes`).Scan(&notes); err != nil {
		t.Fatalf("health dropped the old notes table: %v", err)
	}
	if err := db.QueryRow(`SELECT count(*) FROM sqlite_schema WHERE type='table' AND name='dropped_notes'`).Scan(&droppedTable); err != nil {
		t.Fatal(err)
	}
	if version != index.SchemaVersion-1 || notes != 1 || droppedTable != 0 {
		t.Fatalf("health mutated the old index: version=%d notes=%d dropped_notes table=%d", version, notes, droppedTable)
	}
}

func TestHealthRefusesToCallAStaleIndexHealthy(t *testing.T) {
	dir := t.TempDir()
	writeNote(t, dir, "good.md", goodNote)
	if out, err := runCLI(t, indexCmd(), dir); err != nil {
		t.Fatalf("mesh index: %v\n%s", err, out)
	}
	writeNote(t, dir, "new.md", "---\nid: new-note\ntype: note\ntitle: New\nwhen: \"2026-08-26\"\n---\n# New\nnot indexed yet\n")

	out, err := runCLI(t, healthCmd(), dir)
	if err == nil {
		t.Fatalf("health exited 0 with a valid note missing from the index:\n%s", out)
	}
	if !strings.Contains(out, "health: STALE") || !strings.Contains(out, "+1 new") {
		t.Fatalf("health did not explain the stale index:\n%s", out)
	}
	if strings.Contains(out, "vault healthy") {
		t.Fatalf("health printed a clean verdict over stale rows:\n%s", out)
	}
}

func corruptNotesLeafForCLI(t *testing.T, dir string) {
	t.Helper()
	dbPath := filepath.Join(dir, ".mesh", "mesh.db")
	db, err := sql.Open("sqlite", dbPath)
	if err != nil {
		t.Fatal(err)
	}
	var pageSize, pageNo int
	if err := db.QueryRow(`PRAGMA page_size`).Scan(&pageSize); err != nil {
		db.Close()
		t.Fatal(err)
	}
	if err := db.QueryRow(`SELECT pageno FROM dbstat WHERE name='notes' AND pagetype='leaf' ORDER BY pageno LIMIT 1`).Scan(&pageNo); err != nil {
		db.Close()
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	if pageNo <= 1 || pageSize <= 0 {
		t.Fatalf("unsafe corruption target: page=%d size=%d", pageNo, pageSize)
	}
	f, err := os.OpenFile(dbPath, os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	_, writeErr := f.WriteAt(make([]byte, pageSize), int64(pageNo-1)*int64(pageSize))
	syncErr := f.Sync()
	closeErr := f.Close()
	if err := errors.Join(writeErr, syncErr, closeErr); err != nil {
		t.Fatal(err)
	}
}

func TestDoctorFindsCorruptionOutsideTheSchemaPage(t *testing.T) {
	dir := t.TempDir()
	writeNote(t, dir, "good.md", goodNote)
	if out, err := runCLI(t, indexCmd(), dir); err != nil {
		t.Fatalf("mesh index: %v\n%s", err, out)
	}
	corruptNotesLeafForCLI(t, dir)

	out, err := runCLI(t, doctorCmd(), dir)
	if err == nil {
		t.Fatalf("doctor exited 0 over a corrupt data page:\n%s", out)
	}
	for _, want := range []string{"status: BROKEN", "index database is corrupt", "mesh index " + shellpath.Quote(dir)} {
		if !strings.Contains(out, want) {
			t.Errorf("doctor output missing %q:\n%s", want, out)
		}
	}
	if strings.Contains(out, "status: OK") {
		t.Fatalf("doctor printed a clean verdict over corruption:\n%s", out)
	}
}

func TestDoctorRefusesFutureIndexWithoutDowngradeAdvice(t *testing.T) {
	dir := t.TempDir()
	writeNote(t, dir, "good.md", goodNote)
	if out, err := runCLI(t, indexCmd(), dir); err != nil {
		t.Fatalf("mesh index: %v\n%s", err, out)
	}
	db, err := sql.Open("sqlite", filepath.Join(dir, ".mesh", "mesh.db"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`UPDATE meta SET value=? WHERE key='schema_version'`, index.SchemaVersion+1); err != nil {
		db.Close()
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}

	out, err := runCLI(t, doctorCmd(), dir)
	if err == nil {
		t.Fatalf("doctor exited 0 on a future index:\n%s", out)
	}
	if !strings.Contains(out, "upgrade Mesh") || !strings.Contains(out, "leave this index untouched") {
		t.Fatalf("doctor did not give safe future-index advice:\n%s", out)
	}
	if strings.Contains(out, "fix: mesh index "+dir) {
		t.Fatalf("doctor advised a destructive downgrade rebuild:\n%s", out)
	}
}
