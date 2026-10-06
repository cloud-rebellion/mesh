// SPDX-License-Identifier: LicenseRef-Mesh-Sustainable-Use-License
// Copyright (C) 2026 Bright Interaction AB

package main

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/bright-interaction/mesh/internal/index"
	"github.com/bright-interaction/mesh/internal/mcp"
)

func TestHTTPBackgroundWatcherIsJoinedBeforeShutdownReturns(t *testing.T) {
	started := make(chan struct{})
	cancelled := make(chan struct{})
	release := make(chan struct{})
	stop := startMCPBackgroundWatch(func(ctx context.Context) {
		close(started)
		<-ctx.Done()
		close(cancelled)
		<-release
	})

	select {
	case <-started:
	case <-time.After(3 * time.Second):
		t.Fatal("background watcher did not start")
	}
	stopped := make(chan struct{})
	go func() {
		stop()
		close(stopped)
	}()
	select {
	case <-cancelled:
	case <-time.After(3 * time.Second):
		t.Fatal("stop did not cancel the watcher")
	}
	select {
	case <-stopped:
		t.Fatal("stop returned while the watcher was still using the MCP server")
	default:
	}
	close(release)
	select {
	case <-stopped:
	case <-time.After(3 * time.Second):
		t.Fatal("stop did not join the watcher after it exited")
	}
}

type mcpCloseObservedListener struct {
	net.Listener
	closed chan struct{}
	once   sync.Once
}

func (l *mcpCloseObservedListener) Close() error {
	err := l.Listener.Close()
	l.once.Do(func() { close(l.closed) })
	return err
}

func mcpTestListener(t *testing.T) *mcpCloseObservedListener {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = l.Close() })
	return &mcpCloseObservedListener{Listener: l, closed: make(chan struct{})}
}

func mcpAwait(t *testing.T, ch <-chan struct{}, what string) {
	t.Helper()
	select {
	case <-ch:
	case <-time.After(10 * time.Second):
		t.Fatalf("timed out waiting for %s", what)
	}
}

func mcpServeResult(t *testing.T, ch <-chan error) error {
	t.Helper()
	select {
	case err := <-ch:
		return err
	case <-time.After(10 * time.Second):
		t.Fatal("HTTP lifecycle did not return")
		return nil
	}
}

// Keep the lifecycle fixture complete so a receipt proves the admitted write
// reached durable publication rather than merely testing argument rejection.
func mcpDrainWritePayload(title string) string {
	b, err := json.Marshal(map[string]any{
		"jsonrpc": "2.0", "id": 1, "method": "tools/call",
		"params": map[string]any{"name": "mesh_append_note", "arguments": map[string]any{
			"template": "decision", "template_version": 1, "title": title,
			"summary": "The fixture checks that an admitted write finishes before shutdown closes its owner.",
			"sections": map[string]string{
				"context":      "The test shuts down HTTP admission while a note write is in flight.",
				"options":      "Cancel the admitted write or allow it to finish during the drain grace period.",
				"decision":     "Allow the admitted write to finish before closing the watcher and store.",
				"rationale":    "The caller needs a durable receipt during a controlled restart.",
				"consequences": "New admission stops first; an expiry still bounds shutdown.",
			},
		}},
	})
	if err != nil {
		panic(err)
	}
	return string(b)
}

func TestMCPHTTPDrainCompletesWriteBeforeWatcherAndStoreClose(t *testing.T) {
	if mcpHTTPDrainGrace <= mcp.OwnerIndexBound {
		t.Fatal("HTTP grace must leave room for a normal write acknowledgement")
	}
	dir := t.TempDir()
	seed, err := index.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	if err := seed.Close(); err != nil {
		t.Fatal(err)
	}
	owner, err := mcp.NewOwningServer(dir, "HTTP drain test")
	if err != nil {
		t.Fatal(err)
	}
	if err := owner.WaitReady(); err != nil {
		_ = owner.Close()
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	listener := mcpTestListener(t)
	admitted, release := make(chan struct{}), make(chan struct{})
	var releaseOnce sync.Once
	unblock := func() { releaseOnce.Do(func() { close(release) }) }
	defer unblock()
	httpSrv := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		close(admitted)
		<-release
		if err := r.Context().Err(); err != nil {
			t.Errorf("admitted write canceled during grace: %v", err)
		}
		owner.HandleHTTP(w, r)
	})}
	watchCanceled, releaseWatch := make(chan struct{}), make(chan struct{})
	var watchOnce sync.Once
	unblockWatch := func() { watchOnce.Do(func() { close(releaseWatch) }) }
	defer unblockWatch()
	stopWatch := startMCPBackgroundWatch(func(ctx context.Context) {
		<-ctx.Done()
		close(watchCanceled)
		<-releaseWatch
	})
	done := make(chan error, 1)
	finished := make(chan struct{})
	go func() {
		defer close(finished)
		serveErr := serveMCPHTTPListener(ctx, httpSrv, listener, mcpHTTPDrainGrace)
		stopWatch()
		done <- errors.Join(serveErr, owner.Close())
	}()
	defer func() {
		cancel()
		unblock()
		unblockWatch()
		mcpAwait(t, finished, "test server cleanup")
	}()
	body := mcpDrainWritePayload("HTTP drain preserves admitted write")
	response := make(chan string, 1)
	go func() {
		client := &http.Client{Timeout: 15 * time.Second}
		resp, err := client.Post("http://"+listener.Addr().String()+"/mcp", "application/json", strings.NewReader(body))
		if err != nil {
			response <- "HTTP error: " + err.Error()
			return
		}
		defer resp.Body.Close()
		b, _ := io.ReadAll(resp.Body)
		response <- string(b)
	}()
	mcpAwait(t, admitted, "write admission")
	cancel()
	mcpAwait(t, listener.closed, "shutdown closing the listener")
	select {
	case <-watchCanceled:
		t.Fatal("watcher canceled before admitted write finished")
	default:
	}
	unblock()
	var result struct {
		Result struct {
			Content []struct{ Text string }
			IsError bool
		}
		Error json.RawMessage
	}
	select {
	case raw := <-response:
		if err := json.Unmarshal([]byte(raw), &result); err != nil || len(result.Error) > 0 || result.Result.IsError || len(result.Result.Content) == 0 {
			t.Fatalf("write failed during drain: %s (%v)", raw, err)
		}
	case <-time.After(15 * time.Second):
		t.Fatal("write receipt did not finish")
	}
	var receipt struct {
		ID         string `json:"id"`
		Path       string `json:"path"`
		IndexStale bool   `json:"index_stale"`
	}
	if err := json.Unmarshal([]byte(result.Result.Content[0].Text), &receipt); err != nil || receipt.ID == "" || receipt.IndexStale {
		t.Fatalf("write was not acknowledged: %+v (%v)", result, err)
	}
	if _, err := os.Stat(filepath.Join(dir, receipt.Path)); err != nil {
		t.Fatalf("acknowledged note not saved: %v", err)
	}
	mcpAwait(t, watchCanceled, "watcher cancellation after write completion")
	select {
	case err := <-done:
		t.Fatalf("lifecycle returned before watcher joined: %v", err)
	default:
	}
	if !owner.OwnsIndex() {
		t.Fatal("owner closed before watcher joined")
	}
	unblockWatch()
	if err := mcpServeResult(t, done); err != nil {
		t.Fatal(err)
	}
}

func TestMCPHTTPDrainExpiryCancelsAndJoinsHandler(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	listener := mcpTestListener(t)
	admitted, canceled, release := make(chan struct{}), make(chan struct{}), make(chan struct{})
	var once sync.Once
	unblock := func() { once.Do(func() { close(release) }) }
	defer unblock()
	srv := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		close(admitted)
		<-r.Context().Done()
		close(canceled)
		<-release // simulate cleanup still using the store after cancellation
	})}
	done := make(chan error, 1)
	finished := make(chan struct{})
	go func() {
		defer close(finished)
		done <- serveMCPHTTPListener(ctx, srv, listener, 30*time.Millisecond)
	}()
	defer func() {
		cancel()
		unblock()
		mcpAwait(t, finished, "expired server cleanup")
	}()
	clientDone := make(chan struct{})
	go func() {
		defer close(clientDone)
		client := &http.Client{Timeout: 5 * time.Second}
		if resp, err := client.Get("http://" + listener.Addr().String()); err == nil {
			_ = resp.Body.Close()
		}
	}()
	mcpAwait(t, admitted, "handler admission")
	cancel()
	mcpAwait(t, canceled, "request cancellation after grace expires")
	mcpAwait(t, clientDone, "forced connection close")
	select {
	case err := <-done:
		t.Fatalf("returned under a live handler: %v", err)
	default:
	}
	unblock()
	if err := mcpServeResult(t, done); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("lost shutdown deadline error: %v", err)
	}
}

func TestMCPHTTPDrainRejectsLateRequests(t *testing.T) {
	drain := &mcpHTTPRequestDrain{}
	handler := drain.wrap(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		t.Error("late request reached MCP")
	}))
	drain.stop()
	drain.stop() // idempotent
	var requests sync.WaitGroup
	for range 100 {
		requests.Go(func() {
			w := httptest.NewRecorder()
			handler.ServeHTTP(w, httptest.NewRequest(http.MethodPost, "/mcp", nil))
			if w.Code != http.StatusServiceUnavailable || w.Header().Get("Connection") != "close" {
				t.Errorf("late response: %d %v", w.Code, w.Header())
			}
		})
	}
	drain.active.Wait()
	requests.Wait()
}

func TestMCPHTTPDrainHandlesEarlyServeFailure(t *testing.T) {
	listener := mcpTestListener(t)
	if err := listener.Close(); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() {
		done <- serveMCPHTTPListener(context.Background(), &http.Server{Handler: http.NotFoundHandler()}, listener, time.Second)
	}()
	if err := mcpServeResult(t, done); !errors.Is(err, net.ErrClosed) {
		t.Fatalf("lost serve error: %v", err)
	}
}

func TestMCPHTTPAcceptFailureJoinsAdmittedHandler(t *testing.T) {
	listener := mcpTestListener(t)
	admitted, canceled, release := make(chan struct{}), make(chan struct{}), make(chan struct{})
	var once sync.Once
	unblock := func() { once.Do(func() { close(release) }) }
	srv := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		close(admitted)
		<-r.Context().Done()
		close(canceled)
		<-release
	})}
	done := make(chan error, 1)
	finished := make(chan struct{})
	go func() {
		defer close(finished)
		done <- serveMCPHTTPListener(context.Background(), srv, listener, time.Second)
	}()
	defer func() {
		_ = listener.Close()
		unblock()
		mcpAwait(t, finished, "failed listener cleanup")
	}()
	clientDone := make(chan struct{})
	go func() {
		defer close(clientDone)
		client := &http.Client{Timeout: 5 * time.Second}
		if resp, err := client.Get("http://" + listener.Addr().String()); err == nil {
			_ = resp.Body.Close()
		}
	}()
	mcpAwait(t, admitted, "handler before accept failure")
	// Closing just the listener makes Serve fail without closing its active
	// connections. Our lifecycle must cancel and join them on this path too.
	_ = listener.Close()
	mcpAwait(t, canceled, "handler cancellation after accept failure")
	mcpAwait(t, clientDone, "failed server connection close")
	select {
	case err := <-done:
		t.Fatalf("accept failure returned under a live handler: %v", err)
	default:
	}
	unblock()
	if err := mcpServeResult(t, done); !errors.Is(err, net.ErrClosed) {
		t.Fatalf("lost accept failure: %v", err)
	}
}

func TestMCPHTTPDrainWithoutRequests(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel() // cancellation may arrive even before Serve registers the listener
	listener := mcpTestListener(t)
	if err := serveMCPHTTPListener(ctx, &http.Server{Handler: http.NotFoundHandler()}, listener, time.Second); err != nil {
		t.Fatal(err)
	}
	mcpAwait(t, listener.closed, "listener close")
}

// Exercise the production command and real OS signals, not just a canceled test
// context. Expect: 100-continue proves HandleHTTP has started reading this body
// before the signal; the remaining body is sent only after admission is stopped.
func TestMCPHTTPSignalDrainsWrite(t *testing.T) {
	if os.Getenv("MESH_TEST_HTTP_SIGNAL_CHILD") == "1" {
		cmd := rootCmd()
		cmd.SetArgs([]string{"mcp", "--vault", os.Getenv("MESH_TEST_HTTP_SIGNAL_VAULT"), "--http", "127.0.0.1:0", "--watch"})
		if err := cmd.Execute(); err != nil {
			t.Fatal(err)
		}
		return
	}
	if runtime.GOOS == "windows" {
		t.Skip("Unix signal lifecycle")
	}
	for _, sig := range []os.Signal{os.Interrupt, syscall.SIGTERM} {
		t.Run(sig.String(), func(t *testing.T) {
			dir := t.TempDir()
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()
			cmd := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestMCPHTTPSignalDrainsWrite$")
			cmd.Env = append(os.Environ(), "MESH_TEST_HTTP_SIGNAL_CHILD=1", "MESH_TEST_HTTP_SIGNAL_VAULT="+dir, "MESH_MCP_TOKEN=")
			stderr, err := cmd.StderrPipe()
			if err != nil {
				t.Fatal(err)
			}
			if err := cmd.Start(); err != nil {
				t.Fatal(err)
			}
			waited := false
			defer func() {
				if !waited {
					cancel()
					_ = cmd.Wait()
				}
			}()
			address := make(chan string, 1)
			logsDone := make(chan struct{})
			go func() {
				defer close(logsDone)
				scanner := bufio.NewScanner(stderr)
				for scanner.Scan() {
					line := scanner.Text()
					if tail, ok := strings.CutPrefix(line, "mesh mcp: serving HTTP at "); ok {
						addr, _, _ := strings.Cut(tail, "/mcp")
						address <- addr
					}
				}
			}()
			var addr string
			select {
			case addr = <-address:
			case <-ctx.Done():
				t.Fatal("child did not start HTTP")
			}
			conn, err := net.DialTimeout("tcp", addr, time.Second)
			if err != nil {
				t.Fatal(err)
			}
			defer conn.Close()
			_ = conn.SetDeadline(time.Now().Add(15 * time.Second))
			body := mcpDrainWritePayload("Signal drain receipt")
			if _, err := fmt.Fprintf(conn, "POST /mcp HTTP/1.1\r\nHost: %s\r\nContent-Type: application/json\r\nContent-Length: %d\r\nExpect: 100-continue\r\n\r\n", addr, len(body)); err != nil {
				t.Fatal(err)
			}
			reader := bufio.NewReader(conn)
			resp, err := http.ReadResponse(reader, nil)
			if err != nil {
				t.Fatal(err)
			}
			_ = resp.Body.Close()
			if resp.StatusCode != http.StatusContinue {
				t.Fatalf("handler not admitted: %d", resp.StatusCode)
			}
			if err := cmd.Process.Signal(sig); err != nil {
				t.Fatal(err)
			}
			deadline := time.Now().Add(5 * time.Second)
			for {
				probe, err := net.DialTimeout("tcp", addr, 100*time.Millisecond)
				if err != nil {
					break
				}
				_ = probe.Close()
				if time.Now().After(deadline) {
					t.Fatal("signal did not close listener")
				}
				time.Sleep(10 * time.Millisecond)
			}
			if _, err := io.WriteString(conn, body); err != nil {
				t.Fatal(err)
			}
			resp, err = http.ReadResponse(reader, nil)
			if err != nil {
				t.Fatalf("signal interrupted admitted write: %v", err)
			}
			raw, err := io.ReadAll(resp.Body)
			_ = resp.Body.Close()
			if err != nil || resp.StatusCode != http.StatusOK || !strings.Contains(string(raw), `signal-drain-receipt`) || strings.Contains(string(raw), `index_stale`) || strings.Contains(string(raw), `"isError":true`) {
				t.Fatalf("write receipt failed: %s (%v)", raw, err)
			}
			if _, err := os.Stat(filepath.Join(dir, "decisions", "signal-drain-receipt.md")); err != nil {
				t.Fatalf("missing durable note: %v", err)
			}
			mcpAwait(t, logsDone, "child exit")
			err = cmd.Wait()
			waited = true
			if err != nil {
				t.Fatalf("unclean signal shutdown: %v", err)
			}
		})
	}
}
