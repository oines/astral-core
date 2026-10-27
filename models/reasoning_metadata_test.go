package models_test

import (
	"encoding/json"
	"reflect"
	"testing"

	"github.com/oines/astral-core/models"
)

func TestExplicitReasoningMetadataPreservesAPIBindingsAndPresence(t *testing.T) {
	for _, container := range []string{"reasoning", "reasoning_capabilities"} {
		payload := `{"` + container + `":{"supported":true,"toggle":true,"disable":false,"default_on":null,"efforts":[],"default_effort":"vendor-normal","on_effort":"vendor-on","off_effort":"vendor-off","budget":null,"dynamic_budget":false,"min_budget":null,"max_budget":8192}}`
		got, err := models.ParseReasoningMetadata("compatible", json.RawMessage(payload))
		if err != nil {
			t.Fatal(err)
		}
		var record map[string]map[string]json.RawMessage
		_ = json.Unmarshal([]byte(payload), &record)
		if !reflect.DeepEqual(got, record[container]) {
			t.Fatalf("explicit fields lost: %s: %v", container, got)
		}
	}
}

func TestExplicitReasoningMetadataMandatoryAndInvalidTypes(t *testing.T) {
	got, err := models.ParseReasoningMetadata("compatible", json.RawMessage(`{"reasoning":{"supported":true,"mandatory":true,"efforts":["minimal","high"],"dynamic_budget":null}}`))
	if err != nil || string(got["toggle"]) != "false" || string(got["disable"]) != "false" || string(got["default_on"]) != "true" || string(got["dynamic_budget"]) != "null" {
		t.Fatalf("mandatory not preserved: %v %v", got, err)
	}
	for _, data := range []string{`{"toggle":"yes"}`, `{"efforts":[""]}`, `{"efforts":[" high"]}`, `{"budget":12}`, `{"dynamic_budget":[]}`, `{"on_effort":false}`, `{"mandatory":"always"}`} {
		if _, err := models.ParseReasoningMetadata("compatible", json.RawMessage(`{"reasoning":`+data+`}`)); err == nil {
			t.Fatalf("invalid metadata accepted: %s", data)
		}
	}
}
