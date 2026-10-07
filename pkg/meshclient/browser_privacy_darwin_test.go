//go:build darwin

// SPDX-License-Identifier: LicenseRef-Mesh-Sustainable-Use-License
// Copyright (C) 2026 Bright Interaction AB
//
package meshclient

import (
	"context"
	"os/exec"
	"os/user"
	"path/filepath"
	"testing"
)

func TestPrepareBrowserLoginRejectsExtendedPrivateACL(t *testing.T) {
	for _, rel := range []string{".", ".mesh", ".mesh/credentials"} {
		t.Run(rel, func(t *testing.T) {
			dir := t.TempDir()
			writeBrowserFixture(t, dir, "https://fixture.example")
			current, err := user.Current()
			if err != nil {
				t.Fatal(err)
			}
			command := exec.Command("/bin/chmod", "+a", "user:"+current.Username+" allow read", filepath.Join(dir, rel))
			if output, err := command.CombinedOutput(); err != nil {
				t.Fatalf("synthetic ACL:%v %s", err, output)
			}
			var calls int
			_, err = prepareBrowserLogin(context.Background(), dir, "vault", func(string, string) *Client { calls++; return nil })
			assertNoBrowserSecret(t, err)
			if calls != 0 {
				t.Fatal("unsafe ACL reached HTTP")
			}
		})
	}
}
