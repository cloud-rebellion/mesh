// SPDX-License-Identifier: LicenseRef-Mesh-Sustainable-Use-License
// Copyright (C) 2026 Bright Interaction AB

package retrieve

import (
	"context"
	"crypto/sha256"
	"encoding/gob"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/bright-interaction/mesh/internal/latency"
	"github.com/bright-interaction/mesh/internal/meshcfg"
	"github.com/bright-interaction/mesh/internal/rerank"
)

// ConstructionOptions is selected only by a trusted Go server constructor.
// LocalOnly uses indexed lexical/graph knowledge without discovering any local
// provider configuration, subscription profile, environment or stored vectors.
// Normal CLI/server constructors retain their existing configured semantics.
type ConstructionOptions struct{ LocalOnly bool }

// ConfigInputs is the immutable set of local inputs consumed by one retriever
// build. Reuse compares actual consumed values, not file mtimes or before/after
// hashes of files that the constructor might have read at a different instant.
// Neither the inputs (which may include API keys) nor their digest may be logged.
type ConfigInputs struct {
	localOnly   bool
	cfg         meshcfg.Config
	env         map[string]string
	sub         rerank.SubscriptionConfig
	subEnabled  bool
	subErr      error
	fingerprint [sha256.Size]byte
	reusable    bool
}

func snapshotEnvironment() map[string]string {
	out := make(map[string]string)
	for _, entry := range os.Environ() {
		key, value, ok := strings.Cut(entry, "=")
		if ok {
			out[key] = value
		}
	}
	return out
}

// LoadConfigInputs performs local configuration reads only, never inference.
// Errors retain the existing construction fallback/fail-loud behavior but make
// the resulting reader ineligible for reuse until a successful later read.
func LoadConfigInputs(ctx context.Context, meshDir string) (*ConfigInputs, error) {
	return LoadConfigInputsWithOptions(ctx, meshDir, ConstructionOptions{})
}

func LoadConfigInputsWithOptions(ctx context.Context, meshDir string, options ConstructionOptions) (*ConfigInputs, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if options.LocalOnly {
		return &ConfigInputs{localOnly: true, reusable: true, fingerprint: sha256.Sum256([]byte("mesh-local-retrieval-v1"))}, nil
	}
	trace := latency.Start("retriever_config", "local_inputs")
	defer trace.End()
	return loadConfigInputs(ctx, meshDir, rerank.LoadLocalSubscription)
}

type subscriptionLoader func(string) (rerank.SubscriptionConfig, bool, string, error)

func loadConfigInputs(ctx context.Context, meshDir string, loadSubscription subscriptionLoader) (*ConfigInputs, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	in := &ConfigInputs{env: snapshotEnvironment(), reusable: true}
	var err error
	in.cfg, err = meshcfg.LoadConfigContext(ctx, meshDir)
	if err != nil {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		in.cfg, in.reusable = meshcfg.Config{}, false
	}
	in.loadSubscriptionFrom(ctx, filepath.Dir(meshDir), loadSubscription)
	if ctx.Err() != nil {
		return nil, ctx.Err()
	}
	// Hash sorted, length-delimited inputs without JSON's lossy normalization of
	// non-UTF-8 environment bytes or rejection of legacy NaN/Inf config floats.
	// Only the digest survives in the server, never a second copy of API keys.
	keys := make([]string, 0, len(in.env))
	for key := range in.env {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	env := make([][2]string, 0, len(keys))
	for _, key := range keys {
		env = append(env, [2]string{key, in.env[key]})
	}
	hash := sha256.New()
	err = gob.NewEncoder(hash).Encode(struct {
		Config       meshcfg.Config
		Env          [][2]string
		Subscription rerank.SubscriptionConfig
		Enabled      bool
	}{in.cfg, env, in.sub, in.subEnabled})
	if err != nil {
		in.reusable = false // fingerprinting must never break legacy construction
		return in, nil
	}
	copy(in.fingerprint[:], hash.Sum(nil))
	return in, nil
}

func (in *ConfigInputs) Fingerprint() ([sha256.Size]byte, bool) {
	return in.fingerprint, in.reusable
}

func (in *ConfigInputs) getenv(key string) string { return in.env[key] }

func (in *ConfigInputs) envOrFile(key, fallback string) (string, bool) {
	if v := in.getenv(key); v != "" {
		return v, true
	}
	return fallback, false
}

func (in *ConfigInputs) envOr(key, fallback string) string {
	v, _ := in.envOrFile(key, fallback)
	return v
}

func (in *ConfigInputs) loadSubscription(ctx context.Context, root string) {
	in.loadSubscriptionFrom(ctx, root, rerank.LoadLocalSubscription)
}

func (in *ConfigInputs) loadSubscriptionFrom(ctx context.Context, root string, loader subscriptionLoader) {
	if strings.TrimSpace(in.getenv("MESH_RERANK_AGENT")) != "" {
		return
	}
	// The existing loader has bounded file size but its filesystem calls can
	// block. Late results remain private after caller cancellation.
	type result struct {
		sub     rerank.SubscriptionConfig
		enabled bool
		err     error
	}
	read := func() result {
		sub, enabled, _, err := loader(root)
		return result{sub, enabled, err}
	}
	var r result
	if ctx.Done() == nil {
		r = read()
	} else {
		done := make(chan result, 1)
		go func() { done <- read() }()
		select {
		case r = <-done:
		case <-ctx.Done():
			r.err = ctx.Err()
		}
	}
	in.sub, in.subEnabled, in.subErr = r.sub, r.enabled, r.err
	if r.err != nil {
		in.reusable = false
	}
}
