// SPDX-License-Identifier: LicenseRef-Mesh-Sustainable-Use-License
// Copyright (C) 2026 Bright Interaction AB
//go:build darwin

package meshclient

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"strings"
	"time"
)

func browserPrivateACL(file *os.File) error {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	command := exec.CommandContext(ctx, "/bin/ls", "-lde", "--", browserPrivatePath(file))
	command.Env = []string{"PATH=/usr/bin:/bin", "LC_ALL=C"}
	output, err := command.Output()
	if err != nil {
		return errBrowserLogin
	}
	lines := strings.Split(strings.TrimSpace(string(output)), "\n")
	fields := strings.Fields(lines[0])
	if len(fields) == 0 || strings.Contains(fields[0], "+") || len(lines) != 1 {
		return errors.New("extended ACL requires review")
	}
	return nil
}
