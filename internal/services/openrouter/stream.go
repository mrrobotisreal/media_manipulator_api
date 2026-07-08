package openrouter

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
)

// SSE consumption for streamed chat completions. The wire format (per the
// OpenRouter streaming docs): records are separated by a blank line; payload
// lines are prefixed "data: "; comment lines start with ':' (e.g.
// ": OPENROUTER PROCESSING" keepalives) and must be ignored; the stream ends
// with "data: [DONE]". A mid-stream upstream failure arrives as a normal
// record whose JSON carries a top-level `error` object (HTTP status stays 200).

// errMalformedRecord flags a record whose payload is not valid JSON. Next()
// tolerates these by skipping the record (exported for tests via parse tests).
var errMalformedRecord = errors.New("openrouter: malformed sse record")

// errStreamDone is the internal [DONE] sentinel; Next() converts it to io.EOF.
var errStreamDone = errors.New("openrouter: stream done")

// parseSSERecord decodes ONE SSE record's joined data payload. It is a pure
// function so it unit-tests with fixture bytes and no network.
//
// Returns:
//   - (nil, nil)                 — nothing to process (comment-only/empty record)
//   - (nil, errStreamDone)       — the "[DONE]" terminator
//   - (nil, errMalformedRecord)  — invalid JSON (callers skip)
//   - (chunk, chunk.Error)       — an upstream mid-stream error payload
//   - (chunk, nil)               — a normal chunk
func parseSSERecord(payload []byte) (*StreamChunk, error) {
	payload = bytes.TrimSpace(payload)
	if len(payload) == 0 {
		return nil, nil
	}
	if string(payload) == "[DONE]" {
		return nil, errStreamDone
	}
	var chunk StreamChunk
	if err := json.Unmarshal(payload, &chunk); err != nil {
		return nil, errMalformedRecord
	}
	if chunk.Error != nil {
		return &chunk, chunk.Error
	}
	return &chunk, nil
}

// ChatStream wraps a live streaming completion response. Next() yields parsed
// chunks until io.EOF (clean [DONE] or connection end) or an error.
type ChatStream struct {
	resp   *http.Response
	reader *bufio.Reader
}

func newChatStream(resp *http.Response) *ChatStream {
	// Generous buffer: single SSE lines can be large (multimodal echoes,
	// verbose reasoning deltas). bufio.Reader grows reads via ReadString, but a
	// big initial buffer avoids repeated growth.
	return &ChatStream{resp: resp, reader: bufio.NewReaderSize(resp.Body, 64<<10)}
}

// readRecord reads one SSE record (up to a blank line), returning the joined
// `data:` payload. Comment (":") and non-data field lines are dropped here so
// parseSSERecord only ever sees payload bytes.
func (s *ChatStream) readRecord() ([]byte, error) {
	var data []string
	for {
		line, err := s.reader.ReadString('\n')
		if err != nil {
			if err == io.EOF && len(strings.TrimSpace(line)) == 0 && len(data) == 0 {
				return nil, io.EOF
			}
			if err != io.EOF {
				return nil, err
			}
			// EOF mid-record: fall through with whatever we have.
		}
		trimmed := strings.TrimRight(line, "\r\n")
		if trimmed == "" {
			if len(data) > 0 || err == io.EOF {
				if len(data) == 0 && err == io.EOF {
					return nil, io.EOF
				}
				return []byte(strings.Join(data, "\n")), err2eof(err)
			}
			continue // blank separator before any data — keep reading
		}
		if strings.HasPrefix(trimmed, ":") {
			continue // comment / keepalive
		}
		if rest, ok := strings.CutPrefix(trimmed, "data:"); ok {
			data = append(data, strings.TrimPrefix(rest, " "))
		}
		if err == io.EOF {
			if len(data) == 0 {
				return nil, io.EOF
			}
			return []byte(strings.Join(data, "\n")), nil
		}
	}
}

// err2eof maps a nil/io.EOF error to nil (the record itself was complete).
func err2eof(err error) error {
	if err == nil || err == io.EOF {
		return nil
	}
	return err
}

// Next returns the next parsed chunk. io.EOF signals a clean end ([DONE] or
// upstream close); an *APIError signals a mid-stream upstream failure (the
// chunk, possibly with partial choices, is returned alongside it); any other
// error is a transport failure. Malformed records are skipped.
func (s *ChatStream) Next() (*StreamChunk, error) {
	for {
		payload, err := s.readRecord()
		if err != nil {
			return nil, err
		}
		chunk, perr := parseSSERecord(payload)
		switch {
		case errors.Is(perr, errStreamDone):
			return nil, io.EOF
		case errors.Is(perr, errMalformedRecord):
			continue // tolerate and skip
		case perr != nil:
			return chunk, perr // upstream error payload (typed *APIError)
		case chunk == nil:
			continue
		default:
			return chunk, nil
		}
	}
}

// Close releases the underlying response body.
func (s *ChatStream) Close() error {
	return s.resp.Body.Close()
}
