// SPDX-License-Identifier: LicenseRef-Mesh-Sustainable-Use-License
// Copyright (C) 2026 Bright Interaction AB

package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/signal"
	"syscall"

	"github.com/bright-interaction/mesh/internal/desktop"
)

func main() {
	if run(os.Args[1:], os.Stdin, os.Stdout, os.Stderr) != nil {
		fmt.Fprintln(os.Stderr, "Mesh desktop core unavailable")
		os.Exit(1)
	}
}
func run(args []string, in io.Reader, out, stderr io.Writer) error {
	flags := flag.NewFlagSet("mesh-desktop-core", flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	stdio := flags.Bool("stdio", false, "")
	root := flags.String("vault", "", "")
	version := flags.Bool("version", false, "")
	asJSON := flags.Bool("json", false, "")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if flags.NArg() != 0 {
		return fmt.Errorf("unexpected arguments")
	}
	if *version && *asJSON && !*stdio && *root == "" {
		return json.NewEncoder(out).Encode(desktop.IdentityInfo())
	}
	if !*stdio || *root == "" || *version || *asJSON {
		return fmt.Errorf("invalid mode")
	}
	// stdout is exclusively the framed protocol; legacy operation logs stay on the
	// private diagnostic stream, which the shell never forwards to the renderer.
	slog.SetDefault(slog.New(slog.NewTextHandler(stderr, &slog.HandlerOptions{Level: slog.LevelError})))
	_ = os.Unsetenv("MESH_WEB_DEV")
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()
	engine, err := desktop.New(ctx, *root)
	if err != nil {
		return err
	}
	return engine.Serve(ctx, in, out)
}
