// SPDX-License-Identifier: LicenseRef-Mesh-Sustainable-Use-License
// Copyright (C) 2026 Bright Interaction AB
package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"sync/atomic"
	"testing"
	"time"

	"github.com/bright-interaction/mesh/internal/index"
	"github.com/bright-interaction/mesh/internal/vault"
)

func TestOrphanPlanningBoundsConcurrencyAndKeepsSourceOrder(t *testing.T) {
	sources := make([]index.StructureFinding, 24)
	for i := range sources {
		sources[i].Path = fmt.Sprintf("source-%02d", i)
	}
	entered := make(chan struct{}, 24)
	release := make(chan struct{})
	finished := make(chan []orphanCandidatePlan, 1)
	var active, maximum atomic.Int32
	go func() {
		plans, err := planOrphanCandidates(context.Background(), sources, 4, func(_ context.Context, f index.StructureFinding) (string, []string, error) {
			n := active.Add(1)
			defer active.Add(-1)
			for old := maximum.Load(); n > old; old = maximum.Load() {
				if maximum.CompareAndSwap(old, n) {
					break
				}
			}
			entered <- struct{}{}
			<-release
			if f.Path == "source-03" {
				return f.Path, nil, errRepairDraft
			}
			return f.Path, []string{"target-for-" + f.Path}, nil
		})
		if err != nil {
			finished <- nil
			return
		}
		finished <- plans
	}()
	for i := 0; i < 4; i++ {
		select {
		case <-entered:
		case <-time.After(5 * time.Second):
			t.Fatal("readers did not overlap")
		}
	}
	if active.Load() != 4 {
		t.Fatal("wrong active reader count")
	}
	close(release)
	plans := <-finished
	if len(plans) != len(sources) || maximum.Load() != 4 || active.Load() != 0 {
		t.Fatal("unbounded or unfinished workers")
	}
	for i, p := range plans {
		if p.sourceID != sources[i].Path {
			t.Fatal("source order changed")
		}
		if i == 3 {
			if !errors.Is(p.err, errRepairDraft) {
				t.Fatal("source error lost")
			}
			continue
		}
		if !reflect.DeepEqual(p.links, []string{"target-for-" + sources[i].Path}) {
			t.Fatal("candidate assigned to another source")
		}
	}
}

func TestOrphanPlanningCancellationJoinsEveryReader(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	entered := make(chan struct{}, 4)
	done := make(chan error, 1)
	var active atomic.Int32
	go func() {
		_, err := planOrphanCandidates(ctx, make([]index.StructureFinding, 24), 4, func(ctx context.Context, _ index.StructureFinding) (string, []string, error) {
			active.Add(1)
			defer active.Add(-1)
			entered <- struct{}{}
			<-ctx.Done()
			return "", nil, ctx.Err()
		})
		done <- err
	}()
	for i := 0; i < 4; i++ {
		select {
		case <-entered:
		case <-time.After(5 * time.Second):
			t.Fatal("readers did not enter")
		}
	}
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) || active.Load() != 0 {
			t.Fatal("cancel returned before workers stopped", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("worker leak")
	}
}

func TestParallelLocalRepairMatchesSerialAndPreservesDraftsAndAudiences(t *testing.T) {
	fixture := map[string]string{
		"canonical.md": "---\nid: canonical\ntype: entity\ntitle: MeridianCRM\nscope: [dev, sales]\nupdated: 2026-10-01\n---\nA canonical application.\n",
		"private.md":   "---\nid: private\ntype: note\ntitle: MeridianCRM details private\nscope: [dev]\ntags: [meridian]\nupdated: 2026-10-01\n---\nPrivate administration.\n",
		"draft.md":     "---\nid: draft\ntype: note\ntitle: MeridianCRM draft\nstatus: draft\nscope: [dev, sales]\ntags: [meridian]\nupdated: 2026-10-01\n---\nAn unfinished capture.\n",
		"explicit.md":  "---\nid: explicit\ntype: note\ntitle: MeridianCRM explicit opt-out\nscope: [dev, sales]\ntags: [meridian]\nrelated: []\nupdated: 2026-10-01\n---\nAn intentional empty relationship list.\n",
	}
	for i := 0; i < 12; i++ {
		id := fmt.Sprintf("source-%02d", i)
		fixture[id+".md"] = fmt.Sprintf("---\nid: %s\ntype: note\ntitle: MeridianCRM administration %02d\nscope: [dev, sales]\ntags: [meridian]\nupdated: 2026-10-01\n---\nKnowledge about MeridianCRM.\n", id, i)
	}
	paths := make([]string, 0, len(fixture))
	for p := range fixture {
		paths = append(paths, p)
	}
	sort.Strings(paths)
	run := func(workers int) map[string][]string {
		t.Helper()
		root := t.TempDir()
		for _, p := range paths {
			if err := os.WriteFile(filepath.Join(root, p), []byte(fixture[p]), 0600); err != nil {
				t.Fatal(err)
			}
		}
		for _, args := range [][]string{{"index", root}, {"structure", root, "--wire-orphans", "--local-only", "--repair-workers", fmt.Sprint(workers), "--apply"}} {
			cmd := rootCmd()
			cmd.SetArgs(args)
			if err := cmd.Execute(); err != nil {
				t.Fatal(args, err)
			}
		}
		result := map[string][]string{}
		for _, p := range paths {
			data, err := os.ReadFile(filepath.Join(root, p))
			if err != nil {
				t.Fatal(err)
			}
			if p == "draft.md" || p == "explicit.md" {
				if string(data) != fixture[p] {
					t.Fatalf("preserved source changed: %s", p)
				}
			}
			h, body, _ := vault.SplitFrontmatter(string(data))
			fm, raw, err := vault.ParseFrontmatter([]byte(h))
			if err != nil {
				t.Fatal(err)
			}
			h0, b0, _ := vault.SplitFrontmatter(fixture[p])
			_, raw0, err := vault.ParseFrontmatter([]byte(h0))
			if err != nil {
				t.Fatal(err)
			}
			delete(raw, "related")
			delete(raw0, "related")
			if body != b0 || !reflect.DeepEqual(raw, raw0) {
				t.Fatalf("body/unrelated metadata changed: %s", p)
			}
			result[fm.ID] = []string(fm.Related)
			if len(p) >= 7 && p[:7] == "source-" {
				if len(fm.Related) == 0 {
					t.Fatal("eligible source stayed disconnected")
				}
				for _, id := range fm.Related {
					if id == "private" || id == "draft" {
						t.Fatal("private/draft target linked")
					}
				}
			}
		}
		return result
	}
	if a, b := run(1), run(4); !reflect.DeepEqual(a, b) {
		t.Fatal("parallel relationship choices differ", a, b)
	}
}

func TestParallelRepairRejectsUnboundedOrRemoteFanout(t *testing.T) {
	for _, flags := range [][]string{{"--wire-orphans", "--local-only", "--repair-workers", "0"}, {"--wire-orphans", "--local-only", "--repair-workers", "5"}, {"--wire-orphans", "--repair-workers", "4"}, {"--wire-orphans", "--local-only", "--allow-uncorroborated-top", "--repair-workers", "4"}} {
		cmd := rootCmd()
		cmd.SetArgs(append([]string{"structure", t.TempDir()}, flags...))
		if err := cmd.Execute(); err == nil {
			t.Fatal("unsafe workers accepted", flags)
		}
	}
}
