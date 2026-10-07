// SPDX-License-Identifier: LicenseRef-Mesh-Sustainable-Use-License
// Copyright (C) 2026 Bright Interaction AB

package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"time"

	"github.com/bright-interaction/mesh/internal/shellpath"
	"github.com/bright-interaction/mesh/pkg/meshclient"
	"github.com/mattn/go-isatty"
	"github.com/spf13/cobra"
)

type browserCommandDeps struct {
	prepare func(context.Context, string, string) (meshclient.BrowserLogin, error)
	launch  func(context.Context, string) error
}

func defaultBrowserDeps() browserCommandDeps {
	return browserCommandDeps{meshclient.PrepareBrowserLogin, launchNativeBrowser}
}
func browserCmd(destination string) *cobra.Command {
	return browserCmdWith(destination, defaultBrowserDeps())
}
func browserCmdWith(destination string, deps browserCommandDeps) *cobra.Command {
	var printURL bool
	name := "open"
	short := "Open your joined vault in the browser"
	if destination == "team" {
		name = "team"
		short = "Open your joined vault's team settings in the browser"
	}
	cmd := &cobra.Command{Use: name + " [vault]", Short: short, Args: cobra.MaximumNArgs(1), RunE: func(cmd *cobra.Command, args []string) error {
		return finishBrowserOpen(cmd, vaultArg(args), destination, printURL, deps)
	}}
	cmd.Flags().BoolVar(&printURL, "print-url", false, "print one private two-minute sign-in URL instead of launching a browser; keep it private")
	return cmd
}
func finishBrowserOpen(cmd *cobra.Command, vaultDir, destination string, printURL bool, deps browserCommandDeps) error {
	login, err := deps.prepare(cmd.Context(), vaultDir, destination)
	if err != nil {
		return err
	}
	// The transport and selected-vault loader validate the complete origin/URL
	// contract before this function receives a capability. Never echo opener errors.
	if printURL {
		fmt.Fprintln(cmd.OutOrStdout(), login.URL)
		return nil
	}
	if err = deps.launch(cmd.Context(), login.URL); err != nil {
		return errors.New("browser could not be opened; retry mesh " + map[string]string{"vault": "open", "team": "team"}[destination] + " " + shellpath.Quote(vaultDir) + " --print-url and privately open that short-lived URL")
	}
	fmt.Fprintf(cmd.OutOrStdout(), "Opened %s sign-in in your browser for user %q (%s, client %d); confirm there within two minutes.\n", destination, login.User, login.Role, login.ClientID)
	return nil
}

func launchNativeBrowser(ctx context.Context, url string) error {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	path, args, err := nativeBrowserCommand(url)
	if err != nil {
		return err
	}
	// No shell, BROWSER environment interpolation, credential arguments, stdout or
	// stderr forwarding. Even a native opener's error must not expose the URL.
	return runBrowserCommand(ctx, path, args)
}
func runBrowserCommand(ctx context.Context, path string, args []string) error {
	command := exec.CommandContext(ctx, path, args...)
	command.Env = []string{}
	for _, key := range []string{"PATH", "HOME", "USER", "LOGNAME", "LANG", "LC_ALL", "DISPLAY", "WAYLAND_DISPLAY", "XDG_RUNTIME_DIR", "DBUS_SESSION_BUS_ADDRESS", "SystemRoot", "WINDIR", "APPDATA", "LOCALAPPDATA", "TEMP", "TMP"} {
		if value, ok := os.LookupEnv(key); ok {
			command.Env = append(command.Env, key+"="+value)
		}
	}
	return command.Run()
}

func unixBrowserCommand(platform, url string) (string, []string, error) {
	switch platform {
	case "darwin":
		return "/usr/bin/open", []string{url}, nil
	case "linux":
		return "/usr/bin/xdg-open", []string{url}, nil
	default:
		return "", nil, errors.New("browser opening unavailable on this platform")
	}
}

type joinBrowserDeps struct {
	join        func(string, string, string) (meshclient.Summary, error)
	reconcile   joinIndexReconcile
	interactive func() bool
	browser     browserCommandDeps
}

func defaultJoinBrowserDeps() joinBrowserDeps {
	return joinBrowserDeps{meshclient.JoinVault, reconcileOneShotThroughOwner, func() bool { return isatty.IsTerminal(os.Stdin.Fd()) && isatty.IsTerminal(os.Stdout.Fd()) }, defaultBrowserDeps()}
}
func joinCmdWithBrowser(deps joinBrowserDeps) *cobra.Command {
	var open, noOpen bool
	cmd := &cobra.Command{Use: "join <hub-url> <invite-token> [vault]", Short: "Join a team vault and open it in your browser", Long: "Redeem a one-time invite, securely store this device's credential, and clone the vault. Interactive joins open a short-lived browser sign-in. Use --no-open for scripts or --open explicitly in a noninteractive environment.", Args: cobra.RangeArgs(2, 3), RunE: func(cmd *cobra.Command, args []string) error {
		dir := "."
		if len(args) == 3 {
			dir = args[2]
		}
		sum, err := deps.join(args[0], args[1], dir)
		if err != nil {
			return err
		}
		if err = finishJoin(cmd.Context(), dir, sum, deps.reconcile); err != nil {
			return err
		}
		if noOpen || (!open && !deps.interactive()) {
			return nil
		}
		if err = finishBrowserOpen(cmd, dir, "vault", false, deps.browser); err != nil {
			// The invite has already been redeemed. Preserve the durable join receipt,
			// sanitize any dependency error, and retry only the existing-device handoff.
			return fmt.Errorf("join and sync are complete; browser sign-in failed. Retry mesh open %s after updating the client/hub or resolving browser access. Do not repeat mesh join", shellpath.Quote(dir))
		}
		return nil
	}}
	cmd.Flags().BoolVar(&open, "open", false, "open the joined vault in a browser even when not interactive")
	cmd.Flags().BoolVar(&noOpen, "no-open", false, "join without creating a browser sign-in handoff")
	cmd.MarkFlagsMutuallyExclusive("open", "no-open")
	return cmd
}
