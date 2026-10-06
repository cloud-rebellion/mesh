// SPDX-License-Identifier: LicenseRef-Mesh-Sustainable-Use-License
// Copyright (C) 2026 Bright Interaction AB

// Package retrieve is the wedge: it fuses the FTS5 and graph-BM25 signals,
// expands one hop along the graph, boosts the institutional-memory tier, and
// packs the result to a token budget. The agent calls this, not raw search.
package retrieve

import (
	"context"
	"errors"
	"fmt"
	"math"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/bright-interaction/mesh/internal/embed"
	"github.com/bright-interaction/mesh/internal/graph"
	"github.com/bright-interaction/mesh/internal/index"
	"github.com/bright-interaction/mesh/internal/latency"
	"github.com/bright-interaction/mesh/internal/meshcfg"
	"github.com/bright-interaction/mesh/internal/rerank"
	"github.com/bright-interaction/mesh/internal/vault"
)

const (
	// tier0Mult nudges decision/gotcha/post-mortem notes UP among similarly-scored
	// results so institutional memory surfaces, but as a small multiplier (not the
	// old +0.5 additive, which could override a much stronger content match and
	// flip the top-1 pick to a wrong tier-0 note - the Gate-1 answer@1 regression).
	tier0Mult = 1.1
	// supersededMult demotes a note another note formally retired via `supersedes:`.
	//
	// DEMOTE, not hide. The superseded note is usually the only record of what was tried
	// and ruled out, and burying it invites the next session to re-derive the same wrong
	// answer - which is the exact failure this whole mechanism exists to stop. It also
	// has to survive being wrong: a mistaken supersedes must cost rank, not erase a note.
	//
	// 0.5 is chosen to lose to the tier-0 nudge decisively (0.5 vs 1.1 is a 2.2x gap, so a
	// superseded note cannot outrank its own replacement on a tie) while still beating a
	// genuinely weak match, so the correction wins the top slot and the history stays
	// reachable a few rows down.
	supersededMult = 0.5
	expandSeeds    = 5   // expand from the top-N fused notes
	expandK        = 3   // pull at most K strong note-neighbors per seed
	expandDecay    = 0.4 // a neighbor inherits this fraction of the seed's score
	// godDegree: skip expansion into hub notes above this KNOWLEDGE degree (distinct
	// other notes linked in or out). Measured on raw fan-out it also skipped any note
	// with enough headings, so a 25-section runbook was passed over for a two-line stub.
	godDegree = 24

	rerankK = 30 // rerank at most this many top fused candidates

	// rerankBlendDefault weights the cross-encoder vs the fused score when reranking
	// the head: score = a*rerank + (1-a)*fused. 1.0 = pure rerank (the default).
	// On a large production vault an alpha sweep showed pure rerank dominates every blend
	// (lowering it monotonically hurt paraphrase answer@1 and never recovered the
	// one keyword case), so 1.0 ships; the MESH_RERANK_BLEND knob stays for corpora
	// where the lexical/graph signal is strong enough to deserve a vote.
	rerankBlendDefault = 1.0
)

var tier0Types = map[string]bool{"decision": true, "gotcha": true, "post-mortem": true}

// Card is one retrieval result: a note, why it surfaced, and its fused score.
type Card struct {
	NodeID  string
	NoteID  string
	Title   string
	Path    string
	Type    string
	Scope   string // access-control scope(s), comma-joined (for the scope read filter)
	Snippet string
	Score   float64 // relevance; explicit title navigation can take precedence in result order
	Tier0   bool
	Reason  string
	// SupersededBy is the id of the note that formally retired this one, empty when
	// nothing has. Carried on the card (not just applied to the score) so the agent can
	// see WHICH note to read instead: a demoted note that still surfaces is useful, a
	// demoted note that surfaces unlabelled is a trap.
	// omitempty because this is empty on the overwhelming majority of cards and every
	// card is priced against the caller's token budget.
	SupersededBy string `json:",omitempty"`
	// MissingGuidance identifies unfilled required fields, not a factual quality
	// score. Tier0 remains a note-type classification, never a verification badge.
	MissingGuidance   []string               `json:",omitempty"`
	State             string                 `json:",omitempty"`
	Summary           string                 `json:",omitempty"`
	Template          string                 `json:",omitempty"`
	TemplateVersion   int                    `json:",omitempty"`
	Updated           string                 `json:",omitempty"`
	VerifiedAt        string                 `json:",omitempty"`
	Source            string                 `json:",omitempty"`
	SourceURL         string                 `json:",omitempty"`
	Sections          []index.SectionAddress `json:",omitempty"`
	SectionsTruncated bool                   `json:",omitempty"`
}

// GuidanceWarning is the human/prompt form of the compact structured card flag.
func (c Card) GuidanceWarning() string {
	if len(c.MissingGuidance) == 0 {
		return ""
	}
	return "Incomplete authored content: " + strings.Join(c.MissingGuidance, ", ") + "; review the source before relying on this note."
}

// Options tunes a retrieval. Zero values get sensible defaults.
type Options struct {
	// Limit is both the number of candidates pulled per signal AND the hard cap on
	// the cards returned (default 20). It has to be both: the vector arm and the
	// 1-hop expansion add candidates the per-signal fetch never saw, so a fetch-only
	// limit let a caller asking for 5 receive one card per vector-bearing note.
	Limit       int
	Budget      int     // token budget for packing; 0 = return all ranked (up to Limit)
	WeightFTS   float64 // fusion weight; 0 across all three => resolved defaults
	WeightGraph float64
	WeightVec   float64
	NoRerank    bool // skip any rerank stage even when configured (for evaluation/tuning)
	// Economics, when non-nil, receives content-free routing and token accounting
	// for this call. Callers own the pointer, so concurrent searches never share
	// mutable "last request" state.
	Economics *Economics
	// AllowedScopes, when non-nil, restricts results to notes whose scope intersects
	// the set (access control). nil = unrestricted (solo / no-ACL fast path). This is
	// THE read boundary and it is enforced throughout candidate generation: in the
	// FTS snapshot, against current persisted metadata before graph/vector/expansion
	// limits, again while enriching cards, and against metadata read atomically with
	// each document before a reranker or answering model sees its body.
	AllowedScopes map[string]bool
	// AllowPath, when non-nil, is the FOLDER read boundary: it reports whether the
	// caller may read the note at that vault-relative path. nil = unrestricted (no
	// folder ACL configured). It is a SEPARATE partition from AllowedScopes and a
	// caller must clear both: a team can fence folders with ACLs while never defining
	// a scope, in which case AllowedScopes is nil and filters nothing at all.
	AllowPath func(path string) bool
}

type Retriever struct {
	store  *index.Store
	graph  *graph.Graph
	ranker *graph.Ranker
	// False when optional persisted state could not be read. The current
	// lexical fallback remains usable, but ordinary refresh must retry later.
	refreshReusable bool

	emb         embed.Embedder
	vecModel    string
	vecDim      int                    // stored embedding width; pins the space (query embeddings of any other width are rejected, never silently cosined to a uniform 0)
	vecs        map[string][][]float32 // node id -> per-section chunk vectors
	ann         annSearcher            // optional ANN index over vecs; nil => brute-force cosine scan
	hnswGate    int                    // build hnsw only when chunk count >= this; 0 => never (brute force)
	queryPrefix string                 // e.g. "search_query: " for nomic-style asymmetric models

	rr          rerank.Reranker // optional endpoint or subscription reranker
	rerankName  string          // provider/model id, for status/diagnostics
	rerankSetup error           // configured but invalid; fail loudly instead of switching off
	rerankBlend float64         // cross-encoder vs fused weight (see rerankBlendDefault)

	// Learned/operator fusion-weight defaults (0 across all three => built-in
	// defaults). Set from MESH_WEIGHT_FTS/GRAPH/VEC or by `mesh tune`.
	defWFTS, defWGraph, defWVec float64

	qvec   map[string][]float32 // query-embedding cache (keyed by prefixed query)
	qvecMu sync.Mutex
	// Prevent every concurrent search from paying the provider timeout after one
	// request has already proved the optional semantic lane unavailable.
	semanticCircuitUntil time.Time

	freshHalfLife int                       // freshness decay half-life in days; 0 = off
	freshDates    map[string]index.NoteDate // note id -> lifecycle dates, lazy-loaded
	freshOnce     sync.Once
}

func New(store *index.Store, g *graph.Graph) *Retriever {
	r, _ := NewContext(context.Background(), store, g)
	return r
}

// NewContext is New with cooperative cancellation of the graph ranker build.
func NewContext(ctx context.Context, store *index.Store, g *graph.Graph) (*Retriever, error) {
	return newContext(ctx, store, g, nil)
}

func newContext(ctx context.Context, store *index.Store, g *graph.Graph, previous *graph.Ranker) (*Retriever, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	ranker, err := g.NewRankerReusingContext(ctx, previous)
	if err != nil {
		return nil, err
	}
	return &Retriever{store: store, graph: g, ranker: ranker, refreshReusable: true, rerankBlend: rerankBlendDefault, qvec: map[string][]float32{}}, nil
}

// RefreshReusable reports whether construction loaded its optional persisted
// inputs without a transient read failure. It does not probe any model.
func (r *Retriever) RefreshReusable() bool { return r.refreshReusable }

// SetWeights sets the fusion-weight defaults used when a retrieval does not pass
// explicit Options weights (e.g. learned weights from `mesh tune`). Any value
// may be 0; if all three are 0 the built-in defaults apply.
func (r *Retriever) SetWeights(fts, graph, vec float64) {
	r.defWFTS, r.defWGraph, r.defWVec = fts, graph, vec
}

// Weights reports the active fusion-weight defaults (0,0,0 => built-in defaults).
func (r *Retriever) Weights() (fts, graph, vec float64) {
	return r.defWFTS, r.defWGraph, r.defWVec
}

// NewFromEnv builds a retriever and enables the optional BYOAI stages from the
// environment plus their local persisted configuration. The semantic (vector)
// and rerank stages are independent: either, both, or neither can be on. Falls
// back silently to lexical-only when nothing is configured.
func NewFromEnv(store *index.Store, g *graph.Graph) *Retriever {
	r, _ := NewFromEnvContext(context.Background(), store, g)
	return r
}

// NewFromEnvContext is NewFromEnv with cancellation threaded through every
// potentially slow local construction stage: ranker statistics, config I/O,
// vector loading, and the optional pro HNSW build. Construction never calls a
// model: queries validate returned dimensions; explicit health probes test it.
func NewFromEnvContext(ctx context.Context, store *index.Store, g *graph.Graph) (*Retriever, error) {
	in, err := LoadConfigInputs(ctx, store.MeshDir())
	if err != nil {
		return nil, err
	}
	return NewFromInputsContext(ctx, store, g, in)
}

// NewFromInputsContext consumes exactly the configuration previously sampled by
// the reader's freshness gate. It does not reread config or the environment.
func NewFromInputsContext(ctx context.Context, store *index.Store, g *graph.Graph, in *ConfigInputs) (*Retriever, error) {
	return NewFromInputsReusingContext(ctx, store, g, in, nil)
}

// NewFromInputsReusingContext may reuse only the previous ranker's immutable
// term counts for exactly unchanged searchable inputs. It does not retain the
// previous retriever: configuration, vectors, freshness/query caches and all
// current read boundaries are constructed afresh, as with NewFromInputsContext.
func NewFromInputsReusingContext(ctx context.Context, store *index.Store, g *graph.Graph, in *ConfigInputs, previous *Retriever) (*Retriever, error) {
	trace := latency.Start("retriever_build", "ranker")
	defer trace.End()
	if ctx == nil {
		ctx = context.Background()
	}
	var priorRanker *graph.Ranker
	if previous != nil {
		priorRanker = previous.ranker
	}
	r, err := newContext(ctx, store, g, priorRanker)
	if err != nil {
		return nil, err
	}
	trace.Phase("config")
	cfg := in.cfg
	trace.Phase("stored_vectors")
	if err := r.enableVectorsFromInputs(ctx, cfg.Embedding, cfg.Retrieval, in); err != nil {
		return nil, err
	}
	if err := retrieveContextErr(ctx); err != nil {
		return nil, err
	}
	trace.Phase("rerank_weights")
	r.enableRerankFromInputs(cfg.Retrieval, in)
	r.loadWeights(cfg.Retrieval, in.getenv)
	if err := retrieveContextErr(ctx); err != nil {
		return nil, err
	}
	return r, nil
}

// loadWeights applies fusion weights, env-first then the solo config file (0 means
// "use the built-in default"). Env MESH_WEIGHT_* overrides the file, matching every
// other knob's precedence.
func (r *Retriever) loadWeights(rv meshcfg.Retrieval, getenv func(string) string) {
	pick := func(env string, file float64) float64 {
		if v, err := strconv.ParseFloat(getenv(env), 64); err == nil && v >= 0 {
			return v
		}
		if file >= 0 {
			return file
		}
		return 0
	}
	r.SetWeights(
		pick("MESH_WEIGHT_FTS", rv.WeightFTS),
		pick("MESH_WEIGHT_GRAPH", rv.WeightGraph),
		pick("MESH_WEIGHT_VEC", rv.WeightVec),
	)
	r.freshHalfLife = rv.FreshnessHalfLifeDays
	if v, err := strconv.Atoi(getenv("MESH_FRESHNESS_HALFLIFE_DAYS")); err == nil && v >= 0 {
		r.freshHalfLife = v
	}
}

// enableVectorsFromEnv turns on the semantic signal when the vault has stored
// vectors and the embedding endpoint + model are configured. Resolution is
// env-first, then the solo .mesh/config.toml (written by `mesh embed`), so a solo
// dev does not re-export env vars every session. Env always wins.
func (r *Retriever) enableVectors(emb meshcfg.Embedding, rv meshcfg.Retrieval) {
	_ = r.enableVectorsContext(context.Background(), emb, rv)
}

func (r *Retriever) enableVectorsContext(ctx context.Context, emb meshcfg.Embedding, rv meshcfg.Retrieval) error {
	return r.enableVectorsFromInputs(ctx, emb, rv, &ConfigInputs{env: snapshotEnvironment()})
}

func (r *Retriever) enableVectorsFromInputs(ctx context.Context, emb meshcfg.Embedding, rv meshcfg.Retrieval, in *ConfigInputs) error {
	if err := retrieveContextErr(ctx); err != nil {
		return err
	}
	endpoint, fromEnv := in.envOrFile("MESH_EMBED_ENDPOINT", emb.Endpoint)
	model := in.envOr("MESH_EMBED_MODEL", emb.Model)
	if endpoint == "" || model == "" {
		return nil
	}
	vm, dim, vecs, err := r.store.LoadVectorsContext(ctx)
	if err != nil || len(vecs) == 0 {
		if err != nil {
			r.refreshReusable = false
		}
		if ctxErr := retrieveContextErr(ctx); ctxErr != nil {
			return ctxErr
		}
		return nil
	}
	queryPrefix := in.envOr("MESH_EMBED_QUERY_PREFIX", emb.QueryPrefix)
	// key_env is a POINTER to a process secret, so it is resolved through the closed
	// allow-list rather than read verbatim: a hand-edited config.toml that never passed
	// through the web config API must not be able to aim this at MESH_UI_TOKEN and have
	// the embedding endpoint receive it.
	keyEnv := meshcfg.ResolveKeyEnv("embedding.key_env", emb.KeyEnv, "MESH_EMBED_KEY")
	// Optional ANN: build an HNSW index past the threshold (0/unset = brute force,
	// the default; sub-5ms well past v1 scale). Env wins, then the config file.
	hnswGate := 0
	if v, err := strconv.Atoi(in.getenv("MESH_HNSW_THRESHOLD")); err == nil && v > 0 {
		hnswGate = v
	} else if rv.HNSWThreshold > 0 {
		hnswGate = rv.HNSWThreshold
	}
	// The endpoint's SOURCE picks the HTTP client. From the process environment it is
	// operator input and may point at a local model server; from config.toml it is
	// member-writable (the web config API rewrites that file) and stays SSRF-guarded.
	newEmbedder := embed.NewHTTP
	if fromEnv {
		newEmbedder = embed.NewOperatorHTTP
	}
	ok, err := r.configureVectorsContext(ctx, newEmbedder(endpoint, model, in.getenv(keyEnv)), vm, dim, vecs, hnswGate, false)
	if err != nil {
		return err
	}
	if ok {
		r.queryPrefix = queryPrefix
		r.hnswGate = hnswGate
	}
	return nil
}

// enableRerank turns on either a user-local subscription CLI (environment first,
// then the per-user/per-vault config outside the project) or the legacy cross-
// encoder endpoint (env-first, then solo config). No shared vault or project file
// can force subscription egress. The model sees compact cards, never note bodies.
func (r *Retriever) enableRerank(rv meshcfg.Retrieval) {
	in := &ConfigInputs{env: snapshotEnvironment()}
	in.loadSubscription(context.Background(), filepath.Dir(r.store.MeshDir()))
	r.enableRerankFromInputs(rv, in)
}

func (r *Retriever) enableRerankFromInputs(rv meshcfg.Retrieval, in *ConfigInputs) {
	if b := in.getenv("MESH_RERANK_BLEND"); b != "" {
		if v, err := strconv.ParseFloat(b, 64); err == nil && v >= 0 && v <= 1 {
			r.rerankBlend = v
		}
	} else if rv.RerankBlend > 0 {
		r.rerankBlend = rv.RerankBlend
	}

	agent := strings.ToLower(strings.TrimSpace(in.getenv("MESH_RERANK_AGENT")))
	model := strings.TrimSpace(in.getenv("MESH_RERANK_MODEL"))
	policy := strings.TrimSpace(in.getenv("MESH_RERANK_POLICY"))
	if agent == "" {
		sub, enabled, err := in.sub, in.subEnabled, in.subErr
		if err != nil {
			r.rerankName = "subscription/local-config"
			r.rerankSetup = err
			return
		}
		if enabled {
			agent = sub.Agent
			if model == "" {
				model = sub.Model
			}
			if policy == "" {
				policy = sub.Policy
			}
		}
	}
	if agent != "" && agent != "http" {
		rr, err := rerank.NewConfiguredSubscriptionCLIWithEnv(agent, model, policy, in.getenv)
		if err != nil {
			r.rerankName = "subscription/" + agent
			r.rerankSetup = err
			return
		}
		r.EnableRerank(rr)
		return
	}

	endpoint, fromEnv := in.envOrFile("MESH_RERANK_ENDPOINT", rv.RerankEndpoint)
	endpointModel := in.envOr("MESH_RERANK_MODEL", rv.RerankModel)
	if endpoint == "" || endpointModel == "" {
		return
	}
	// Same allow-list as the embedding key: see enableVectors.
	keyEnv := meshcfg.ResolveKeyEnv("rerank.key_env", rv.RerankKeyEnv, "MESH_RERANK_KEY")
	// Same provenance split as enableVectors: MESH_RERANK_ENDPOINT is operator input and
	// may be the local tools/rerank-server on 127.0.0.1; rerank.endpoint in config.toml
	// is member-writable and stays guarded.
	newReranker := rerank.NewHTTP
	if fromEnv {
		newReranker = rerank.NewOperatorHTTP
	}
	r.EnableRerank(newReranker(endpoint, endpointModel, in.getenv(keyEnv)))
}

// EnableRerank turns on the cross-encoder rerank stage. The reranker reorders the top-K
// fused candidates. Once enabled it is part of the contract: an endpoint that cannot be
// reached fails the query with ErrRerankUnavailable rather than quietly returning the
// fused order as if it had been reranked. Returns false for a nil reranker.
func (r *Retriever) EnableRerank(rr rerank.Reranker) bool {
	if rr == nil {
		return false
	}
	r.rr, r.rerankName = rr, rr.Model()
	return true
}

// ErrRerankUnavailable wraps every failure of a CONFIGURED reranker, so a caller that
// genuinely wants to keep serving (a long-lived server, say) can errors.Is it and choose
// to, while the default for a one-shot CLI or MCP call is to surface it. It never fires
// when no reranker is configured.
var ErrRerankUnavailable = errors.New("reranker unavailable")

// ErrEmbeddingUnavailable wraps failures from a configured semantic lane. Ordinary
// retrieval catches it and returns an explicitly labelled lexical + graph fallback;
// caller cancellation and a per-call vector weight stay fail-loud.
var ErrEmbeddingUnavailable = errors.New("embedding endpoint unavailable")

var errEmbeddingCircuitOpen = errors.New("embedding circuit open")

const semanticCircuitCooldown = time.Minute

// ErrInvalidWeights means a retrieval could not produce a meaningful, deterministic
// ranking because at least one configured fusion weight was negative or non-finite.
// Weight inputs are operator/request data; rejecting them is safer than letting NaN
// enter sort comparators (which do not define a strict order).
var ErrInvalidWeights = errors.New("invalid retrieval weights")

// rerankProbeTimeout bounds the liveness probe below. A cross-encoder scoring one short
// sentinel document answers in well under a second; anything slower is not usable on a
// query path either.
const rerankProbeTimeout = 5 * time.Second

// RerankProbe sends ONE sentinel scoring request to the configured reranker and reports
// whether it answered. `mesh status` prints "rerank active" off configuration alone,
// which told a user with a dead endpoint that everything was fine; a probe is the only
// honest answer to "is it on". Returns nil when nothing is configured (there is nothing
// to be wrong) and wraps failures in ErrRerankUnavailable.
func (r *Retriever) RerankProbe(ctx context.Context) error {
	if r.rerankSetup != nil {
		return fmt.Errorf("%w: %w", ErrRerankUnavailable, r.rerankSetup)
	}
	if r.rr == nil {
		return nil
	}
	if p, ok := r.rr.(rerank.Prober); ok {
		if err := p.Probe(ctx); err != nil {
			return fmt.Errorf("%w: %w", ErrRerankUnavailable, err)
		}
		return nil
	}
	ctx, cancel := context.WithTimeout(ctx, rerankProbeTimeout)
	defer cancel()
	if _, err := r.rr.Rerank(ctx, "mesh rerank probe", []string{"mesh rerank probe"}); err != nil {
		return fmt.Errorf("%w: %w", ErrRerankUnavailable, err)
	}
	return nil
}

// SignalReport is the honest answer to "which retrieval signals will actually fire":
// each optional stage as CONFIGURED plus, separately, what its readiness check proved.
// HTTP stages make an inference probe. Subscription CLIs only verify that the binary
// exists so status costs no quota, and RerankCheck makes that limitation explicit.
type SignalReport struct {
	VectorsConfigured bool
	VectorsReachable  bool
	VectorsError      string // empty when reachable or not configured
	VectorModel       string
	ANN               bool

	RerankConfigured bool
	RerankReachable  bool   // endpoint answered, or subscription CLI executable exists
	RerankError      string // empty when ready or not configured
	RerankModel      string
	RerankCheck      string // what the probe verified; empty for an inference endpoint probe
}

// Signals probes every configured BYOAI stage once and returns what is really on. It is
// the single renderer behind `mesh status` and the MCP mesh://retrieval resource, which
// used to answer the same question from their own copies of the logic and could not
// disagree only by accident. A stage that is not configured is reported unreachable with
// no error: there is nothing to be wrong with it.
func (r *Retriever) Signals(ctx context.Context) SignalReport {
	rep := SignalReport{
		VectorsConfigured: r.VectorsActive(),
		VectorModel:       r.VectorModel(),
		ANN:               r.HNSWActive(),
		RerankConfigured:  r.RerankActive(),
		RerankModel:       r.RerankModel(),
	}
	if rep.VectorsConfigured {
		if err := r.EmbedderProbe(); err != nil {
			rep.VectorsError = err.Error()
		} else {
			rep.VectorsReachable = true
		}
	}
	if rep.RerankConfigured {
		if err := r.RerankProbe(ctx); err != nil {
			rep.RerankError = err.Error()
		} else {
			rep.RerankReachable = true
			if reporter, ok := r.rr.(rerank.ProbeReporter); ok {
				rep.RerankCheck = reporter.ProbeReport()
			}
		}
	}
	return rep
}

// EmbedderProbe reports whether the configured query embedder answers. Like RerankProbe
// this exists because `mesh status` was reporting configuration, not reachability. Dim()
// performs the round trip (and caches the result), so a width of 0 means the endpoint did
// not answer. Returns nil when no embedder is configured.
func (r *Retriever) EmbedderProbe() error {
	if r.emb == nil {
		return nil
	}
	dim := r.emb.Dim()
	if dim == 0 {
		return fmt.Errorf("embedding endpoint unavailable: model %s returned no vector width", r.emb.Model())
	}
	if dim != r.vecDim {
		return fmt.Errorf("embedding dimension mismatch: model %s reports %d, stored width is %d", r.emb.Model(), dim, r.vecDim)
	}
	return nil
}

// RerankActive reports whether any rerank stage is configured, including one
// whose setup failed and must remain visible to status and retrieval callers.
func (r *Retriever) RerankActive() bool { return r.rr != nil || r.rerankSetup != nil }

// RerankModel returns the configured rerank model id (empty when inactive).
func (r *Retriever) RerankModel() string { return r.rerankName }

// VectorsActive reports whether the semantic signal will fire (an embedder is
// configured and the vault has stored vectors that match its model).
func (r *Retriever) VectorsActive() bool { return r.emb != nil && len(r.vecs) > 0 }

// VectorModel returns the active embedding model id (empty when inactive).
func (r *Retriever) VectorModel() string { return r.vecModel }

// HNSWActive reports whether the ANN index is built and serving the vector signal
// (vs the brute-force scan). Only true past the configured MESH_HNSW_THRESHOLD,
// and only in the pro build (the open core has no ANN implementation wired).
func (r *Retriever) HNSWActive() bool { return r.ann != nil }

// annResult is one ANN hit. It mirrors the (pro-only) hnsw.Result shape but lives
// here so the open core compiles without importing the hnsw package.
type annResult struct {
	NodeID  string
	ChunkIx int
	Score   float64
}

// annSearcher is the optional approximate-nearest-neighbour seam. The open core
// ships no implementation (brute-force cosine only); the pro build wires HNSW by
// setting buildANN (see retrieve_ann_pro.go, //go:build pro).
type annSearcher interface {
	Search(q []float32, k, ef int) []annResult
}

// buildANN constructs the ANN index from the per-node vectors. nil in the open
// core (brute-force always); set by the pro build. On any error the caller keeps
// the brute-force scan, so the ANN path can only speed up, never break, retrieval.
var buildANN func(context.Context, map[string][][]float32) (annSearcher, error)

// resolveWeights picks the fusion weights: explicit Options weights win; else the
// learned/operator defaults (SetWeights / env); else the built-in defaults. The
// vector weight is zeroed when no semantic signal is active.
func (r *Retriever) resolveWeights(opt Options, vectorsActive bool) (wFTS, wGraph, wVec float64) {
	switch {
	case opt.WeightFTS != 0 || opt.WeightGraph != 0 || opt.WeightVec != 0:
		wFTS, wGraph, wVec = opt.WeightFTS, opt.WeightGraph, opt.WeightVec
	case r.defWFTS != 0 || r.defWGraph != 0 || r.defWVec != 0:
		wFTS, wGraph, wVec = r.defWFTS, r.defWGraph, r.defWVec
	case vectorsActive:
		// FTS-top1 beat fused-top1 lexically, so FTS stays the largest share, the
		// semantic signal gets real weight, graph-BM25 the smallest.
		wFTS, wGraph, wVec = 0.5, 0.2, 0.3
	default:
		wFTS, wGraph = 0.7, 0.3
	}
	if !vectorsActive {
		wVec = 0
	}
	return
}

// queryVec returns the (cached) embedding of the query, prefixed for asymmetric
// models. The cache makes repeated retrievals of the same query (e.g. a weight sweep)
// embed only once. Retrieve decides whether a returned provider error is strict or an
// explicitly reported local fallback.
func (r *Retriever) queryVec(ctx context.Context, query string) ([]float32, error) {
	if r.emb == nil {
		return nil, nil
	}
	key := r.queryPrefix + query
	r.qvecMu.Lock()
	defer r.qvecMu.Unlock()
	if v, ok := r.qvec[key]; ok {
		return v, nil
	}
	if time.Now().Before(r.semanticCircuitUntil) {
		return nil, fmt.Errorf("%w (embedder %s): %w", ErrEmbeddingUnavailable, r.emb.Model(), errEmbeddingCircuitOpen)
	}
	qv, err := r.emb.Embed(ctx, []string{key})
	if err != nil {
		if retrieveContextErr(ctx) == nil {
			r.semanticCircuitUntil = time.Now().Add(semanticCircuitCooldown)
		}
		return nil, fmt.Errorf("%w (embedder %s): %w", ErrEmbeddingUnavailable, r.emb.Model(), err)
	}
	if len(qv) != 1 {
		r.semanticCircuitUntil = time.Now().Add(semanticCircuitCooldown)
		return nil, fmt.Errorf("%w (embedder %s): returned %d vectors for one query", ErrEmbeddingUnavailable, r.emb.Model(), len(qv))
	}
	if r.vecDim <= 0 || len(qv[0]) != r.vecDim {
		r.semanticCircuitUntil = time.Now().Add(semanticCircuitCooldown)
		return nil, fmt.Errorf("%w (embedder %s): query width %d does not match stored width %d", ErrEmbeddingUnavailable, r.emb.Model(), len(qv[0]), r.vecDim)
	}
	r.semanticCircuitUntil = time.Time{}
	// Bound the cache: a long-lived shared Retriever (the SSH viewer builds one and
	// never swaps it) under a high-cardinality query stream would otherwise grow this
	// map forever. A query-embedding cache tolerates a coarse reset on overflow.
	if len(r.qvec) >= maxQvecEntries {
		r.qvec = make(map[string][]float32, maxQvecEntries)
	}
	r.qvec[key] = qv[0]
	return qv[0], nil
}

// maxQvecEntries caps the per-Retriever query-embedding cache.
const maxQvecEntries = 4096

// EnableVectors turns on the semantic signal. It is a no-op unless the query
// embedder's model matches the vault's stored model AND its vector width matches
// the stored width (homogeneity guard: vectors from a different model, or even the
// same model name at a different dimension, are not comparable. A length mismatch
// makes every cosine return 0, which min-max then normalizes to a uniform 1 - a
// silent garbage signal that boosts every note equally. Activation refuses a known
// mismatch; a mismatch discovered on the query path is returned as an error rather
// than emitting it). storedDim is the vault's recorded width; if it
// is 0 (old vault, pre-vector_dim) we derive it from the loaded vectors.
func (r *Retriever) EnableVectors(e embed.Embedder, model string, storedDim int, vecs map[string][][]float32) bool {
	ok, _ := r.EnableVectorsContext(context.Background(), e, model, storedDim, vecs)
	return ok
}

// EnableVectorsContext is EnableVectors with cancellation of dimension probing
// and optional ANN construction. Receiver fields are published only after the
// complete activation succeeds.
func (r *Retriever) EnableVectorsContext(ctx context.Context, e embed.Embedder, model string, storedDim int, vecs map[string][][]float32) (bool, error) {
	return r.enableVectorsWithGateContext(ctx, e, model, storedDim, vecs, r.hnswGate)
}

type contextDimmer interface {
	DimContext(context.Context) (int, error)
}

func (r *Retriever) enableVectorsWithGateContext(ctx context.Context, e embed.Embedder, model string, storedDim int, vecs map[string][][]float32, hnswGate int) (bool, error) {
	return r.configureVectorsContext(ctx, e, model, storedDim, vecs, hnswGate, true)
}

func (r *Retriever) configureVectorsContext(ctx context.Context, e embed.Embedder, model string, storedDim int, vecs map[string][][]float32, hnswGate int, probe bool) (bool, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if err := retrieveContextErr(ctx); err != nil {
		return false, err
	}
	if e == nil || model == "" || len(vecs) == 0 || e.Model() != model {
		return false, nil
	}
	dim := storedDim
	if dim == 0 {
		for _, chunks := range vecs {
			if len(chunks) > 0 && len(chunks[0]) > 0 {
				dim = len(chunks[0])
				break
			}
		}
	}
	// We must know the stored width to guard the query side; if we cannot determine
	// it (no stamped dim AND only zero-length vectors), refuse rather than activate
	// with vecDim==0, which would disable the per-query length guard and let a
	// uniform-garbage signal through.
	if dim == 0 {
		return false, nil
	}
	// If the embedder reports a width and it disagrees with the stored width, refuse.
	// A 0 from Dim() means the probe failed (endpoint down); allow activation and let
	// the per-query length guard in Retrieve catch any mismatch at retrieval time.
	ed := 0
	if d, ok := e.(contextDimmer); probe && ok {
		var err error
		ed, err = d.DimContext(ctx)
		if err != nil {
			if ctxErr := retrieveContextErr(ctx); ctxErr != nil {
				return false, ctxErr
			}
			ed = 0 // legacy behavior: a failed probe activates and fails honestly on query
		}
	} else if probe {
		ed = e.Dim()
	}
	if err := retrieveContextErr(ctx); err != nil {
		return false, err
	}
	if ed != 0 && ed != dim {
		return false, nil
	}
	var ann annSearcher
	// Optional ANN index for large vaults (pro build only). Off unless hnswGate is
	// set AND a buildANN implementation is wired; on any build error the brute-force
	// scan stays (r.ann nil), so this can only speed up, never break, retrieval.
	// Built from the same vecs map, so the vectors are identical. In the open core
	// buildANN is nil, so retrieval is always brute-force cosine.
	if hnswGate > 0 && buildANN != nil {
		total := 0
		for _, chunks := range vecs {
			if err := retrieveContextErr(ctx); err != nil {
				return false, err
			}
			total += len(chunks)
		}
		if total >= hnswGate {
			ix, err := buildANN(ctx, vecs)
			if err == nil {
				ann = ix
			} else if ctxErr := retrieveContextErr(ctx); ctxErr != nil {
				return false, ctxErr
			}
		}
	}
	if err := retrieveContextErr(ctx); err != nil {
		return false, err
	}
	r.emb, r.vecModel, r.vecDim, r.vecs, r.ann = e, model, dim, vecs, ann
	return true, nil
}

func retrieveContextErr(ctx context.Context) error {
	if ctx == nil {
		return nil
	}
	select {
	case <-ctx.Done():
		if err := context.Cause(ctx); err != nil {
			return err
		}
		return ctx.Err()
	default:
		return nil
	}
}

// Retrieve runs the full pipeline and returns ranked (and optionally
// budget-packed) cards.
func (r *Retriever) Retrieve(ctx context.Context, query string, opt Options) ([]Card, error) {
	started := time.Now()
	if opt.Economics != nil {
		*opt.Economics = Economics{
			SemanticConfigured: r.VectorsActive(),
			RerankConfigured:   r.RerankActive(),
			RerankModel:        r.RerankModel(),
			Route:              "disabled",
		}
		defer func() { opt.Economics.SearchLatencyMS = durationMillis(time.Since(started)) }()
	}
	if opt.Limit <= 0 {
		opt.Limit = 20
	}
	if err := validateWeights(opt.WeightFTS, opt.WeightGraph, opt.WeightVec); err != nil {
		return nil, err
	}
	if opt.WeightFTS == 0 && opt.WeightGraph == 0 && opt.WeightVec == 0 {
		if err := validateWeights(r.defWFTS, r.defWGraph, r.defWVec); err != nil {
			return nil, err
		}
	}
	vectorsActive := r.emb != nil && len(r.vecs) > 0
	wFTS, wGraph, wVec := r.resolveWeights(opt, vectorsActive)
	if err := validateWeights(wFTS, wGraph, wVec); err != nil {
		return nil, err
	}

	// Embeddings are an optional recall lane, not a dependency for ordinary Mesh
	// retrieval. Probe the query side before scoring any lane so an unavailable,
	// blocked, or width-incompatible provider can cleanly switch the whole request to
	// lexical + graph weights. Cancellation remains an error, and an explicit vector
	// weight remains strict: evals and operator-requested vector searches must never
	// masquerade as semantic when they did not run.
	var qv []float32
	var readableVecs map[string]Card
	if vectorsActive && wVec > 0 {
		vecIDs := make([]string, 0, len(r.vecs))
		for id := range r.vecs {
			vecIDs = append(vecIDs, id)
		}
		sort.Strings(vecIDs)
		var err error
		readableVecs, err = r.currentCards(ctx, vecIDs, opt)
		if err != nil {
			return nil, fmt.Errorf("read current vector-candidate metadata: %w", err)
		}
		if len(readableVecs) == 0 {
			vectorsActive = false
		} else {
			qv, err = r.queryVec(ctx, query)
			if err != nil {
				if ctxErr := retrieveContextErr(ctx); ctxErr != nil {
					return nil, ctxErr
				}
				if opt.WeightVec > 0 {
					return nil, err
				}
				vectorsActive = false
				if opt.Economics != nil {
					opt.Economics.SemanticFallback = true
					opt.Economics.SemanticCircuitOpen = errors.Is(err, errEmbeddingCircuitOpen)
				}
			}
		}
		if !vectorsActive {
			wFTS, wGraph, wVec = r.resolveWeights(opt, false)
			// A vector-only persisted default must not turn an optional-provider outage
			// into an empty success. The caller did not explicitly demand vectors, so use
			// Mesh's built-in local blend.
			if wFTS == 0 && wGraph == 0 {
				wFTS, wGraph = 0.7, 0.3
			}
		}
	}

	// Candidate generation is scope-aware: both keyword signals apply the read
	// boundary BEFORE their own truncation, so the fetch limit counts only rows this
	// caller may read. The old shape (fetch globally, over-fetch 4x, filter at the
	// card loop) starved a scoped caller to zero results as soon as ~4*Limit
	// higher-ranked unreadable notes matched the query.
	fetchLimit := opt.Limit
	// ctx, not context.Background(): this call used to drop the caller's context, so
	// cancelling the agent tool call or the HTTP request left the FTS read running to
	// completion. With the deadline the store now applies, a pathological query ends in
	// a named error instead of a process pinned at 100% CPU with nothing to cancel.
	fused := map[string]float64{}
	snippet := map[string]string{}
	reason := map[string]string{}

	// FTS signal, min-max normalized. A zero-weight arm is absent, not a source of
	// zero-scored candidates: adding its hits to fused made explicitly vector-only
	// searches return lexical tail cards that the caller had disabled.
	if wFTS > 0 {
		ftsHits, err := r.store.SearchScopedPaths(ctx, query, fetchLimit, opt.AllowedScopes, opt.AllowPath)
		if err != nil {
			return nil, err
		}
		fScores := make([]float64, len(ftsHits))
		for i, h := range ftsHits {
			fScores[i] = h.Score
		}
		fNorm := minMaxFloored(fScores)
		for i, h := range ftsHits {
			fused[h.NodeID] += wFTS * fNorm[i]
			snippet[h.NodeID] = h.Snippet
			reason[h.NodeID] = "fts"
		}
	}

	// graph-BM25 signal, min-max normalized. As above, zero means disabled rather
	// than "include every match with a score of zero".
	if wGraph > 0 {
		// The graph can lag the notes table by one watcher generation. Score the
		// full ranked stream and apply CURRENT persisted metadata before taking the
		// limit; deleted/stale/forbidden nodes are neither results nor allowed to
		// consume the caller's candidate budget.
		all := r.ranker.Score(query, 0)
		ids := make([]string, len(all))
		for i := range all {
			ids[i] = all[i].Node.ID
		}
		readable, err := r.currentCards(ctx, ids, opt)
		if err != nil {
			return nil, fmt.Errorf("read current graph-candidate metadata: %w", err)
		}
		graphHits := make([]graph.ScoredNode, 0, fetchLimit)
		// Keep explicit full-title navigation BEFORE the cap, just like FTS.
		// Two stable passes preserve BM25 order within each group and consult
		// current authorized metadata rather than the stale graph label.
		for _, exact := range []bool{true, false} {
			for _, h := range all {
				c, ok := readable[h.Node.ID]
				if !ok || graph.MatchesFullTitle(query, c.Title) != exact {
					continue
				}
				graphHits = append(graphHits, h)
				if len(graphHits) >= fetchLimit {
					break
				}
			}
			if len(graphHits) >= fetchLimit {
				break
			}
		}
		gScores := make([]float64, len(graphHits))
		for i, h := range graphHits {
			gScores[i] = h.Score
		}
		gNorm := minMaxFloored(gScores)
		for i, h := range graphHits {
			fused[h.Node.ID] += wGraph * gNorm[i]
			if reason[h.Node.ID] == "" {
				reason[h.Node.ID] = "graph"
			}
		}
	}

	// Semantic signal: cosine of the query embedding against stored note vectors
	// (brute-force; the homogeneity guard already ensured comparable models). A
	// note is scored by its best-matching section (max over its chunk vectors),
	// so a long multi-topic note still surfaces on the one section that answers
	// the query instead of being diluted by a whole-note average.
	if vectorsActive && wVec > 0 {
		// Length guard: a query embedding whose width disagrees with the stored width
		// would make every cosine 0, which min-max turns into a uniform 1 boosting every
		// note equally. Skip the whole vector contribution rather than emit that garbage.
		// vecDim is always > 0 once EnableVectors succeeds, so a mismatch is a real skip.
		if len(readableVecs) > 0 {
			// Both arms produce the same shape: the top-K chunk vectors folded to a
			// per-note max. Keeping the ANN and brute-force candidate sets identical is
			// what makes the two paths rank alike (see vectorCandidates).
			ids, sims := r.vectorCandidates(qv, vecCandidateK(opt.Limit), readableVecs)
			for i, id := range ids {
				// Path-independent scaling: map the cosine onto [0,1] against its own
				// fixed range instead of min-maxing the per-request candidate set. The
				// old min-max rescaled every score to whatever happened to be fetched,
				// so switching on the ANN index (a much smaller candidate set) silently
				// reordered results instead of only making them faster.
				fused[id] += wVec * cosineTo01(sims[i])
				if reason[id] == "" {
					reason[id] = "vector"
				}
			}
		}
	}

	// Capped 1-hop expansion from the strongest seeds the caller is allowed to read.
	// The seed filter is load-bearing, not defence in depth: a seed the caller cannot
	// read used to stamp its own frontmatter title into the neighbour's Reason
	// ("linked from <secret title>") and donate seed.score to the neighbour's rank,
	// so an unreadable note leaked its title and steered the scoped ranking. Scanning
	// past forbidden seeds (rather than dropping them from the top-5 slate) keeps
	// expansion recall intact for scoped callers.
	seedOrder := topN(fused, 0)
	seedIDs := make([]string, len(seedOrder))
	for i := range seedOrder {
		seedIDs[i] = seedOrder[i].id
	}
	seedCards, err := r.currentCards(ctx, seedIDs, opt)
	if err != nil {
		return nil, fmt.Errorf("read current expansion-seed metadata: %w", err)
	}
	seeded := 0
	for _, seed := range seedOrder {
		if seeded >= expandSeeds {
			break
		}
		seedCard, seedOK := seedCards[seed.id]
		if !seedOK {
			continue
		}
		seeded++
		neighbors := r.strongNeighbors(seed.id, 0)
		neighborIDs := make([]string, 0, len(neighbors))
		for _, nb := range neighbors {
			if _, seen := fused[nb.id]; !seen {
				neighborIDs = append(neighborIDs, nb.id)
			}
		}
		readableNeighbors, err := r.currentCards(ctx, neighborIDs, opt)
		if err != nil {
			return nil, fmt.Errorf("read current expansion-neighbor metadata: %w", err)
		}
		expanded := 0
		for _, nb := range neighbors {
			if _, seen := fused[nb.id]; seen {
				continue
			}
			if _, ok := readableNeighbors[nb.id]; !ok {
				continue
			}
			fused[nb.id] = seed.score * expandDecay * nb.weight
			reason[nb.id] = "linked from " + seedCard.Title
			expanded++
			if expanded >= expandK {
				break
			}
		}
	}

	// Enrich from CURRENT persisted metadata, not the independently refreshed graph.
	// That prevents a fresh FTS snippet/body from being paired with a stale public path
	// or scope, and also makes a newly indexed note actionable before the graph swap.
	allIDs := make([]string, 0, len(fused))
	for id := range fused {
		allIDs = append(allIDs, id)
	}
	sort.Strings(allIDs)
	current, err := r.currentCards(ctx, allIDs, opt)
	if err != nil {
		return nil, fmt.Errorf("read current result metadata: %w", err)
	}

	// Apply the tier-0 boost.
	cards := make([]Card, 0, len(fused))
	for id, score := range fused {
		c, ok := current[id]
		if !ok {
			continue
		}
		c.Snippet = snippet[id]
		c.Reason = reason[id]
		c.Score = score * r.boostMult(c)
		cards = append(cards, c)
	}
	sortCards(cards)
	if wFTS > 0 || wGraph > 0 {
		prioritizeTitleLookup(query, cards, seedIDs)
	}
	if opt.Economics != nil {
		localView := cards
		if opt.Limit > 0 && len(localView) > opt.Limit {
			localView = localView[:opt.Limit]
		}
		if opt.Budget > 0 {
			localView = packToBudget(localView, opt.Budget)
		}
		opt.Economics.LocalCards = len(localView)
		opt.Economics.LocalCardTokens = TotalTokens(localView)
	}

	// Optional rerank of the head. HTTP cross-encoders score bounded note text;
	// subscription CLIs rank an even smaller card slate. A CONFIGURED reranker that
	// is unavailable fails the query rather than dressing fused output up as reranked.
	// Skipped when tuning the fusion itself (NoRerank).
	if !opt.NoRerank {
		cards, err = r.rerankHead(ctx, query, cards, fused, opt)
		if err != nil {
			return nil, err
		}
		// Subscription ranking is explicitly a low-context mode: after a cheap model
		// has selected the useful head, do not make the expensive calling agent read
		// the discarded cards too. The operator can tune this bounded cap with
		// MESH_RERANK_RESULTS; HTTP cross-encoder behavior is unchanged.
		if compact, ok := r.rr.(rerank.CandidateReranker); ok {
			if limit := compact.ResultLimit(); limit > 0 && len(cards) > limit {
				cards = cards[:limit]
			}
		}
	} else if opt.Economics != nil {
		opt.Economics.Route = "disabled"
	}

	// Limit bounds the RETURNED set, after the reranker has had its say (so it can
	// still pull a card up from the tail) and before packing. Without this the vector
	// arm and the 1-hop expansion pushed cards the per-signal fetch never counted
	// straight to the caller: a Limit of 5 over a vector-enabled vault returned one
	// card per note in the vault.
	if opt.Limit > 0 && len(cards) > opt.Limit {
		cards = cards[:opt.Limit]
	}

	if opt.Budget > 0 {
		cards = packToBudget(cards, opt.Budget)
	}
	if opt.Economics != nil {
		opt.Economics.ReturnedCards = len(cards)
		opt.Economics.ReturnedTokens = TotalTokens(cards)
	}
	return cards, nil
}

// vecCandidateFloor is the smallest chunk-candidate pool the vector arm considers,
// so a tiny Limit still leaves the fused head a stable semantic signal.
const vecCandidateFloor = 50

// vecCandidateK is the number of chunk vectors the semantic signal considers for a
// given result limit. It is generous, so the fused and reranked head is stable even
// though the deep tail is cut off (and, on the pro build, approximate).
func vecCandidateK(limit int) int {
	k := limit * 4
	if k < vecCandidateFloor {
		k = vecCandidateFloor
	}
	return k
}

// cosineTo01 maps a cosine similarity onto [0,1] against its own fixed range.
//
// This must NOT be a min-max over the request's candidates. The candidate set
// differs per path (every chunk in the vault on the brute-force arm, the ANN
// index's top-k on the pro arm), so a relative normalizer made a note's semantic
// contribution depend on what else happened to be fetched: enabling the HNSW index
// rescaled every surviving score and reordered the results rather than only
// speeding them up. A fixed reference keeps a given cosine worth the same thing on
// both arms. The clamp only absorbs float drift outside [-1,1].
func cosineTo01(c float64) float64 {
	if math.IsNaN(c) {
		return 0
	}
	v := (c + 1) / 2
	if v < 0 {
		return 0
	}
	if v > 1 {
		return 1
	}
	return v
}

func validateWeights(weights ...float64) error {
	for _, w := range weights {
		if math.IsNaN(w) || math.IsInf(w, 0) || w < 0 {
			return fmt.Errorf("%w: weights must be finite and non-negative", ErrInvalidWeights)
		}
	}
	return nil
}

// vectorCandidates returns the top-k chunk vectors for the query, folded to one
// entry per note carrying that note's best chunk score, in descending score order.
// Both arms go through here so the ANN and brute-force candidate sets have the same
// shape and size: the ANN index is then an accelerator for the same computation,
// not a different ranking.
func (r *Retriever) vectorCandidates(qv []float32, k int, allowed map[string]Card) (ids []string, sims []float64) {
	if k <= 0 {
		return nil, nil
	}
	var hits []annResult
	if r.ann != nil {
		// ANN cannot apply an arbitrary ACL predicate internally. Grow the ranked
		// chunk pool until k current/readable chunks survive or the index is exhausted.
		total := 0
		for _, chunks := range r.vecs {
			total += len(chunks)
		}
		probe := k
		if probe > total {
			probe = total
		}
		for probe > 0 {
			raw := r.ann.Search(qv, probe, 0)
			hits = hits[:0]
			for _, h := range raw {
				if _, ok := allowed[h.NodeID]; ok {
					hits = append(hits, h)
				}
			}
			if len(hits) >= k || probe >= total || len(raw) < probe {
				break
			}
			probe *= 2
			if probe > total {
				probe = total
			}
		}
		if len(hits) > k {
			hits = hits[:k]
		}
	} else {
		hits = r.bruteForceTopChunks(qv, k, allowed)
	}
	best := map[string]float64{}
	for _, h := range hits {
		cur, seen := best[h.NodeID]
		if !seen {
			best[h.NodeID] = h.Score
			ids = append(ids, h.NodeID)
			continue
		}
		if h.Score > cur {
			best[h.NodeID] = h.Score
		}
	}
	sims = make([]float64, len(ids))
	for i, id := range ids {
		sims[i] = best[id]
	}
	return ids, sims
}

// bruteForceTopChunks scans every stored chunk vector and returns the k closest,
// mirroring what the ANN index returns (chunks, not notes: the caller max-pools).
// Ties break on node id then chunk index so the order is deterministic.
func (r *Retriever) bruteForceTopChunks(qv []float32, k int, allowed map[string]Card) []annResult {
	all := make([]annResult, 0, len(r.vecs))
	for id, chunks := range r.vecs {
		if _, ok := allowed[id]; !ok {
			continue
		}
		for ci, v := range chunks {
			all = append(all, annResult{NodeID: id, ChunkIx: ci, Score: embed.Cosine(qv, v)})
		}
	}
	sort.Slice(all, func(i, j int) bool {
		if all[i].Score != all[j].Score {
			return all[i].Score > all[j].Score
		}
		if all[i].NodeID != all[j].NodeID {
			return all[i].NodeID < all[j].NodeID
		}
		return all[i].ChunkIx < all[j].ChunkIx
	})
	if len(all) > k {
		all = all[:k]
	}
	return all
}

func sortCards(cards []Card) {
	sort.Slice(cards, func(i, j int) bool {
		if cards[i].Score != cards[j].Score {
			return cards[i].Score > cards[j].Score
		}
		return cards[i].NodeID < cards[j].NodeID
	})
}

// prioritizeTitleLookup prefers explicit navigation after ordinary relevance
// sorting. Scores remain relevance scores, not navigation priority; the ordered
// result slice is authoritative. Only CURRENT authorized non-superseded direct
// candidates qualify. Other direct matches and linked warnings remain in their
// existing relative order. Configured rerankers retain final authority.
func prioritizeTitleLookup(query string, cards []Card, directIDs []string) {
	direct := make(map[string]bool, len(directIDs))
	for _, id := range directIDs {
		direct[id] = true
	}
	exact := make(map[string]bool)
	for _, c := range cards {
		if direct[c.NodeID] && c.SupersededBy == "" && graph.MatchesFullTitle(query, c.Title) {
			exact[c.NodeID] = true
		}
	}
	if len(exact) == 0 {
		return
	}
	sort.SliceStable(cards, func(i, j int) bool {
		return exact[cards[i].NodeID] && !exact[cards[j].NodeID]
	})
}

// rerankHead returns the cards with the top-K reordered using the configured
// cross-encoder. Reranked cards are rescored above any fused tail card so the
// head stays on top after the final sort, with the tier-0 nudge preserved. It may
// also drop a head card whose current body-snapshot metadata no longer clears ACLs.
//
// fusedRaw carries the PRE-BOOST fused score per node id (the map Retrieve built),
// because this function ASSIGNS head[i].Score rather than adjusting it. Reading the
// fused component back off head[i].Score instead had two consequences: the tier-0
// factor was folded in twice (once by the card loop, once here, an effective 1.21x
// on the fused half of the blend), and the freshness decay was applied to the tail
// only, so the head never decayed at all.
//
// HTTP and `always` policy return an error when the CONFIGURED reranker could not
// score the head. That is a deliberate reversal: this used to return silently on a connect error, so a user who
// pointed MESH_RERANK_ENDPOINT at a server that was down (or, before the client split
// above, at a loopback address the SSRF guard refused) got byte-identical unreranked
// results, exit 0, and `mesh status` still printing "rerank active". Zero requests ever
// reached their server and nothing said so. Auto subscription mode is different by
// explicit policy: it returns local cards, labels the fallback in the trace/wire, and
// opens a circuit. Conditions that are NOT errors (no reranker configured, a head too
// short to reorder, a flat uninformative response) still leave the fused order intact.
func (r *Retriever) rerankHead(ctx context.Context, query string, cards []Card, fusedRaw map[string]float64, opt Options) ([]Card, error) {
	if r.rerankSetup != nil {
		return nil, fmt.Errorf("%w (%s): %w", ErrRerankUnavailable, r.rerankName, r.rerankSetup)
	}
	if r.rr == nil {
		return cards, nil
	}
	if len(cards) < 2 {
		if opt.Economics != nil {
			opt.Economics.Route = "too_few"
		}
		return cards, nil
	}
	k := rerankK
	compact, compactOK := r.rr.(rerank.CandidateReranker)
	if compactOK {
		if limit := compact.CandidateLimit(); limit > 0 && limit < k {
			k = limit
		}
	}
	if k > len(cards) {
		k = len(cards)
	}
	if k < 2 {
		if opt.Economics != nil {
			opt.Economics.Route = "too_few"
		}
		return cards, nil
	}
	candidateHead := cards[:k]
	if compactOK {
		call, route := subscriptionRoute(query, candidateHead, rerankPolicy(r.rr))
		if opt.Economics != nil {
			opt.Economics.CandidateCards = k
			opt.Economics.Route = route
		}
		if !call {
			return cards, nil
		}
	}
	ids := make([]string, k)
	for i := range candidateHead {
		ids[i] = candidateHead[i].NodeID
	}
	documents, err := r.store.NoteDocuments(ctx, ids)
	if err != nil {
		return nil, fmt.Errorf("rerank: reading note bodies for the head: %w", err)
	}
	// Re-check each candidate against metadata read in the SAME statement as its
	// body. If a watcher tightened an ACL since card enrichment, drop it before the
	// external reranker sees a byte. Refresh returned identity fields at the same time.
	head := make([]Card, 0, k)
	docs := make([]string, 0, k)
	for _, previous := range candidateHead {
		d, ok := documents[previous.NodeID]
		if !ok {
			continue
		}
		current, ok := currentCardFromMetadata(d.NoteMetadata, opt)
		if !ok {
			continue
		}
		current.Snippet, current.Score, current.Reason = previous.Snippet, previous.Score, previous.Reason
		head = append(head, current)
		docs = append(docs, d.Text)
	}
	cards = append(head, cards[k:]...)
	k = len(head)
	if k < 2 {
		if opt.Economics != nil {
			opt.Economics.Route = "too_few"
			opt.Economics.CandidateCards = k
		}
		return cards, nil
	}
	var res []rerank.Result
	var callStats rerank.CallStats
	if compactOK {
		candidates := make([]rerank.Candidate, k)
		for i := range head {
			candidates[i] = rerank.Candidate{
				Index:   i,
				Title:   head[i].Title,
				Snippet: head[i].Snippet,
				Reason:  head[i].Reason,
			}
		}
		if measured, ok := r.rr.(rerank.MeasuredCandidateReranker); ok {
			res, callStats, err = measured.RerankCandidatesMeasured(ctx, query, candidates)
		} else {
			callStats.Called = true
			started := time.Now()
			res, err = compact.RerankCandidates(ctx, query, candidates)
			callStats.Duration = time.Since(started)
		}
	} else {
		callStats.Called = true
		callStats.InputTokens = EstimateTokens(query)
		for _, doc := range docs {
			callStats.InputTokens += EstimateTokens(doc)
		}
		started := time.Now()
		res, err = r.rr.Rerank(ctx, query, docs)
		callStats.Duration = time.Since(started)
		if err == nil {
			callStats.OutputTokens = estimateRerankResults(res)
		}
	}
	if opt.Economics != nil {
		opt.Economics.CandidateCards = k
		opt.Economics.RerankCalled = callStats.Called
		opt.Economics.CacheHit = callStats.CacheHit
		opt.Economics.CircuitOpen = callStats.CircuitOpen
		opt.Economics.RerankInput = callStats.InputTokens
		opt.Economics.RerankOutput = callStats.OutputTokens
		opt.Economics.RerankProviderTokens = callStats.ProviderTokens
		opt.Economics.ProviderReported = callStats.ProviderReported
		opt.Economics.RerankLatencyMS = durationMillis(callStats.Duration)
		if callStats.CacheHit {
			opt.Economics.Route = "cache"
		} else {
			opt.Economics.Route = "model"
		}
	}
	if err != nil {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		// Auto subscription mode is an optimization, never a retrieval dependency.
		// Fall back to the already-ranked local cards, but make that state explicit
		// in the trace and persistent counters. `always` and HTTP retain fail-loud
		// semantics for evaluation and operator-enforced reranking.
		if compactOK && rerankPolicy(r.rr) == "auto" {
			if opt.Economics != nil {
				opt.Economics.Route = "fallback"
				opt.Economics.Fallback = true
			}
			return cards, nil
		}
		return nil, fmt.Errorf("%w (%s): %w\n  fix the provider CLI, start the endpoint (see tools/rerank-server), or unset MESH_RERANK_AGENT / MESH_RERANK_ENDPOINT to search without it", ErrRerankUnavailable, r.rerankName, err)
	}
	if len(res) != k {
		return nil, fmt.Errorf("%w (%s): returned %d scores for %d candidates", ErrRerankUnavailable, r.rerankName, len(res), k)
	}
	scores := make([]float64, k)
	seen := make([]bool, k)
	for _, x := range res {
		if x.Index < 0 || x.Index >= k {
			return nil, fmt.Errorf("%w (%s): returned out-of-range candidate index %d for %d candidates", ErrRerankUnavailable, r.rerankName, x.Index, k)
		}
		if seen[x.Index] {
			return nil, fmt.Errorf("%w (%s): returned duplicate candidate index %d", ErrRerankUnavailable, r.rerankName, x.Index)
		}
		if math.IsNaN(x.Score) || math.IsInf(x.Score, 0) {
			return nil, fmt.Errorf("%w (%s): returned non-finite score for candidate index %d", ErrRerankUnavailable, r.rerankName, x.Index)
		}
		seen[x.Index] = true
		scores[x.Index] = x.Score
	}
	lo, hi := scores[0], scores[0]
	for _, score := range scores[1:] {
		if score < lo {
			lo = score
		}
		if score > hi {
			hi = score
		}
	}
	// A flat (uninformative) rerank response carries no ranking signal; leave the
	// fused head order intact rather than collapsing it to alphabetical via the
	// constant-score branch of minMax.
	if hi == lo {
		return cards, nil
	}
	norm := minMaxFloored(scores)
	// The head's fused scores, normalized over the head, so the blend can give the
	// lexical/graph/vector signal a real vote instead of discarding it. Pure rerank
	// (alpha=1) threw away a correct fused top-1 on keyword queries; blending keeps a
	// strong fused hit in contention. Read from fusedRaw, NOT from head[i].Score:
	// head[i].Score already carries the tier-0 and freshness multipliers, and this
	// loop applies them again below.
	fused := make([]float64, k)
	for i := range head {
		fused[i] = fusedRaw[head[i].NodeID]
	}
	fusedNorm := minMaxFloored(fused)
	// Lift the reranked head above the untouched fused tail. Derive the base from
	// the actual max tail score (not a fixed constant) so the invariant holds
	// regardless of edge-weight magnitudes in graph expansion.
	base := 1.0
	for _, c := range cards[k:] {
		if c.Score+1.0 > base {
			base = c.Score + 1.0
		}
	}
	a := r.rerankBlend
	for i := range head {
		// Convex blend of cross-encoder relevance and fused score, both in [0,1].
		rel := a*norm[i] + (1-a)*fusedNorm[i]
		// The tier-0 nudge and the freshness decay multiply the relevance component
		// only, never the offset, so institutional-memory notes get a small (<=0.1)
		// tiebreak among near-equal scores without overriding a clearly better pick,
		// and the head decays with age exactly like the tail does.
		rel *= r.boostMult(head[i])
		head[i].Score = base + rel
		if head[i].Reason != "" {
			head[i].Reason += " +reranked"
		} else {
			head[i].Reason = "reranked"
		}
	}
	sortCards(cards)
	return cards, nil
}

// currentCards resolves actionable, authorized cards from the current notes-table
// snapshot. The in-memory graph is a ranking aid only: it refreshes independently and
// must never supply path/scope metadata for fresh indexed content.
func (r *Retriever) currentCards(ctx context.Context, ids []string, opt Options) (map[string]Card, error) {
	out := make(map[string]Card, len(ids))
	metadata, err := r.store.NoteMetadataFor(ctx, ids)
	if err != nil {
		return nil, err
	}
	for id, m := range metadata {
		if c, ok := currentCardFromMetadata(m, opt); ok {
			out[id] = c
		}
	}
	return out, nil
}

// currentCardFromMetadata applies both current read boundaries to the target and its
// optional superseder. SupersededBy affects both bytes and rank, so missing, deleted,
// or unreadable superseder metadata must leave the card completely undemoted.
func currentCardFromMetadata(m index.NoteMetadata, opt Options) (Card, bool) {
	c, ok := cardFromMetadata(m)
	if !ok || strings.EqualFold(strings.TrimSpace(m.State), "draft") || !scopeAllowed(c.Scope, opt.AllowedScopes) || !pathAllowed(c.Path, opt.AllowPath) {
		return Card{}, false
	}
	// NoteMetadataFor and NoteDocuments read this relation plus the superseding note's
	// CURRENT path/scope in the same statement. A stale in-memory graph can therefore
	// neither leak an old id nor acknowledge a newly fenced/deleted replacement.
	if m.SupersededBy != "" && m.SupersederPath != "" && !strings.EqualFold(strings.TrimSpace(m.SupersederState), "draft") &&
		scopeAllowed(m.SupersederScope, opt.AllowedScopes) &&
		pathAllowed(m.SupersederPath, opt.AllowPath) {
		c.SupersededBy = m.SupersededBy
	}
	return c, true
}

func cardFromMetadata(m index.NoteMetadata) (Card, bool) {
	if strings.TrimSpace(m.NodeID) == "" || strings.TrimSpace(m.NoteID) == "" ||
		strings.TrimSpace(m.Path) == "" || strings.TrimSpace(m.Title) == "" {
		return Card{NodeID: m.NodeID}, false
	}
	return Card{
		NodeID:            m.NodeID,
		NoteID:            m.NoteID,
		Title:             m.Title,
		Path:              m.Path,
		Type:              m.Type,
		Scope:             m.Scope,
		Tier0:             tier0Types[m.Type],
		MissingGuidance:   m.MissingGuidance,
		State:             m.State,
		Summary:           m.Summary,
		Template:          m.Template,
		TemplateVersion:   m.TemplateVersion,
		Updated:           m.Updated,
		VerifiedAt:        m.VerifiedAt,
		Source:            m.Source,
		SourceURL:         m.SourceURL,
		Sections:          m.Sections,
		SectionsTruncated: m.SectionsTruncated,
	}, true
}

// card builds a Card from a node id, reading title/path/type/tier-0 from the
// in-memory graph node. The bool is false when the node is not in the graph, in which
// case the Card is a shell. Production retrieval uses currentCards; this helper remains
// for graph-only diagnostics and its actionability contract.
func (r *Retriever) card(id string) (Card, bool) {
	c := Card{NodeID: id}
	n, ok := r.graph.Node(id)
	if !ok || n.Kind != "note" || strings.TrimSpace(n.Label) == "" ||
		strings.TrimSpace(n.NotePath) == "" || strings.TrimSpace(n.NoteID) == "" {
		return c, false
	}
	if state, ok := n.Attrs["status"].(string); ok {
		c.State = state
		if strings.EqualFold(strings.TrimSpace(state), "draft") {
			return c, false
		}
	}
	c.Title = n.Label
	c.Path = n.NotePath
	c.NoteID = n.NoteID
	if t, ok := n.Attrs["type"].(string); ok {
		c.Type = t
		c.Tier0 = tier0Types[t]
	}
	if sc, ok := n.Attrs["scope"].(string); ok {
		c.Scope = sc
	}
	return c, true
}

// scopeAllowed reports whether a card may be returned given an allowed-scope set.
// allowed==nil means unrestricted (the solo / no-ACL fast path). A card with no scope
// attr is treated as the fail-safe default (dev-only). Delegates to the one shared
// predicate so this surface cannot drift from the MCP/web scope checks.
func scopeAllowed(cardScope string, allowed map[string]bool) bool {
	return vault.ScopeAllowsCSV(cardScope, allowed)
}

// pathAllowed reports whether a card's note path clears the folder read boundary.
// allow==nil means unrestricted (no folder ACL configured). Search consumes the ranked
// SQL stream through this predicate; graph, vector and expansion arms apply it to current
// persisted metadata before their limits, so fenced rows never consume readable slots.
func pathAllowed(path string, allow func(string) bool) bool {
	return allow == nil || allow(path)
}

// freshnessTypes are NON-institutional notes that decay with age. Decisions,
// gotchas, post-mortems (tier-0) and entities/concepts/maps are structural memory
// and never decay; only loose notes + status pages do.
var freshnessTypes = map[string]bool{"note": true, "status": true, "": true}

// freshnessMult returns a (floor,1] multiplier from a note's age. Institutional
// types return 1 (no decay). An overdue review_by applies a small extra penalty.
//
// The curve is floor + (1-floor)*0.5^(age/halfLife): it DECAYS ASYMPTOTICALLY
// TOWARD the floor instead of being clipped at it.
//
// That distinction is the whole point. The old form was
// `mult = 0.5^(age/halfLife); if mult < 0.6 { mult = 0.6 }`, a hard clamp, and
// 0.5^(age/30) crosses 0.6 at just 22 days. So EVERY note older than ~22 days
// received the identical 0.6 multiplier and freshness stopped discriminating
// entirely: a 24-day-old note and an 11-year-old note ranked the same, ties
// never broke, and the alphabetical NodeID fallback decided the order. The
// signal was dead for essentially the whole corpus, which is exactly the corpus
// it exists to rank.
//
// Asymptotic decay keeps the same guarantee (an old note is demoted at most
// 40%, never buried) while staying strictly monotonic in age forever, so
// freshness always breaks a tie in favour of the fresher note.
const freshnessFloor = 0.6

// boostMult is everything that multiplies a card's fused signal: the tier-0 nudge
// and the freshness decay. It lives in one place because the head (rerankHead) and
// the tail (the card loop in Retrieve) must apply exactly the same factors.
//
// They did not. rerankHead ASSIGNS head[i].Score, so every multiplier the card loop
// folded in was discarded for the whole head, and rerankK (30) is larger than the
// default Limit (20), which makes the head the entire returned set. The tier-0 nudge
// was re-applied there by hand; the freshness decay was not, so
// MESH_FRESHNESS_HALFLIFE_DAYS was a silent no-op on any deployment with a rerank
// endpoint configured (freshness on and off produced identical scores).
func (r *Retriever) boostMult(c Card) float64 {
	m := 1.0
	if c.Tier0 {
		m *= tier0Mult
	}
	if c.SupersededBy != "" {
		m *= supersededMult
	}
	if r.freshHalfLife > 0 {
		m *= r.freshnessMult(c)
	}
	return m
}

func (r *Retriever) freshnessMult(c Card) float64 {
	r.freshOnce.Do(func() {
		if d, err := r.store.NoteDates(); err == nil {
			r.freshDates = d
		}
	})
	d, ok := r.freshDates[c.NoteID]
	if !ok {
		return 1
	}
	now := time.Now()
	mult := 1.0
	if freshnessTypes[c.Type] {
		if t, err := time.Parse("2006-01-02", d.Updated); err == nil {
			ageDays := now.Sub(t).Hours() / 24
			if ageDays > 0 {
				// Asymptotic toward freshnessFloor, never clipped at it: stays
				// strictly monotonic in age, so a fresher note always outranks a
				// staler one on a tie, at any age.
				decay := math.Pow(0.5, ageDays/float64(r.freshHalfLife))
				mult = freshnessFloor + (1-freshnessFloor)*decay
			}
		}
	}
	// Overdue review: a small nudge down regardless of type (it asked to be rechecked).
	if vault.ReviewOverdue(d.ReviewBy, now) {
		mult *= 0.85
	}
	return mult
}

func (r *Retriever) title(id string) string {
	if n, ok := r.graph.Node(id); ok {
		return n.Label
	}
	return id
}

type neighbor struct {
	id     string
	weight float64
}

// strongNeighbors returns the top-K note neighbors of id by edge weight,
// following reference edges in both directions and skipping hub (god) nodes.
func (r *Retriever) strongNeighbors(id string, k int) []neighbor {
	seen := map[string]float64{}
	consider := func(other string, w float64) {
		n, ok := r.graph.Node(other)
		if !ok || n.Kind != "note" || n.KnowledgeDegree > godDegree {
			return
		}
		if w > seen[other] {
			seen[other] = w
		}
	}
	for _, e := range r.graph.Neighbors(id) {
		if e.Relation == "references" {
			consider(e.Target, e.Weight)
		}
	}
	for _, e := range r.graph.RefsTo(id) {
		if e.Relation == "references" {
			consider(e.Source, e.Weight)
		}
	}
	out := make([]neighbor, 0, len(seen))
	for nid, w := range seen {
		out = append(out, neighbor{nid, w})
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].weight != out[j].weight {
			return out[i].weight > out[j].weight
		}
		return out[i].id < out[j].id
	})
	if k > 0 && len(out) > k {
		out = out[:k]
	}
	return out
}

type scored struct {
	id    string
	score float64
}

func topN(m map[string]float64, n int) []scored {
	out := make([]scored, 0, len(m))
	for id, s := range m {
		out = append(out, scored{id, s})
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].score != out[j].score {
			return out[i].score > out[j].score
		}
		return out[i].id < out[j].id
	})
	if n > 0 && len(out) > n {
		out = out[:n]
	}
	return out
}

// minMax scales scores to [0,1]. When all values are equal (or there is one),
// every value maps to 1 so the signal still contributes.
func minMax(xs []float64) []float64 {
	out := make([]float64, len(xs))
	if len(xs) == 0 {
		return out
	}
	lo, hi := xs[0], xs[0]
	for _, x := range xs {
		if x < lo {
			lo = x
		}
		if x > hi {
			hi = x
		}
	}
	if hi == lo {
		for i := range out {
			out[i] = 1
		}
		return out
	}
	for i, x := range xs {
		out[i] = (x - lo) / (hi - lo)
	}
	return out
}

// normFloor is the share of a normalized signal that every candidate keeps, so the
// weakest one never normalizes to exactly 0.
//
// minMax maps the minimum to 0, and 0 * tier0Mult == 0, so a multiplicative boost
// had nothing to act on over part of its own domain: the weakest of three FTS
// matches scored exactly 0.000000, and when that was a decision note its
// institutional-memory nudge bought it nothing at all - it sorted dead last on the
// alphabetical NodeID tie-break. Same shape as the historic 0.6 freshness clamp
// documented above freshnessMult: a boost that cannot discriminate over part of the
// corpus it exists to rank. A candidate that never matched a signal is simply absent
// from that signal's slice, so the floor lifts weak MATCHES only, never non-matches.
const normFloor = 0.02

// minMaxFloored is minMax lifted off zero by normFloor. The lexical signals and
// both halves of the rerank blend use it so multiplicative boosts always have a
// positive quantity to move. Vector scores keep their fixed cosine scale.
func minMaxFloored(xs []float64) []float64 {
	out := minMax(xs)
	for i, v := range out {
		out[i] = normFloor + (1-normFloor)*v
	}
	return out
}
