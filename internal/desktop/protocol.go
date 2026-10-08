// SPDX-License-Identifier: LicenseRef-Mesh-Sustainable-Use-License
// Copyright (C) 2026 Bright Interaction AB

// Package desktop is the private bounded process API for the installed Mesh
// application. It never opens a listener or offers shell/arbitrary HTTP access.
package desktop

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
)

const Protocol = 1
const MaxRequest = 1 << 20
const MaxResponse = 16 << 20
const maxID = int64(9007199254740991)

type Request struct {
	Protocol int             `json:"protocol"`
	ID       int64           `json:"id"`
	Method   string          `json:"method"`
	Params   json.RawMessage `json:"params"`
}
type APIError struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}
type Response struct {
	ID       int64     `json:"id"`
	Protocol int       `json:"protocol"`
	Result   any       `json:"result,omitempty"`
	Error    *APIError `json:"error,omitempty"`
}

func fail(code, message string) *APIError { return &APIError{code, message} }

// JSON objects are duplicate-free at every depth. Unknown transport fields,
// trailing values and fractional/unsafe IDs fail before dispatch.
func strictJSON(raw []byte, out any) error {
	if len(raw) > MaxRequest {
		return errors.New("request bound")
	}
	d := json.NewDecoder(bytes.NewReader(raw))
	d.UseNumber()
	var value func(int) error
	value = func(depth int) error {
		if depth > 64 {
			return errors.New("depth")
		}
		t, err := d.Token()
		if err != nil {
			return err
		}
		if delim, ok := t.(json.Delim); ok {
			switch delim {
			case '{':
				seen := map[string]bool{}
				for d.More() {
					key, err := d.Token()
					if err != nil {
						return err
					}
					s, ok := key.(string)
					if !ok || seen[s] {
						return errors.New("duplicate key")
					}
					seen[s] = true
					if err = value(depth + 1); err != nil {
						return err
					}
				}
			case '[':
				for d.More() {
					if err = value(depth + 1); err != nil {
						return err
					}
				}
			default:
				return errors.New("unexpected delimiter")
			}
			_, err = d.Token()
			return err
		}
		return nil
	}
	if err := value(0); err != nil {
		return err
	}
	if _, err := d.Token(); err != io.EOF {
		return errors.New("trailing value")
	}
	d = json.NewDecoder(bytes.NewReader(raw))
	d.DisallowUnknownFields()
	return d.Decode(out)
}

// Serve has a single ordered dispatcher. Background sync/join is cancelable and
// status stays responsive. close drains these phases before its reply and exit.
// EOF closes the engine as well; parent death therefore releases index ownership.
func (e *Engine) Serve(ctx context.Context, in io.Reader, out io.Writer) error {
	defer e.Close()
	scanner := bufio.NewScanner(in)
	scanner.Buffer(make([]byte, 4096), MaxRequest+2)
	lines := make(chan []byte)
	done := make(chan struct{})
	stop := make(chan struct{})
	go func() {
		defer close(done)
		defer close(lines)
		for scanner.Scan() {
			raw := append([]byte(nil), scanner.Bytes()...)
			select {
			case lines <- raw:
			case <-stop:
				return
			}
		}
	}()
	defer func() {
		close(stop)
		if closer, ok := in.(io.Closer); ok {
			_ = closer.Close()
			<-done
		}
	}()
	for {
		var raw []byte
		select {
		case <-ctx.Done():
			return ctx.Err()
		case line, ok := <-lines:
			if !ok {
				if scanner.Err() != nil {
					return errors.New("desktop input unavailable or exceeds bound")
				}
				return nil
			}
			raw = line
		}
		var req Request
		if len(raw) > MaxRequest || strictJSON(raw, &req) != nil || req.Protocol != Protocol || req.ID <= 0 || req.ID > maxID || req.Method == "" || len(req.Params) == 0 || req.Params[0] != '{' {
			if err := writeResponse(out, Response{Protocol: Protocol, Error: fail("INVALID_REQUEST", "Invalid desktop request")}); err != nil {
				return err
			}
			continue
		}
		result, apiErr := e.Dispatch(ctx, req.Method, req.Params)
		if err := writeResponse(out, Response{Protocol: Protocol, ID: req.ID, Result: result, Error: apiErr}); err != nil {
			return err
		}
		if req.Method == "close" && apiErr == nil {
			return nil
		}
	}
}
func writeResponse(out io.Writer, response Response) error {
	raw, err := json.Marshal(response)
	if err != nil {
		return errors.New("desktop response unavailable")
	}
	if len(raw) > MaxResponse {
		raw, _ = json.Marshal(Response{ID: response.ID, Protocol: Protocol, Error: fail("RESPONSE_TOO_LARGE", "Response exceeds desktop limit")})
	}
	raw = append(raw, '\n')
	_, err = out.Write(raw)
	return err
}
