package push

import (
	"context"
	"sync"
)

// FakeSender records calls and answers from Codes (token -> ErrCode; absent or
// "" = accepted). Err, when set, fails the whole call.
type FakeSender struct {
	mu    sync.Mutex
	Codes map[string]string
	Err   error
	calls []FakeCall
}

// FakeCall is one recorded Send.
type FakeCall struct {
	Tokens []string
	Msg    Message
}

// Send implements Sender.
func (f *FakeSender) Send(_ context.Context, tokens []string, msg Message) ([]Result, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, FakeCall{Tokens: append([]string(nil), tokens...), Msg: msg})
	if f.Err != nil {
		return nil, f.Err
	}
	out := make([]Result, len(tokens))
	for i, t := range tokens {
		code := f.Codes[t]
		out[i] = Result{Token: t, OK: code == "", ErrCode: code}
	}
	return out, nil
}

// Calls returns a copy of the recorded calls.
func (f *FakeSender) Calls() []FakeCall {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]FakeCall(nil), f.calls...)
}
