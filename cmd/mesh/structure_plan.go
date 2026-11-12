// SPDX-License-Identifier: LicenseRef-Mesh-Sustainable-Use-License
// Copyright (C) 2026 Bright Interaction AB

package main

import (
	"context"
	"errors"
	"fmt"
	"sync"

	"github.com/bright-interaction/mesh/internal/index"
)

var errRepairDraft = errors.New("draft notes are excluded from relation repair")

type orphanCandidatePlan struct {
	sourceID string
	links    []string
	err      error
}

// planOrphanCandidates only calculates candidates. Results retain source order;
// callers must revalidate current files and perform all writes sequentially.
func planOrphanCandidates(ctx context.Context, sources []index.StructureFinding, workers int, calculate func(context.Context, index.StructureFinding) (string, []string, error)) ([]orphanCandidatePlan, error) {
	if workers < 1 || workers > 4 {
		return nil, fmt.Errorf("repair workers must be between 1 and 4")
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	plans := make([]orphanCandidatePlan, len(sources))
	jobs := make(chan int)
	var group sync.WaitGroup
	for worker := 0; worker < workers; worker++ {
		group.Add(1)
		go func() {
			defer group.Done()
			for {
				select {
				case <-ctx.Done():
					return
				case position, ok := <-jobs:
					if !ok {
						return
					}
					if ctx.Err() != nil {
						return
					}
					id, links, err := calculate(ctx, sources[position])
					plans[position] = orphanCandidatePlan{sourceID: id, links: links, err: err}
				}
			}
		}()
	}
	for position := range sources {
		select {
		case jobs <- position:
		case <-ctx.Done():
			close(jobs)
			group.Wait()
			return nil, ctx.Err()
		}
	}
	close(jobs)
	group.Wait()
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return plans, nil
}
