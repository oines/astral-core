package jsonl

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	astral "github.com/oines/astral-core"
	s "github.com/oines/astral-core/session"
)

func rolloutCrashFixture(t *testing.T, st *Store) s.ForkRequest {
	t.Helper()
	entries := []s.Entry{{ID: "created", SessionID: "parent", Version: 1, Kind: s.EntryCreated, Created: &s.CreationRecord{FormatVersion: s.FormatVersion, SessionID: "parent", RolloutID: "root-rollout"}}, {ID: "config", SessionID: "parent", Version: 1, Kind: s.EntryConfiguration, Configuration: &s.Configuration{ModelID: "test"}}}
	parent := "config"
	for i := 0; i < 700; i++ {
		id := fmt.Sprintf("node-%d", i)
		m := astral.User("durable ancestor")
		entries = append(entries, s.Entry{ID: id, SessionID: "parent", Version: 1, ParentID: parent, Kind: s.EntryMessage, Message: &m})
		parent = id
	}
	if _, err := st.Commit(context.Background(), "parent", s.Commit{ID: "fixture", Entries: entries}); err != nil {
		t.Fatal(err)
	}
	return s.ForkRequest{SourceID: "parent", SourceRolloutID: "root-rollout", TargetID: "child", HeadID: parent, Mode: s.ForkDetached}
}
func TestRolloutForkCrashWorker(t *testing.T) {
	path := os.Getenv("ASTRAL_ROLLOUT_CRASH_PATH")
	if path == "" {
		t.Skip("subprocess worker")
	}
	st, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	request := rolloutCrashFixture(t, st)
	_, err = st.forkWithCheckpoint(context.Background(), request, func(phase string) {
		if phase == os.Getenv("ASTRAL_ROLLOUT_CRASH_PHASE") {
			fmt.Println("ROLLOUT-BARRIER")
			select {}
		}
	})
	t.Fatalf("worker missed barrier: %v", err)
}
func TestRolloutForkSIGKILLPublicationAndRetry(t *testing.T) {
	for _, phase := range []string{"page", "before_publish", "after_publish"} {
		t.Run(phase, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "history")
			ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
			defer cancel()
			cmd := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestRolloutForkCrashWorker$", "-test.v")
			cmd.Env = append(os.Environ(), "ASTRAL_ROLLOUT_CRASH_PATH="+path, "ASTRAL_ROLLOUT_CRASH_PHASE="+phase)
			pipe, err := cmd.StdoutPipe()
			if err != nil {
				t.Fatal(err)
			}
			var stderr bytes.Buffer
			cmd.Stderr = &stderr
			if err = cmd.Start(); err != nil {
				t.Fatal(err)
			}
			scanner := bufio.NewScanner(pipe)
			reached := false
			var output bytes.Buffer
			for scanner.Scan() {
				output.WriteString(scanner.Text() + "\n")
				if scanner.Text() == "ROLLOUT-BARRIER" {
					reached = true
					break
				}
			}
			_ = cmd.Process.Kill()
			_ = cmd.Wait()
			if !reached {
				t.Fatalf("missing barrier: %s %s", output.String(), stderr.String())
			}
			st, err := Open(path)
			if err != nil {
				t.Fatal(err)
			}
			defer st.Close()
			request := s.ForkRequest{SourceID: "parent", SourceRolloutID: "root-rollout", TargetID: "child", HeadID: "node-699", Mode: s.ForkDetached}
			_, err = st.LoadState(ctx, "child")
			if phase == "after_publish" && err != nil {
				t.Fatalf("published child lost: %v", err)
			}
			if phase != "after_publish" && !errors.Is(err, s.ErrNotFound) {
				t.Fatalf("unpublished child visible: %v", err)
			}
			changed := request
			changed.HeadID = "node-1"
			if _, err = st.Fork(ctx, changed); !errors.Is(err, s.ErrConflict) {
				t.Fatalf("pending identity mismatch: %v", err)
			}
			child, err := st.Fork(ctx, request)
			if err != nil {
				t.Fatal(err)
			}
			if child.Version != 1 || child.Branch.HeadID != request.HeadID {
				t.Fatal("invalid creation state")
			}
			original, err := st.Get(ctx, request.SourceRolloutID, request.HeadID)
			if err != nil {
				t.Fatal(err)
			}
			copied, err := st.Get(ctx, child.RolloutID, request.HeadID)
			if err != nil || !reflect.DeepEqual(copied, original) {
				t.Fatal("import changed provenance")
			}
			page, err := st.Scan(ctx, child.RolloutID, s.Query{Limit: 1000})
			if err != nil || len(page.Records) != 702 {
				t.Fatalf("incomplete copy: %d %v", len(page.Records), err)
			}
			receipt, err := st.LookupCommit(ctx, "child", s.ForkCommitID)
			if err != nil || receipt.Version != 1 {
				t.Fatal("missing creation receipt")
			}
			if err = st.DiscardUnpublished(ctx, "child"); !errors.Is(err, s.ErrConflict) {
				t.Fatal("published child cleanup accepted")
			}
		})
	}
}

func TestDiscardAndRetryCleanPreIdentityFiles(t *testing.T) {
	st, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	request := rolloutCrashFixture(t, st)
	orphan := st.filename(request.RolloutID()) + ".preparing-interrupted"
	for _, retry := range []bool{false, true} {
		if err = os.WriteFile(orphan, []byte(`{"format_version":`), 0600); err != nil {
			t.Fatal(err)
		}
		if retry {
			_, err = st.Fork(context.Background(), request)
		} else {
			err = st.DiscardUnpublished(context.Background(), request.TargetID)
		}
		if err != nil {
			t.Fatal(err)
		}
		if _, err = os.Stat(orphan); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("unpublished identity file survived: %v", err)
		}
	}
}
