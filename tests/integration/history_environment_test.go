package integration_test

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	astral "github.com/oines/astral-core"
	environments "github.com/oines/astral-core/environment"
	sessions "github.com/oines/astral-core/session"
	"github.com/oines/astral-core/standardtools"
	"github.com/oines/astral-core/store/memory"
)

func TestBranchesSelectExistingLeavesAndPersistHead(t *testing.T) {
	store := memory.New()
	m := model(func(_ context.Context, _ int, r astral.Request, _ func(astral.ModelEvent)) (astral.Reply, error) {
		users := texts(r.Messages, astral.RoleUser)
		return answer("reply to " + users[len(users)-1]), nil
	})
	c := testConfig(m, store)
	c.SystemPrompt = "first config"
	a := newAgent(t, c)
	s := newSession(t, a, sessions.SessionOptions{ID: "tree"})
	first := waitOK(t, start(t, s, "one"))
	configuration := s.Configuration()
	configuration.SystemPrompt = "second config"
	if err := s.Configure(context.Background(), configuration); err != nil {
		t.Fatal(err)
	}
	second := waitOK(t, start(t, s, "two"))
	originalCount := len(entries(s))
	if err := s.SelectHead(context.Background(), first.LastMessageID); err != nil {
		t.Fatal(err)
	}
	if s.Configuration().SystemPrompt != "first config" {
		t.Fatal("future configuration leaked")
	}
	third := waitOK(t, start(t, s, "three"))
	if len(children(s, first.LastMessageID)) != 2 {
		t.Fatal("sibling branch was overwritten")
	}
	if err := s.SelectHead(context.Background(), second.LastMessageID); err != nil {
		t.Fatal(err)
	}
	view, err := s.InspectContext(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if users := texts(view.Request.Messages, astral.RoleUser); !reflect.DeepEqual(users, []string{"one", "two"}) {
		t.Fatalf("wrong leaf path: %v", users)
	}
	if len(entries(s)) <= originalCount {
		t.Fatal("history removed")
	}
	if err = a.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
	opened, err := newAgent(t, c).OpenSession(context.Background(), s.ID())
	if err != nil {
		t.Fatal(err)
	}
	if opened.Head() != second.LastMessageID {
		t.Fatal("head selection was not durable")
	}
	if _, ok := entry(opened, third.LastMessageID); !ok {
		t.Fatal("sibling entry missing")
	}
	if len(entries(opened)) == 0 {
		t.Fatal("missing tree")
	}
}

func TestForkBeforeUserCopiesPathButNoOperationalState(t *testing.T) {
	m := model(func(context.Context, int, astral.Request, func(astral.ModelEvent)) (astral.Reply, error) {
		return answer("answer"), nil
	})
	s := newSession(t, newAgent(t, testConfig(m, memory.New())), sessions.SessionOptions{})
	waitOK(t, start(t, s, "one"))
	waitOK(t, start(t, s, "two"))
	var target string
	for _, entry := range entries(s) {
		if entry.Message != nil && entry.Message.Role == astral.RoleUser && entry.Message.Content[0].Text == "two" {
			target = entry.ID
		}
	}
	if _, err := s.FollowUp(context.Background(), astral.User("source-only queue")); err != nil {
		t.Fatal(err)
	}
	fork, err := s.Fork(context.Background(), target, sessions.ForkBefore, sessions.SessionOptions{ID: "fork"})
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries(fork) {
		if !entry.IsHistory() && entry.Kind != sessions.EntryCreated {
			t.Fatalf("operational state copied: %s", entry.Kind)
		}
	}
	view, err := fork.InspectContext(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if users := texts(view.Request.Messages, astral.RoleUser); !reflect.DeepEqual(users, []string{"one"}) {
		t.Fatalf("before semantics: %v", users)
	}
	waitOK(t, start(t, fork, "fork-only"))
	if len(m.observed()) != 3 {
		t.Fatal("source queue copied to fork")
	}
	if _, err = s.Fork(context.Background(), fork.Head(), sessions.ForkBefore, sessions.SessionOptions{}); err == nil {
		t.Fatal("cross-session entry accepted")
	}
}

func TestHistoricalPartialToolBatchCannotExecute(t *testing.T) {
	var effects atomic.Int32
	m := model(func(_ context.Context, n int, _ astral.Request, _ func(astral.ModelEvent)) (astral.Reply, error) {
		if n == 1 {
			return calls(call("c", "echo", `{"value":"x"}`)), nil
		}
		return answer("done"), nil
	})
	c := testConfig(m, memory.New())
	c.Tools = []astral.Tool{echoTool(&effects)}
	s := newSession(t, newAgent(t, c), sessions.SessionOptions{})
	waitOK(t, start(t, s, "go"))
	var boundary string
	for _, entry := range entries(s) {
		if entry.Message != nil && len(entry.Message.ToolCalls) > 0 {
			boundary = entry.ID
		}
	}
	fork, err := s.Fork(context.Background(), boundary, sessions.ForkAt, sessions.SessionOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = fork.Continue(context.Background()); !errors.Is(err, astral.ErrIncompleteBatch) {
		t.Fatalf("partial fork executed: %v", err)
	}
	if err = s.SelectHead(context.Background(), boundary); err != nil {
		t.Fatal(err)
	}
	if _, err = s.Continue(context.Background()); !errors.Is(err, astral.ErrIncompleteBatch) {
		t.Fatalf("partial branch executed: %v", err)
	}
	if effects.Load() != 1 {
		t.Fatal("history replayed effects")
	}
}

func TestEnvironmentSnapshotsRemainInPositionAndHostMutationsAreIsolated(t *testing.T) {
	m := model(func(context.Context, int, astral.Request, func(astral.ModelEvent)) (astral.Reply, error) {
		return answer("done"), nil
	})
	c := testConfig(m, memory.New())
	c.SystemPrompt = "stable harness rules"
	snapshot := environments.Snapshot{RuntimeID: "local", WorkspaceID: "one", CWD: "/one", Skills: []environments.Resource{{Name: "original"}}, Metadata: json.RawMessage(`{"version":1}`)}
	s := newSession(t, newAgent(t, c), sessions.SessionOptions{Environment: &snapshot})
	snapshot.Skills[0].Name = "changed"
	snapshot.Metadata[11] = '9'
	waitOK(t, start(t, s, "first"))
	if err := s.RefreshEnvironment(context.Background(), environments.Snapshot{RuntimeID: "remote", WorkspaceID: "two", CWD: "/two"}); err != nil {
		t.Fatal(err)
	}
	waitOK(t, start(t, s, "second"))
	requests := m.observed()
	if !reflect.DeepEqual(requests[0].Messages, requests[1].Messages[:len(requests[0].Messages)]) {
		t.Fatal("ordinary request prefix rewritten")
	}
	if requests[0].SystemPrompt != requests[1].SystemPrompt {
		t.Fatal("environment mutated system prompt")
	}
	var snapshots []environments.Snapshot
	for _, message := range requests[1].Messages {
		if message.ContextKind == "environment" {
			var e environments.Snapshot
			if err := json.Unmarshal([]byte(message.Content[0].Text), &e); err != nil {
				t.Fatal(err)
			}
			snapshots = append(snapshots, e)
		}
	}
	if len(snapshots) != 2 || snapshots[0].Skills[0].Name != "original" || snapshots[1].WorkspaceID != "two" {
		t.Fatalf("snapshot history: %+v", snapshots)
	}
}

func TestEnvironmentSwitchIsBatchBarrierAndBindsEachCall(t *testing.T) {
	var mu sync.Mutex
	var bindings []environments.Binding
	inspect := tool("inspect", func(_ context.Context, inv astral.Invocation, _ func(astral.ToolProgress)) (astral.ToolResult, error) {
		mu.Lock()
		bindings = append(bindings, bindingOf(inv.Binding))
		mu.Unlock()
		return astral.ToolResult{Content: astral.Text(bindingOf(inv.Binding).RuntimeID + "/" + bindingOf(inv.Binding).WorkspaceID)}, nil
	})
	switcher := standardtools.SwitchEnvironment(func(_ context.Context, inv astral.Invocation, selection standardtools.EnvironmentSelection) (environments.Snapshot, error) {
		if bindingOf(inv.Binding).RuntimeID != "local" {
			return environments.Snapshot{}, errors.New("switch started in wrong environment")
		}
		return environments.Snapshot{RuntimeID: selection.Runtime, WorkspaceID: selection.Workspace, CWD: "/remote/project"}, nil
	})
	m := model(func(_ context.Context, n int, r astral.Request, _ func(astral.ModelEvent)) (astral.Reply, error) {
		if n == 1 {
			return calls(call("before", "inspect", `{"value":"x"}`), call("switch", "switch_environment", `{"runtime":"ssh","workspace":"project-b"}`), call("after", "inspect", `{"value":"y"}`)), nil
		}
		results := texts(r.Messages, astral.RoleTool)
		if len(results) != 3 || results[0] != "local/project-a" || results[2] != "ssh/project-b" {
			return astral.Reply{}, errors.New("bad switch ordering")
		}
		return answer("done"), nil
	})
	c := testConfig(m, memory.New())
	c.Tools = []astral.Tool{inspect, switcher}
	s := newSession(t, newAgent(t, c), sessions.SessionOptions{Environment: &environments.Snapshot{RuntimeID: "local", WorkspaceID: "project-a"}})
	waitOK(t, start(t, s, "go"))
	if len(bindings) != 2 || bindings[0].SnapshotID == bindings[1].SnapshotID {
		t.Fatal("dispatch bindings not pinned")
	}
	view, err := s.InspectContext(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if view.Environment.RuntimeID != "ssh" {
		t.Fatal("new environment missing")
	}
}

func TestReopenAndBranchRequireEnvironmentRediscovery(t *testing.T) {
	store := memory.New()
	m := model(func(context.Context, int, astral.Request, func(astral.ModelEvent)) (astral.Reply, error) {
		return answer("done"), nil
	})
	c := testConfig(m, store)
	a := newAgent(t, c)
	s := newSession(t, a, sessions.SessionOptions{Environment: &environments.Snapshot{RuntimeID: "old", WorkspaceID: "project"}})
	first := waitOK(t, start(t, s, "one"))
	if err := a.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
	a2 := newAgent(t, c)
	opened, err := a2.OpenSession(context.Background(), s.ID())
	if err != nil {
		t.Fatal(err)
	}
	if _, err = opened.Start(context.Background(), astral.User("two")); !errors.Is(err, sessions.ErrEnvironmentRefresh) {
		t.Fatalf("stale environment accepted: %v", err)
	}
	if err = opened.RefreshEnvironment(context.Background(), environments.Snapshot{RuntimeID: "fresh", WorkspaceID: "project"}); err != nil {
		t.Fatal(err)
	}
	waitOK(t, start(t, opened, "two"))
	if err = opened.SelectHead(context.Background(), first.LastMessageID); err != nil {
		t.Fatal(err)
	}
	if _, err = opened.Continue(context.Background()); !errors.Is(err, sessions.ErrEnvironmentRefresh) {
		t.Fatalf("branch trusted historical environment: %v", err)
	}
	if err = a2.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
	var refreshes atomic.Int32
	c.Environment = func(_ context.Context, r environments.Request) (environments.Snapshot, error) {
		refreshes.Add(1)
		return environments.Snapshot{RuntimeID: "host-confirmed", WorkspaceID: "project"}, nil
	}
	opened, err = newAgent(t, c).OpenSession(context.Background(), s.ID())
	if err != nil {
		t.Fatal(err)
	}
	waitOK(t, start(t, opened, "fresh input"))
	if refreshes.Load() != 1 {
		t.Fatal("host rediscovery missing")
	}
	request := m.observed()[len(m.observed())-1]
	data, _ := json.Marshal(request)
	if !strings.Contains(string(data), "host-confirmed") {
		t.Fatal("fresh environment not model visible")
	}
}
