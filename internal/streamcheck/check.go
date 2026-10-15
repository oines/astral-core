// Package streamcheck validates block references without assembling a message
// or retaining deltas. Each Check belongs to a single model attempt.
package streamcheck

import (
	"fmt"
	"strings"
)

type observation struct {
	kind, callID string
}

type Check struct {
	blocks map[int]observation
	err    error
}

func (c *Check) Err() error { return c.err }

func (c *Check) Fail(err error) error {
	if c.err == nil {
		c.err = err
	}
	return c.err
}

func (c *Check) Observe(eventType string, index *int, callID string) error {
	if c.err != nil {
		return c.err
	}
	if strings.TrimSpace(eventType) == "" {
		return c.Fail(fmt.Errorf("model event without type"))
	}
	kind := ""
	switch eventType {
	case "text_delta":
		kind = "text"
	case "reasoning_delta":
		kind = "reasoning"
	case "tool_call_delta":
		kind = "tool_call"
	}
	if index == nil {
		if kind != "" {
			return c.Fail(fmt.Errorf("%s requires content_index", eventType))
		}
		return nil
	}
	if *index < 0 {
		return c.Fail(fmt.Errorf("negative content_index"))
	}
	previous := c.blocks[*index]
	if kind != "" {
		if previous.kind != "" && previous.kind != kind {
			return c.Fail(fmt.Errorf("content_index %d changed block type", *index))
		}
		previous.kind = kind
	}
	if kind == "tool_call" && callID != "" {
		if previous.callID != "" && previous.callID != callID {
			return c.Fail(fmt.Errorf("content_index %d changed tool call id", *index))
		}
		previous.callID = callID
	}
	if c.blocks == nil {
		c.blocks = map[int]observation{}
	}
	c.blocks[*index] = previous
	return nil
}

func (c *Check) Finish(count int, block func(int) (kind, callID string)) error {
	if c.err != nil {
		return c.err
	}
	for index, seen := range c.blocks {
		if index >= count {
			return c.Fail(fmt.Errorf("content_index %d outside final content", index))
		}
		kind, callID := block(index)
		if seen.kind != "" && seen.kind != kind {
			return c.Fail(fmt.Errorf("content_index %d does not match final block type", index))
		}
		if seen.callID != "" && seen.callID != callID {
			return c.Fail(fmt.Errorf("content_index %d does not match final tool call id", index))
		}
	}
	return nil
}
