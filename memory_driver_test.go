package astral

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"testing"
)

func memoryTestAgent(t *testing.T, config Config) (*Agent, *memoryDriver) {
	t.Helper()
	config.Model = ModelFunc{Specification: ModelSpec{ID: "memory-test", ContextWindow: 100000, MaxOutputTokens: 4096}}
	a, err := New(config)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { a.Close(context.Background()) })
	return a, a.driver.(*memoryDriver)
}

func TestMemorySteeringBytePrefixDoesNotMutateQueueOrHistory(t *testing.T) {
	a, d := memoryTestAgent(t, Config{MaxSteeringPerTurn: 16})
	for _, value := range []string{"one", "two", "three"} {
		if _, err := a.Steer(context.Background(), User(value)); err != nil {
			t.Fatal(err)
		}
	}
	request := Request{ModelID: "memory-test", MaxOutputTokens: 16384, Messages: []Message{User("one"), User("two")}}
	raw, _ := json.Marshal(request)
	selected, err := d.steeringPrefix([]int{0, 1, 2}, len(raw))
	if err != nil || !reflect.DeepEqual(selected, []int{0, 1}) || len(d.inputs) != 3 || len(d.messages) != 0 {
		t.Fatalf("wrong or destructive byte prefix: %v %v", selected, err)
	}
	if _, err := d.steeringPrefix([]int{0, 1, 2}, 1); !errors.Is(err, ErrInvalid) || len(d.inputs) != 3 {
		t.Fatalf("oversized head was removed: %v", err)
	}
}

func TestMemorySteeringOversizedBoundaryAndCancelKeepInput(t *testing.T) {
	a, d := memoryTestAgent(t, Config{SystemPrompt: strings.Repeat("x", standaloneSteeringContextBytes), MaxSteeringPerTurn: 16})
	id, err := a.Steer(context.Background(), User("keep me"))
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := d.Boundary(context.Background(), "run", true); !errors.Is(err, ErrInvalid) {
		t.Fatalf("oversized request admitted: %v", err)
	}
	cancelled, cancel := context.WithCancel(context.Background())
	cancel()
	if _, _, err := d.Boundary(cancelled, "run", true); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancel ignored: %v", err)
	}
	if len(d.inputs) != 1 || d.inputs[0].ID != id || len(d.messages) != 0 {
		t.Fatal("rejected boundary lost its input")
	}
}
