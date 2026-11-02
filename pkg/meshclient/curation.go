// SPDX-License-Identifier: LicenseRef-Mesh-Sustainable-Use-License
// Copyright (C) 2026 Bright Interaction AB

package meshclient

import (
	"context"
	"fmt"
	"net/url"
	"strconv"
	"strings"

	"github.com/bright-interaction/mesh/internal/syncproto"
)

// ClientForVault builds an authed transport from a joined vault's stored
// credentials (.mesh/credentials). Used by the mesh-curator to call the hub's
// curation endpoints with the same token it syncs with.
func ClientForVault(vaultDir string) (*Client, error) {
	return ClientForVaultContext(context.Background(), vaultDir)
}

func ClientForVaultContext(ctx context.Context, vaultDir string) (*Client, error) {
	creds, err := readCredentials(vaultDir)
	if err != nil {
		return nil, err
	}
	return NewWithContext(ctx, creds.HubURL, creds.Token), nil
}

// CurationJobs lists the hub's pending curation markers (metadata only).
func (c *Client) CurationJobs() ([]syncproto.CurationJob, error) {
	var resp syncproto.CurationJobsResponse
	if err := c.rpc("GET", "/v1/curation/jobs", nil, true, &resp); err != nil {
		return nil, err
	}
	return resp.Jobs, nil
}

// CurationActivityPageSize is the largest page /v1/curation/activity will serve: the
// hub clamps ?limit to 1..200. Asking for more is not an error, it silently yields 200
// rows, so history below the newest page is reached by PAGING with the cursor and never
// by raising the limit.
const CurationActivityPageSize = 200

// curationActivityCursorHeader carries the position the hub's activity read stopped at;
// it is sent back as ?cursor= to get the page below.
//
// This name must match internal/hub's constant of the same name exactly. Reading the
// wrong header yields "" on every response, which this client cannot tell apart from
// "the history ended here", so it would quietly go back to seeing only the newest page:
// the precise defect the hub-side cursor was added to close.
// TestCurationActivityCursorHeaderMatchesTheHub parses the hub source and pins the two
// names together so they cannot drift.
const curationActivityCursorHeader = "X-Mesh-Curation-Cursor"

// CurationActivityPage lists ONE page of the hub's terminal curation jobs (resolved +
// failed), most-recent first, for the team-wide "what did the curator do" view, and
// returns the cursor for the page below it.
//
// cursor is "" for the newest page, otherwise a cursor a previous call handed back.
// limit is the size of THIS page; the hub clamps it to 1..200 and uses its own default
// (50) when limit <= 0. Reaching further back is paging, not a bigger limit.
//
// The returned cursor is empty ONLY when the hub says the history ended inside this
// page. A page may come back short, or even empty, and still carry a cursor: the hub
// omits rows in folders this token may not read, so a walk that stops on a short page
// never reaches the readable rows sitting below a fenced run. Stop on the empty cursor.
func (c *Client) CurationActivityPage(cursor string, limit int) ([]syncproto.CurationJob, string, error) {
	q := url.Values{}
	if limit > 0 {
		q.Set("limit", strconv.Itoa(limit))
	}
	if cursor != "" {
		q.Set("cursor", cursor)
	}
	path := "/v1/curation/activity"
	if len(q) > 0 {
		path += "?" + q.Encode()
	}
	var resp syncproto.CurationJobsResponse
	hdr, err := c.rpcHeaders("GET", path, nil, true, &resp)
	if err != nil {
		return nil, "", err
	}
	return resp.Jobs, strings.TrimSpace(hdr.Get(curationActivityCursorHeader)), nil
}

// CurationJob fetches one job including its IncomingB64 (the losing version).
func (c *Client) CurationJob(id int64) (syncproto.CurationJob, error) {
	var job syncproto.CurationJob
	err := c.rpc("GET", fmt.Sprintf("/v1/curation/jobs/%d", id), nil, true, &job)
	return job, err
}

// ResolveCuration marks a job resolved, recording the HEAD the merge landed at.
func (c *Client) ResolveCuration(id int64, resolvedHead string) error {
	body := map[string]string{"resolved_head": resolvedHead}
	return c.rpc("POST", fmt.Sprintf("/v1/curation/jobs/%d/resolve", id), body, true, nil)
}

// FailCuration charges a failed attempt against a job; the hub marks it terminal
// at the attempt cap so a job the agent cannot satisfy stops being retried.
func (c *Client) FailCuration(id int64, reason string) error {
	body := map[string]string{"reason": reason}
	return c.rpc("POST", fmt.Sprintf("/v1/curation/jobs/%d/fail", id), body, true, nil)
}
