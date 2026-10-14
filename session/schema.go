package session

import (
	"encoding/json"
	"fmt"

	"github.com/santhosh-tekuri/jsonschema/v6"
)

type embeddedSchemasOnly struct{}

func (embeddedSchemasOnly) Load(url string) (any, error) {
	return nil, fmt.Errorf("external schema reference %q is disabled; embed definitions in the declaration", url)
}
func compileSchema(raw json.RawMessage) (argumentValidator, error) {
	document, err := decodeJSON(raw)
	if err != nil {
		return nil, err
	}
	c := jsonschema.NewCompiler()
	c.DefaultDraft(jsonschema.Draft2020)
	c.UseLoader(embeddedSchemasOnly{})
	const location = "https://astral.invalid/tool-schema.json"
	if err = c.AddResource(location, document); err != nil {
		return nil, err
	}
	return c.Compile(location)
}

type argumentValidator interface{ Validate(any) error }
