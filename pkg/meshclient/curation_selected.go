// SPDX-License-Identifier: LicenseRef-Mesh-Sustainable-Use-License
// Copyright (C) 2026 Bright Interaction AB

package meshclient

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"

	"github.com/bright-interaction/mesh/internal/syncproto"
	"github.com/bright-interaction/mesh/internal/vault"
)

var ErrSelectedReview = errors.New("selected curation requires review")

// SelectedCurationResult distinguishes a proved hub receipt from local refresh.
// Parked means local edits/deletions remain intact; the accepted bytes remain in
// the private journal and may be retrieved by reconciling this same operation.
type SelectedCurationResult struct {
	Receipt         syncproto.CurationOperationReceipt
	Refreshed       bool
	Parked          bool
	RejectionReason string
}

// SelectedCurationPrepare must call providerStarted immediately before provider
// dispatch. Once that marker is durable, a crash/error cannot silently pay for a
// second model call. It returns a final private candidate or deterministic reject.
type SelectedCurationPrepare func(syncproto.CurationSnapshot, func() error) (syncproto.CurationOperationRequest, error)

type selectedJournal struct {
	Version     int                                 `json:"version"`
	HubURL      string                              `json:"hub_url"`
	Snapshot    syncproto.CurationSnapshot          `json:"snapshot"`
	Request     syncproto.CurationOperationRequest  `json:"request"`
	Phase       string                              `json:"phase"`
	LocalExists bool                                `json:"local_exists"`
	LocalHash   string                              `json:"local_hash"`
	TrackedHash string                              `json:"tracked_hash"`
	Receipt     *syncproto.CurationOperationReceipt `json:"receipt,omitempty"`
}

func (c *Client) CurationSnapshot(id int64) (syncproto.CurationSnapshot, error) {
	var s syncproto.CurationSnapshot
	err := c.rpc("GET", fmt.Sprintf("/v1/curation/jobs/%d/snapshot", id), nil, true, &s)
	return s, err
}
func (c *Client) CurationOperation(id int64, op string) (syncproto.CurationOperationReceipt, error) {
	var r syncproto.CurationOperationReceipt
	err := c.rpc("GET", fmt.Sprintf("/v1/curation/jobs/%d/operations/%s", id, op), nil, true, &r)
	return r, err
}
func (c *Client) ApplyCurationOperation(id int64, q syncproto.CurationOperationRequest) (syncproto.CurationOperationReceipt, error) {
	var r syncproto.CurationOperationReceipt
	err := c.rpc("POST", fmt.Sprintf("/v1/curation/jobs/%d/operations", id), q, true, &r)
	return r, err
}

// RunSelectedCuration never lists/syncs the vault or touches its ordinary outbox.
// It serializes with regular sync, journals before dispatch, and uses only the
// exact-job endpoints. An older/opaque 404 is an error, never a sync fallback.
func RunSelectedCuration(ctx context.Context, root string, id int64, prepare SelectedCurationPrepare) (SelectedCurationResult, error) {
	return runSelectedCuration(ctx, root, id, prepare, false)
}

// RunScheduledCuration explicitly permits a fresh semantic attempt on a later
// scheduler pass, only after an authenticated terminal rejection receipt. Lost
// ACKs, provider uncertainty, policy/CAS holds never rotate the operation.
func RunScheduledCuration(ctx context.Context, root string, id int64, prepare SelectedCurationPrepare) (SelectedCurationResult, error) {
	return runSelectedCuration(ctx, root, id, prepare, true)
}

func runSelectedCuration(ctx context.Context, root string, id int64, prepare SelectedCurationPrepare, advanceRejected bool) (SelectedCurationResult, error) {
	var result SelectedCurationResult
	if ctx == nil {
		ctx = context.Background()
	}
	if id <= 0 {
		return result, fmt.Errorf("selected job ID must be positive")
	}
	if err := vault.RequireRoot(root); err != nil {
		return result, err
	}
	journalPath, err := selectedJournalPath(root, id)
	if err != nil {
		return result, err
	}
	release, err := acquireVaultSyncLockContext(ctx, root)
	if err != nil {
		return result, err
	}
	defer release()
	creds, err := readCredentials(root)
	if err != nil {
		return result, err
	}
	cl := NewWithContext(ctx, creds.HubURL, creds.Token)
loadOperation:
	var j selectedJournal
	data, err := readSelectedFile(journalPath, 8<<20)
	existing := err == nil
	if err != nil && !os.IsNotExist(err) {
		return result, err
	}
	if existing {
		if err := json.Unmarshal(data, &j); err != nil {
			return result, fmt.Errorf("%w: corrupt selected operation journal", ErrSelectedReview)
		}
		if err := validateSelectedJournal(j, creds, id); err != nil {
			return result, err
		}
	} else {
		s, err := cl.CurationSnapshot(id)
		if err != nil {
			return result, fmt.Errorf("selected snapshot (no legacy fallback): %w", err)
		}
		if err := validateSelectedSnapshot(s, creds, id); err != nil {
			return result, err
		}
		state, err := readSelectedState(root, creds)
		if err != nil {
			return result, err
		}
		exists, hash, err := selectedLocal(ctx, root, s.Job.Path)
		if err != nil {
			return result, err
		}
		var nonce [16]byte
		if _, err := rand.Read(nonce[:]); err != nil {
			return result, err
		}
		j = selectedJournal{Version: 1, HubURL: strings.TrimRight(creds.HubURL, "/"), Snapshot: s, Phase: "new", LocalExists: exists, LocalHash: hash, TrackedHash: state.Hashes[s.Job.Path], Request: syncproto.CurationOperationRequest{Protocol: syncproto.CurationOperationProtocol, OperationID: hex.EncodeToString(nonce[:]), JobFingerprint: s.JobFingerprint, ExpectedExists: s.NoteExists, ExpectedSHA256: s.NoteSHA256}}
		if err := saveSelectedJournal(journalPath, j); err != nil {
			return result, err
		}
	}
	// An existing prepared operation must be reconciled before a retry. Even a
	// terminal cached receipt is re-authorized on the server after a restart.
	if existing && j.Phase != "new" {
		r, err := cl.CurationOperation(id, j.Request.OperationID)
		if err != nil {
			return result, fmt.Errorf("reconcile selected operation: %w", err)
		}
		if err := validateSelectedReceipt(j, r, r.Status == "not_found"); err != nil {
			return result, err
		}
		switch r.Status {
		case "rejected":
			if advanceRejected && r.JobStatus == "pending" {
				if err := archiveSelectedJournal(journalPath, j, r); err != nil {
					return result, err
				}
				goto loadOperation
			}
			return finishSelected(ctx, root, journalPath, &j, r, false)
		case "resolved":
			return finishSelected(ctx, root, journalPath, &j, r, false)
		case "not_found":
			if j.Phase == "provider_started" || j.Phase == "provider_uncertain" {
				return result, fmt.Errorf("%w: prior selected job provider outcome is uncertain; reconcile manually, never repeat blindly", ErrSelectedReview)
			}
			if j.Phase != "prepared" && j.Phase != "dispatched" {
				return result, fmt.Errorf("%w: selected operation has no durable candidate", ErrSelectedReview)
			}
		default:
			return result, fmt.Errorf("%w: selected operation is %s; no provider or publication replay", ErrSelectedReview, r.Status)
		}
	}
	if existing && j.Phase == "new" {
		fresh, err := cl.CurationSnapshot(id)
		if err != nil {
			return result, fmt.Errorf("refresh selected snapshot: %w", err)
		}
		if err := validateSelectedSnapshot(fresh, creds, id); err != nil {
			return result, err
		}
		if fresh.ClientID != j.Snapshot.ClientID || fresh.JobFingerprint != j.Snapshot.JobFingerprint || fresh.NoteExists != j.Snapshot.NoteExists || fresh.NoteSHA256 != j.Snapshot.NoteSHA256 {
			return result, fmt.Errorf("%w: selected snapshot changed before provider", ErrSelectedReview)
		}
	}
	if j.Phase == "new" {
		if prepare == nil {
			return result, fmt.Errorf("selected candidate preparation unavailable")
		}
		started := false
		q, err := prepare(j.Snapshot, func() error {
			if started {
				return fmt.Errorf("provider already started for selected operation")
			}
			started = true
			j.Phase = "provider_started"
			return saveSelectedJournal(journalPath, j)
		})
		if err != nil {
			if started {
				j.Phase = "provider_uncertain"
				if e := saveSelectedJournal(journalPath, j); e != nil {
					return result, errors.Join(err, e)
				}
			}
			return result, err
		}
		q.Protocol, j.Request.Protocol = syncproto.CurationOperationProtocol, syncproto.CurationOperationProtocol
		q.OperationID, q.JobFingerprint = j.Request.OperationID, j.Request.JobFingerprint
		q.ExpectedExists, q.ExpectedSHA256 = j.Request.ExpectedExists, j.Request.ExpectedSHA256
		if err := validateSelectedRequest(q); err != nil {
			return result, err
		}
		j.Request = q
		j.Phase = "prepared"
		if err := saveSelectedJournal(journalPath, j); err != nil {
			return result, err
		}
	}
	// A local save during provider preparation is a review boundary, not a poison
	// attempt. Pre-existing dirty target bytes are allowed but will be parked.
	exists, hash, err := selectedLocal(ctx, root, j.Snapshot.Job.Path)
	if err != nil {
		return result, err
	}
	if exists != j.LocalExists || hash != j.LocalHash {
		return result, fmt.Errorf("%w: selected local note changed during preparation", ErrSelectedReview)
	}
	unlock, err := selectedNoteLock(root, j.Snapshot)
	if err != nil {
		return result, err
	}
	defer unlock()
	j.Phase = "dispatched"
	if err := saveSelectedJournal(journalPath, j); err != nil {
		return result, err
	}
	r, err := cl.ApplyCurationOperation(id, j.Request)
	if err != nil {
		return result, fmt.Errorf("selected operation acknowledgement uncertain; reconcile this operation before retry: %w", err)
	}
	if err := validateSelectedReceipt(j, r, false); err != nil {
		return result, err
	}
	return finishSelected(ctx, root, journalPath, &j, r, true)
}

func selectedJournalPath(root string, id int64) (string, error) {
	// .mesh and the journal directory must be real private directories. They are
	// never accepted from network data and must not redirect candidate bytes.
	for _, rel := range []string{".mesh", filepath.Join(".mesh", "curation-operations")} {
		p := filepath.Join(root, rel)
		if err := os.Mkdir(p, 0700); err != nil && !os.IsExist(err) {
			return "", err
		}
		fi, err := os.Lstat(p)
		if err != nil {
			return "", err
		}
		if !fi.IsDir() || fi.Mode()&os.ModeSymlink != 0 {
			return "", fmt.Errorf("%w: unsafe private journal directory", ErrSelectedReview)
		}
	}
	p := filepath.Join(root, ".mesh", "curation-operations", fmt.Sprintf("%d.json", id))
	if fi, err := os.Lstat(p); err == nil && (!fi.Mode().IsRegular() || fi.Mode()&os.ModeSymlink != 0) {
		return "", fmt.Errorf("%w: unsafe selected journal", ErrSelectedReview)
	} else if err != nil && !os.IsNotExist(err) {
		return "", err
	}
	return p, nil
}
func saveSelectedJournal(path string, j selectedJournal) error {
	b, err := json.MarshalIndent(j, "", "  ")
	if err != nil {
		return err
	}
	if err := writeFileAtomicPrivate(path, b, 0600); err != nil {
		return err
	}
	// Unlike ordinary note-sync's portability fallback, the operation must not be
	// dispatched unless its directory entries have acknowledged durability.
	if err := syncSelectedDirectory(filepath.Dir(path)); err != nil {
		return err
	}
	return syncSelectedDirectory(filepath.Dir(filepath.Dir(path)))
}
func selectedLocal(ctx context.Context, root, rel string) (bool, string, error) {
	if _, ok := vault.SafeSyncNotePath(rel); !ok || strings.Contains(rel, ".sync-conflict-") {
		return false, "", fmt.Errorf("unsafe selected path")
	}
	b, err := vault.ReadConfinedFileContext(ctx, root, rel, 1<<20)
	if os.IsNotExist(err) {
		return false, "", nil
	}
	if err != nil {
		return false, "", err
	}
	return true, contentHash(b), nil
}
func readSelectedState(root string, creds credentials) (syncState, error) {
	state, _, err := readSelectedStateDocument(root, creds)
	return state, err
}

// The state being validated and the extension-preserving object being rewritten
// are decoded from these exact same bounded bytes, never a second unbounded read.
func readSelectedStateDocument(root string, creds credentials) (syncState, map[string]json.RawMessage, error) {
	state := syncState{Hashes: map[string]string{}}
	raw := map[string]json.RawMessage{}
	b, err := readSelectedFile(statePath(root), 64<<20)
	if os.IsNotExist(err) {
		return state, raw, nil
	}
	if err != nil {
		return state, nil, err
	}
	if err := json.Unmarshal(b, &raw); err != nil || raw == nil {
		return state, nil, fmt.Errorf("%w: corrupt or null sync state", ErrSelectedReview)
	}
	if err := json.Unmarshal(b, &state); err != nil {
		return state, nil, fmt.Errorf("%w: corrupt sync state; no selected refresh", ErrSelectedReview)
	}
	if state.Hashes == nil {
		state.Hashes = map[string]string{}
	}
	if (state.VaultID != "" && state.VaultID != creds.VaultID) || (state.HubURL != "" && strings.TrimRight(state.HubURL, "/") != strings.TrimRight(creds.HubURL, "/")) {
		return state, nil, fmt.Errorf("%w: sync identity changed", ErrSelectedReview)
	}
	return state, raw, nil
}
func validateSelectedSnapshot(s syncproto.CurationSnapshot, c credentials, id int64) error {
	b, err := base64.StdEncoding.DecodeString(s.NoteB64)
	incoming, incomingErr := base64.StdEncoding.DecodeString(s.Job.IncomingB64)
	if incomingErr != nil || len(incoming) > 1<<20 || len(b) > 1<<20 {
		return fmt.Errorf("%w: oversize or malformed selected snapshot", ErrSelectedReview)
	}
	if err != nil || s.Protocol != syncproto.CurationOperationProtocol || s.VaultID == "" || s.VaultID != c.VaultID || s.ClientID <= 0 || s.Job.ID != id || s.Job.Status != "pending" || s.JobFingerprint != syncproto.CurationJobFingerprint(s.Job) || s.HeadSHA == "" || s.NoteSHA256 != contentHash(b) || (!s.NoteExists && len(b) != 0) {
		return fmt.Errorf("%w: invalid selected snapshot binding", ErrSelectedReview)
	}
	if _, ok := vault.SafeSyncNotePath(s.Job.Path); !ok || strings.Contains(s.Job.Path, ".sync-conflict-") {
		return fmt.Errorf("%w: unsafe selected note path", ErrSelectedReview)
	}
	return nil
}
func validateSelectedJournal(j selectedJournal, c credentials, id int64) error {
	if j.Version != 1 || j.HubURL != strings.TrimRight(c.HubURL, "/") {
		return fmt.Errorf("%w: selected journal identity changed", ErrSelectedReview)
	}
	if err := validateSelectedSnapshot(j.Snapshot, c, id); err != nil {
		return err
	}
	q := j.Request
	if len(q.OperationID) != 32 {
		return fmt.Errorf("%w: invalid operation ID", ErrSelectedReview)
	}
	if _, err := hex.DecodeString(q.OperationID); err != nil || strings.ToLower(q.OperationID) != q.OperationID {
		return fmt.Errorf("%w: invalid operation ID", ErrSelectedReview)
	}
	if q.Protocol != syncproto.CurationOperationProtocol || q.JobFingerprint != j.Snapshot.JobFingerprint || q.ExpectedExists != j.Snapshot.NoteExists || q.ExpectedSHA256 != j.Snapshot.NoteSHA256 {
		return fmt.Errorf("%w: selected request binding changed", ErrSelectedReview)
	}
	switch j.Phase {
	case "new", "provider_started", "provider_uncertain":
		return nil
	case "prepared", "dispatched", "resolved", "rejected":
		return validateSelectedRequest(q)
	default:
		return fmt.Errorf("%w: unknown selected journal phase", ErrSelectedReview)
	}
}
func validateSelectedRequest(q syncproto.CurationOperationRequest) error {
	switch q.Action {
	case syncproto.CurationActionPublish:
		b, err := base64.StdEncoding.DecodeString(q.CandidateB64)
		model, e := base64.StdEncoding.DecodeString(q.ModelB64)
		if err != nil || e != nil || len(b) == 0 || len(b) > 1<<20 || len(model) == 0 || len(model) > 1<<20 || q.CandidateSHA256 != contentHash(b) || q.RejectionReason != "" {
			return fmt.Errorf("%w: invalid private candidate", ErrSelectedReview)
		}
	case syncproto.CurationActionNoop:
		if q.ModelB64 != "" || q.CandidateB64 != "" || q.CandidateSHA256 != "" || q.RejectionReason != "" {
			return fmt.Errorf("%w: invalid selected noop", ErrSelectedReview)
		}
	case syncproto.CurationActionReject:
		model, err := base64.StdEncoding.DecodeString(q.ModelB64)
		if err != nil || len(model) > 1<<20 {
			return fmt.Errorf("%w: invalid bounded rejection evidence", ErrSelectedReview)
		}
		if strings.TrimSpace(q.RejectionReason) == "" || q.CandidateB64 != "" || q.CandidateSHA256 != "" {
			return fmt.Errorf("%w: invalid selected reject", ErrSelectedReview)
		}
	default:
		return fmt.Errorf("%w: invalid selected action", ErrSelectedReview)
	}
	return nil
}
func validateSelectedReceipt(j selectedJournal, r syncproto.CurationOperationReceipt, notFound bool) error {
	s, q := j.Snapshot, j.Request
	if r.Protocol != syncproto.CurationOperationProtocol || r.VaultID != s.VaultID || r.ClientID != s.ClientID || r.JobID != s.Job.ID || r.Path != s.Job.Path || r.OperationID != q.OperationID {
		return fmt.Errorf("%w: selected receipt identity mismatch", ErrSelectedReview)
	}
	if notFound {
		return nil
	}
	if r.JobFingerprint != q.JobFingerprint || r.ExpectedExists != q.ExpectedExists || r.ExpectedSHA256 != q.ExpectedSHA256 || r.Action != q.Action || r.CandidateSHA256 != q.CandidateSHA256 || r.RequestSHA256 != syncproto.CurationRequestSHA256(q) {
		return fmt.Errorf("%w: selected receipt request mismatch", ErrSelectedReview)
	}
	if r.Status == "resolved" {
		b, err := base64.StdEncoding.DecodeString(r.ContentB64)
		want := q.CandidateSHA256
		if q.Action == syncproto.CurationActionNoop {
			want = q.ExpectedSHA256
		}
		if err != nil || r.JobStatus != "resolved" || !selectedGitHead(r.LandedHead) || r.Attempts != s.Job.Attempts || q.Action == syncproto.CurationActionReject || contentHash(b) != want {
			return fmt.Errorf("%w: selected receipt lacks matching publication proof", ErrSelectedReview)
		}
	}
	if r.Status == "rejected" && (q.Action != syncproto.CurationActionReject || r.Attempts != s.Job.Attempts+1 || (r.JobStatus != "pending" && r.JobStatus != "failed")) {
		return fmt.Errorf("%w: selected rejection lacks one-attempt proof", ErrSelectedReview)
	}
	return nil
}
func selectedNoteLock(root string, s syncproto.CurationSnapshot) (func(), error) {
	b, _ := base64.StdEncoding.DecodeString(s.NoteB64)
	fm, _, err := vault.ParseFrontmatter(b)
	if err != nil || fm == nil || fm.ID == "" {
		return func() {}, nil
	}
	// Never derive a lock pathname from arbitrary metadata.
	if filepath.Base(fm.ID) != fm.ID || strings.ContainsAny(fm.ID, "/\\") || fm.ID == "." || fm.ID == ".." {
		return nil, fmt.Errorf("%w: unsafe note identity", ErrSelectedReview)
	}
	dir := filepath.Join(root, ".mesh", "note-locks")
	if err := os.MkdirAll(dir, 0700); err != nil {
		return nil, err
	}
	fi, err := os.Lstat(dir)
	if err != nil {
		return nil, err
	}
	if !fi.IsDir() || fi.Mode()&os.ModeSymlink != 0 {
		return nil, fmt.Errorf("%w: unsafe note lock directory", ErrSelectedReview)
	}
	p := filepath.Join(dir, fm.ID+".lock")
	f, err := os.OpenFile(p, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if err != nil {
		return nil, fmt.Errorf("%w: selected note is locked: %v", ErrSelectedReview, err)
	}
	if err := f.Sync(); err != nil {
		_ = f.Close()
		_ = os.Remove(p)
		syncDir(dir)
		return nil, err
	}
	if err := f.Close(); err != nil {
		_ = os.Remove(p)
		syncDir(dir)
		return nil, err
	}
	syncDir(dir)
	return func() {
		_ = os.Remove(p)
		syncDir(dir)
	}, nil
}
func finishSelected(ctx context.Context, root, path string, j *selectedJournal, r syncproto.CurationOperationReceipt, lockHeld bool) (SelectedCurationResult, error) {
	result := SelectedCurationResult{Receipt: r, RejectionReason: j.Request.RejectionReason}
	if r.Status != "resolved" && r.Status != "rejected" {
		return result, fmt.Errorf("%w: selected operation remains %s", ErrSelectedReview, r.Status)
	}
	j.Receipt = &r
	j.Phase = r.Status
	if err := saveSelectedJournal(path, *j); err != nil {
		return result, err
	}
	if r.Status == "rejected" {
		return result, nil
	}
	if r.Action == syncproto.CurationActionNoop && !j.Snapshot.NoteExists {
		return result, nil
	}
	if !lockHeld {
		unlock, err := selectedNoteLock(root, j.Snapshot)
		if err != nil {
			return result, err
		}
		defer unlock()
	}
	creds, err := readCredentials(root)
	if err != nil {
		return result, err
	}
	if _, err := readSelectedState(root, creds); err != nil {
		return result, err
	}
	b, _ := base64.StdEncoding.DecodeString(r.ContentB64)
	exists, hash, err := selectedLocal(ctx, root, j.Snapshot.Job.Path)
	if err != nil {
		return result, err
	}
	// Dirty bytes/deletions present before curation are not disposable scratch.
	eligible := j.LocalExists && j.LocalHash == j.Snapshot.NoteSHA256
	if !j.LocalExists && j.TrackedHash == "" {
		eligible = true
	}
	if exists && hash == contentHash(b) {
		eligible = true
	}
	if !eligible || exists != j.LocalExists || hash != j.LocalHash {
		if !(exists && hash == contentHash(b)) {
			result.Parked = true
			return result, nil
		}
	}
	if !exists || hash != contentHash(b) {
		if eligible && j.Snapshot.NoteExists {
			if err := archiveSelectedWinner(root, j.Snapshot); err != nil {
				return result, err
			}
		}
		applied, _, err := replaceFileDurable(root, j.Snapshot.Job.Path, exists, hash, b)
		if err != nil {
			return result, err
		}
		if !applied {
			result.Parked = true
			return result, nil
		}
	}
	// Updating one path must NOT advance the global head/tombstone cursor or
	// discard extensions: regular sync still owes every unrelated inbound change.
	if err := writeSelectedHash(root, creds, j.Snapshot.Job.Path, contentHash(b)); err != nil {
		return result, err
	}
	result.Refreshed = true
	return result, nil
}

func archiveSelectedWinner(root string, s syncproto.CurationSnapshot) error {
	b, err := base64.StdEncoding.DecodeString(s.NoteB64)
	if err != nil {
		return err
	}
	fm, _, err := vault.ParseFrontmatter(b)
	if err != nil || fm == nil || fm.ID == "" {
		return nil
	}
	if filepath.Base(fm.ID) != fm.ID || strings.ContainsAny(fm.ID, "/\\") || fm.ID == "." || fm.ID == ".." {
		return fmt.Errorf("%w: unsafe history identity", ErrSelectedReview)
	}
	dir := filepath.Join(root, ".mesh", "note-history", fm.ID)
	if err := os.MkdirAll(dir, 0700); err != nil {
		return err
	}
	for _, p := range []string{filepath.Dir(dir), dir} {
		fi, err := os.Lstat(p)
		if err != nil {
			return err
		}
		if !fi.IsDir() || fi.Mode()&os.ModeSymlink != 0 {
			return fmt.Errorf("%w: unsafe selected history directory", ErrSelectedReview)
		}
	}
	p := filepath.Join(dir, s.NoteSHA256+".md")
	created, err := writeNewFileDurable(p, b, 0600)
	if err != nil {
		return err
	}
	if !created {
		old, err := os.ReadFile(p)
		if err != nil {
			return err
		}
		if contentHash(old) != s.NoteSHA256 {
			return fmt.Errorf("%w: historical snapshot differs", ErrSelectedReview)
		}
	}
	return nil
}

func writeSelectedHash(root string, creds credentials, path, hash string) error {
	state, raw, err := readSelectedStateDocument(root, creds)
	if err != nil {
		return err
	}
	state.Hashes[path] = hash
	raw["hashes"], err = json.Marshal(state.Hashes)
	if err != nil {
		return err
	}
	b, err := json.MarshalIndent(raw, "", "  ")
	if err != nil {
		return err
	}
	return writeFileAtomic(statePath(root), b, 0600)
}

func syncSelectedDirectory(path string) error {
	d, err := os.Open(path)
	if err != nil {
		return err
	}
	defer d.Close()
	return d.Sync()
}
func readSelectedFile(path string, limit int64) ([]byte, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() || info.Size() > limit {
		return nil, fmt.Errorf("%w: invalid or oversize selected metadata file", ErrSelectedReview)
	}
	b, err := io.ReadAll(io.LimitReader(f, limit+1))
	if err != nil {
		return nil, err
	}
	if int64(len(b)) > limit {
		return nil, fmt.Errorf("%w: oversize selected metadata file", ErrSelectedReview)
	}
	return b, nil
}
func selectedGitHead(head string) bool {
	b, err := hex.DecodeString(head)
	return err == nil && (len(b) == 20 || len(b) == 32) && strings.ToLower(head) == head
}
func archiveSelectedJournal(path string, j selectedJournal, r syncproto.CurationOperationReceipt) error {
	if err := validateSelectedReceipt(j, r, false); err != nil {
		return err
	}
	j.Receipt = &r
	j.Phase = "rejected"
	data, err := json.MarshalIndent(j, "", "  ")
	if err != nil {
		return err
	}
	archive := filepath.Join(filepath.Dir(path), fmt.Sprintf("%d-%s.json", j.Snapshot.Job.ID, j.Request.OperationID))
	created, err := writeNewFileDurable(archive, data, 0600)
	if err != nil {
		return err
	}
	if !created {
		old, err := readSelectedFile(archive, 8<<20)
		if err != nil {
			return err
		}
		if contentHash(old) != contentHash(data) {
			return fmt.Errorf("%w: prior rejection archive differs", ErrSelectedReview)
		}
	}
	if err := syncSelectedDirectory(filepath.Dir(path)); err != nil {
		return err
	}
	if err := os.Remove(path); err != nil {
		return err
	}
	return syncSelectedDirectory(filepath.Dir(path))
}

// UnfinishedCurationOperations returns only private in-flight operation metadata.
// A server may have resolved a job while its ACK was lost, so the periodic worker
// must reconcile these even when the pending-job list no longer includes them.
func UnfinishedCurationOperations(root string) ([]syncproto.CurationJob, error) {
	dir := filepath.Join(root, ".mesh", "curation-operations")
	entries, err := os.ReadDir(dir)
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	creds, err := readCredentials(root)
	if err != nil {
		return nil, err
	}
	var jobs []syncproto.CurationJob
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".json") {
			continue
		}
		id, err := strconv.ParseInt(strings.TrimSuffix(entry.Name(), ".json"), 10, 64)
		if err != nil || id <= 0 {
			continue
		} // immutable archived attempts have a hyphen
		path, err := selectedJournalPath(root, id)
		if err != nil {
			return nil, err
		}
		b, err := readSelectedFile(path, 8<<20)
		if err != nil {
			return nil, err
		}
		var j selectedJournal
		if err := json.Unmarshal(b, &j); err != nil {
			return nil, fmt.Errorf("%w: corrupt selected operation journal", ErrSelectedReview)
		}
		if err := validateSelectedJournal(j, creds, id); err != nil {
			return nil, err
		}
		if j.Phase != "resolved" && j.Phase != "rejected" {
			job := j.Snapshot.Job
			job.IncomingB64 = ""
			jobs = append(jobs, job)
		}
	}
	sort.Slice(jobs, func(i, j int) bool { return jobs[i].ID < jobs[j].ID })
	return jobs, nil
}
