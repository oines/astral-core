package integration_test

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	astral "github.com/oines/astral-core"

	"github.com/oines/astral-core/store/memory"
)

var errRejectedReplay = errors.New("upstream rejected encrypted reasoning")

type replayRecoveryModel struct {
	*testModel
	recoveryCalls atomic.Int32
}

func (m *replayRecoveryModel) RecoverRequest(ctx context.Context, req astral.Request, cause error) (astral.Request, bool, error) {
	m.recoveryCalls.Add(1)
	if !errors.Is(cause, errRejectedReplay) {
		return req, false, nil
	}
	changed := false
	for i := range req.Messages {
		parts := req.Messages[i].Content[:0]
		for _, part := range req.Messages[i].Content {
			if part.Type == "reasoning" {
				changed = true
				continue
			}
			parts = append(parts, part)
		}
		req.Messages[i].Content = parts
	}
	return req, changed, nil
}
func TestEncryptedReplayRecoveryRecordedOnceWithoutReexecutingTools(t *testing.T) {
	for _, repeatFailure := range []bool{false, true} {
		t.Run(map[bool]string{false: "recover", true: "second-failure-stops"}[repeatFailure], func(t *testing.T) {
			var effects atomic.Int32
			m := &replayRecoveryModel{testModel: model(func(ctx context.Context, n int, req astral.Request, emit func(astral.ModelEvent)) (astral.Reply, error) {
				if n == 1 {
					reply := calls(call("call-1", "echo", `{"value":"saved"}`))
					reply.Message.Content = append([]astral.Content{{Type: "reasoning", Data: json.RawMessage(`{"encrypted_content":"opaque"}`)}}, reply.Message.Content...)
					return reply, nil
				}
				if n == 2 || repeatFailure {
					return astral.Reply{}, &astral.ModelError{Code: "replay_rejected", Cause: errRejectedReplay}
				}
				raw, _ := json.Marshal(req)
				if strings.Contains(string(raw), "opaque") {
					t.Error("rejected ciphertext was retried")
				}
				if !strings.Contains(string(raw), "saved") || len(req.Messages[len(req.Messages)-2].ToolCalls()) != 1 {
					t.Error("tool history was not retained")
				}
				return answer("recovered"), nil
			})}
			c := testConfig(m, memory.New())
			c.Tools = []astral.Tool{echoTool(&effects)}
			c.Retry.MaxAttempts = 2
			c.Retry.InitialBackoff = time.Millisecond
			c.Retry.MaxBackoff = time.Millisecond
			a := newSessions(t, c)
			s := newSession(t, a, astral.SessionOptions{})
			run := start(t, s, "go")
			_, err := wait(t, run)
			if repeatFailure && err == nil || !repeatFailure && err != nil {
				t.Fatalf("run error: %v", err)
			}
			if len(m.observed()) != 3 || effects.Load() != 1 || m.recoveryCalls.Load() != 1 {
				t.Fatalf("requests=%d tools=%d recovery=%d", len(m.observed()), effects.Load(), m.recoveryCalls.Load())
			}
			var requests []astral.Request
			originalKept := false
			for _, entry := range entries(s) {
				if entry.ModelRequest != nil {
					requests = append(requests, entry.ModelRequest.Request)
				}
				if entry.Message != nil {
					raw, _ := json.Marshal(entry.Message)
					if strings.Contains(string(raw), "opaque") {
						originalKept = true
					}
				}
			}
			if len(requests) != 3 || !originalKept {
				t.Fatal("audit history was changed or attempts were not recorded")
			}
			second, _ := json.Marshal(requests[1])
			third, _ := json.Marshal(requests[2])
			if !strings.Contains(string(second), "opaque") || strings.Contains(string(third), "opaque") {
				t.Fatal("request records do not describe actual attempts")
			}
		})
	}
}

func TestReplayRecoveryDefersLateContextUntilNextBoundary(t *testing.T) {
	var s *astral.Session
	fresh := snapshotMessage("changed while the provider rejected replay")
	m := &replayRecoveryModel{testModel: model(func(ctx context.Context, n int, req astral.Request, _ func(astral.ModelEvent)) (astral.Reply, error) {
		switch n {
		case 1:
			reply := calls(call("c", "echo", `{"value":"saved"}`))
			reply.Message.Content = append([]astral.Content{{Type: "reasoning", Data: json.RawMessage(`{"encrypted_content":"opaque"}`)}}, reply.Message.Content...)
			return reply, nil
		case 2:
			if _, err := inject(s, ctx, []astral.Message{fresh}, injectOptions{}); err != nil {
				return astral.Reply{}, err
			}
			return astral.Reply{}, &astral.ModelError{Code: "replay_rejected", Cause: errRejectedReplay}
		default:
			raw, _ := json.Marshal(req)
			if hasMessage(req.Messages, fresh) || strings.Contains(string(raw), "opaque") {
				return astral.Reply{}, errors.New("recovery replaced the fresh request with an old snapshot")
			}
			return answer("done"), nil
		}
	})}
	c := testConfig(m, memory.New())
	c.Tools = []astral.Tool{echoTool(nil)}
	c.Retry.MaxAttempts = 2
	c.Retry.InitialBackoff = time.Millisecond
	c.Retry.MaxBackoff = time.Millisecond
	s = newSession(t, newSessions(t, c), astral.SessionOptions{})
	waitOK(t, start(t, s, "go"))
	view, err := s.InspectContext(context.Background())
	if err != nil || !hasMessage(view.Request.Messages, fresh) {
		t.Fatal("late context was not flushed after the frozen retry")
	}
	if len(m.observed()) != 3 {
		t.Fatal("recovery did not make exactly one additional model attempt")
	}
}
