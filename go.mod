module github.com/bright-interaction/mesh

go 1.26.9

require (
	github.com/charmbracelet/bubbles v1.0.0
	github.com/charmbracelet/bubbletea v1.3.10
	github.com/charmbracelet/lipgloss v1.1.0
	github.com/charmbracelet/ssh v0.0.0-20250826160808-ebfa259c7309
	github.com/charmbracelet/wish v1.4.7
	github.com/coreos/go-oidc/v3 v3.19.0
	github.com/fsnotify/fsnotify v1.9.0
	github.com/getsentry/sentry-go v0.47.0
	github.com/gomarkdown/markdown v0.0.0-20260614204949-e08cff860f76
	github.com/klauspost/compress v1.19.1
	github.com/mattn/go-isatty v0.0.20
	github.com/microcosm-cc/bluemonday v1.0.27
	github.com/spf13/cobra v1.8.1
	github.com/spf13/pflag v1.0.5
	github.com/tiktoken-go/tokenizer v0.8.0
	golang.org/x/mod v0.38.0
	golang.org/x/net v0.57.0
	golang.org/x/oauth2 v0.36.0
	golang.org/x/sys v0.47.0
	golang.org/x/text v0.41.0
	gopkg.in/yaml.v3 v3.0.1
	modernc.org/sqlite v1.52.0
)

require (
	github.com/anmitsu/go-shlex v0.0.0-20200514113438-38f4b401e2be // indirect
	github.com/atotto/clipboard v0.1.4 // indirect
	github.com/aymanbagabas/go-osc52/v2 v2.0.1 // indirect
	github.com/aymerick/douceur v0.2.0 // indirect
	github.com/charmbracelet/colorprofile v0.4.1 // indirect
	github.com/charmbracelet/keygen v0.5.3 // indirect
	github.com/charmbracelet/log v0.4.1 // indirect
	github.com/charmbracelet/x/ansi v0.11.6 // indirect
	github.com/charmbracelet/x/cellbuf v0.0.15 // indirect
	github.com/charmbracelet/x/conpty v0.1.0 // indirect
	github.com/charmbracelet/x/errors v0.0.0-20240508181413-e8d8b6e2de86 // indirect
	github.com/charmbracelet/x/input v0.3.4 // indirect
	github.com/charmbracelet/x/term v0.2.2 // indirect
	github.com/charmbracelet/x/termios v0.1.0 // indirect
	github.com/charmbracelet/x/windows v0.2.0 // indirect
	github.com/clipperhouse/displaywidth v0.9.0 // indirect
	github.com/clipperhouse/stringish v0.1.1 // indirect
	github.com/clipperhouse/uax29/v2 v2.5.0 // indirect
	github.com/creack/pty v1.1.21 // indirect
	github.com/dlclark/regexp2/v2 v2.1.0 // indirect
	github.com/dustin/go-humanize v1.0.1 // indirect
	github.com/erikgeiser/coninput v0.0.0-20211004153227-1c3628e74d0f // indirect
	github.com/go-jose/go-jose/v4 v4.1.4 // indirect
	github.com/go-logfmt/logfmt v0.6.0 // indirect
	github.com/google/uuid v1.6.0 // indirect
	github.com/gorilla/css v1.0.1 // indirect
	github.com/inconshreveable/mousetrap v1.1.0 // indirect
	github.com/kr/text v0.2.0 // indirect
	github.com/lucasb-eyer/go-colorful v1.3.0 // indirect
	github.com/mattn/go-localereader v0.0.1 // indirect
	github.com/mattn/go-runewidth v0.0.19 // indirect
	github.com/muesli/ansi v0.0.0-20230316100256-276c6243b2f6 // indirect
	github.com/muesli/cancelreader v0.2.2 // indirect
	github.com/muesli/termenv v0.16.0 // indirect
	github.com/ncruces/go-strftime v1.0.0 // indirect
	github.com/remyoudompheng/bigfft v0.0.0-20230129092748-24d4a6f8daec // indirect
	github.com/rivo/uniseg v0.4.7 // indirect
	github.com/xo/terminfo v0.0.0-20220910002029-abceb7e1c41e // indirect
	golang.org/x/crypto v0.56.0 // indirect
	golang.org/x/exp v0.0.0-20240719175910-8a7402abbf56 // indirect
	modernc.org/libc v1.72.3 // indirect
	modernc.org/mathutil v1.7.1 // indirect
	modernc.org/memory v1.11.0 // indirect
)

// Retracted releases. Both are still fetchable by explicit version (a module proxy entry
// is immutable, so nothing can remove them) but `go get` will no longer SELECT either,
// and it tells the user why. This is the non-destructive alternative to deleting the
// tags: a published tag is product surface, `git checkout v0.6.0` keeps working, and
// every existing checksum stays valid.
retract (
	// The v0.6.0 fixes, with the hook injection only half closed: Guard.Severity still
	// reaches the same comment line raw. Also: the generated hook is fail-open, because
	// a rejected grep pattern exits 2 and reads as "no violations"; brace expansion of
	// an LLM-written glob field is unbounded; the hub's directory hardening is a no-op
	// on any existing deployment.
	v0.6.1
	// mesh guards: Guard.Title is interpolated raw into the generated pre-commit hook,
	// so a newline in a note's title becomes an executable line in a file the product
	// invites you to install. Also: `mesh doctor` exits 1 on a healthy vault, breaking
	// the CI contract the README documents; `mesh migrate` cements the duplicate-id
	// collision it is supposed to resolve; the index is created world-readable.
	v0.6.0
)
