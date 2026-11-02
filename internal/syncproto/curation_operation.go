// SPDX-License-Identifier: LicenseRef-Mesh-Sustainable-Use-License
// Copyright (C) 2026 Bright Interaction AB

package syncproto

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
)

// CurationOperationProtocol is deliberately separate from ordinary sync. Old
// servers have no selected-operation routes and must never receive a fallback
// unrestricted sync or unconditional resolve.
const CurationOperationProtocol = 1

const (
	CurationActionPublish = "publish"
	CurationActionNoop    = "noop"
	CurationActionReject  = "reject"
)

// CurationSnapshot is one authorized, repository/policy-locked selected job and
// current winner. An absent note is distinguished from a present empty note.
type CurationSnapshot struct {
	Protocol       int         `json:"protocol"`
	VaultID        string      `json:"vault_id"`
	ClientID       int64       `json:"client_id"`
	Job            CurationJob `json:"job"`
	JobFingerprint string      `json:"job_fingerprint"`
	HeadSHA        string      `json:"head_sha"`
	NoteExists     bool        `json:"note_exists"`
	NoteSHA256     string      `json:"note_sha256"`
	NoteB64        string      `json:"note_b64"`
}

// CurationOperationRequest is immutable for its operation ID. Both raw model
// output and privately prepared final bytes are checked by the shared guards;
// neither the provider nor a replay may invent system provenance/verification.
type CurationOperationRequest struct {
	Protocol        int    `json:"protocol"`
	OperationID     string `json:"operation_id"`
	JobFingerprint  string `json:"job_fingerprint"`
	ExpectedExists  bool   `json:"expected_exists"`
	ExpectedSHA256  string `json:"expected_sha256"`
	Action          string `json:"action"`
	ModelB64        string `json:"model_b64,omitempty"`
	CandidateB64    string `json:"candidate_b64,omitempty"`
	CandidateSHA256 string `json:"candidate_sha256,omitempty"`
	RejectionReason string `json:"rejection_reason,omitempty"`
}

// Only resolved/rejected are terminal acknowledgements. prepared/uncertain are
// holds. Missing retained Git proof does not prove no prior publication, so a
// prepared publication never resumes automatically. not_found is returned with HTTP 200 by this new
// endpoint, so HTTP 404 always remains unsupported/opaque access failure.
type CurationOperationReceipt struct {
	Protocol        int    `json:"protocol"`
	VaultID         string `json:"vault_id"`
	ClientID        int64  `json:"client_id"`
	JobID           int64  `json:"job_id"`
	Path            string `json:"path"`
	OperationID     string `json:"operation_id"`
	RequestSHA256   string `json:"request_sha256,omitempty"`
	JobFingerprint  string `json:"job_fingerprint,omitempty"`
	ExpectedExists  bool   `json:"expected_exists"`
	ExpectedSHA256  string `json:"expected_sha256,omitempty"`
	Action          string `json:"action,omitempty"`
	CandidateSHA256 string `json:"candidate_sha256,omitempty"`
	Status          string `json:"status"`
	LandedHead      string `json:"landed_head,omitempty"`
	ContentB64      string `json:"content_b64,omitempty"`
	JobStatus       string `json:"job_status,omitempty"`
	Attempts        int64  `json:"attempts"`
}

// CurationJobFingerprint binds the exact pending row, including its attempt
// counter. Terminal receipts retain the original fingerprint; they do not
// pretend that a later resolved row still has its pending representation.
func CurationJobFingerprint(job CurationJob) string {
	data, _ := json.Marshal(job)
	hash := sha256.Sum256(data)
	return hex.EncodeToString(hash[:])
}

func CurationRequestSHA256(request CurationOperationRequest) string {
	data, _ := json.Marshal(request)
	hash := sha256.Sum256(data)
	return hex.EncodeToString(hash[:])
}
