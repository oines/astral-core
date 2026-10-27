package client

import (
	"encoding/json"
	"strings"
	"testing"

	core "github.com/oines/astral-core"
	models "github.com/oines/astral-core/models"
)

func TestContentEstimateIgnoresImageBytesAndCountsTextAndTools(t *testing.T) {
	c := &Client{}
	empty, err := c.Estimate(core.Request{})
	if err != nil {
		t.Fatal(err)
	}
	request := core.Request{Messages: []core.Message{core.User(strings.Repeat("a", 400))}}
	text, err := c.Estimate(request)
	if err != nil || text-empty != 106 {
		t.Fatalf("text=%d empty=%d err=%v", text, empty, err)
	}
	request.Messages[0].Content = core.Text(strings.Repeat("中", 100))
	chinese, _ := c.Estimate(request)
	if chinese != text {
		t.Fatalf("Chinese fallback underestimated: got %d want %d", chinese, text)
	}
	request.Messages[0].Content = []core.Content{{Type: "image", URL: "data:image/png;base64," + strings.Repeat("A", 100000)}}
	large, _ := c.Estimate(request)
	request.Messages[0].Content[0].URL = "https://example.com/a.png"
	small, _ := c.Estimate(request)
	if large != small || small-empty != 1206 {
		t.Fatalf("image encoding changed estimate: %d %d", large, small)
	}
	c.Config = models.Config{ImageTokenBudget: 3000}
	custom, _ := c.Estimate(request)
	if custom-small != 1800 {
		t.Fatalf("image override=%d", custom)
	}
	request.SystemPrompt = strings.Repeat("s", 400)
	request.Tools = []core.ToolDefinition{{Name: "search", Description: "Find text", Parameters: json.RawMessage(`{"type":"object","properties":{"q":{"type":"string"}}}`)}}
	withSchema, _ := c.Estimate(request)
	if withSchema <= custom+100 {
		t.Fatal("system/tools omitted from full estimate")
	}
	request.Messages[0].Content = []core.Content{{Type: "file"}}
	if _, err := c.Estimate(request); err == nil {
		t.Fatal("file silently ignored")
	}
}

func TestContentEstimateUsesSameIncrementForTailAndFullRequest(t *testing.T) {
	c := &Client{}
	base := core.Request{SystemPrompt: "rules", Messages: []core.Message{core.User("history")}}
	before, _ := c.Estimate(base)
	tail := core.Message{Role: core.RoleTool, ToolCallID: "id", Name: "echo", Content: core.Text("工具返回内容")}
	base.Messages = append(base.Messages, tail)
	after, _ := c.Estimate(base)
	tailCount, _ := c.Estimate(core.Request{Messages: []core.Message{tail}})
	empty, _ := c.Estimate(core.Request{})
	if after-before != tailCount-empty {
		t.Fatalf("inconsistent full/tail estimates: %d vs %d", after-before, tailCount-empty)
	}
}
