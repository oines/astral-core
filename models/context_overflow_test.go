package models_test

import (
	"errors"
	"testing"

	core "github.com/oines/astral-core"
	models "github.com/oines/astral-core/models"
)

func TestContextOverflowClassification(t *testing.T) {
	for _, tc := range []struct {
		name string
		api  *models.APIError
		want bool
	}{
		{"openai", &models.APIError{StatusCode: 400, Code: "context_length_exceeded"}, true},
		{"responses", &models.APIError{StatusCode: 400, Message: "Your input exceeds the context window of this model"}, true},
		{"qwen", &models.APIError{StatusCode: 400, Message: "Range of input length should be [1, 32768]"}, true},
		{"unclassified-throttling", &models.APIError{Message: "Throttling error: prompt too long for rate limit"}, false},
		{"anthropic", &models.APIError{StatusCode: 400, Message: "prompt is too long: 210000 tokens > 200000 maximum"}, true},
		{"google", &models.APIError{StatusCode: 400, Message: "The input token count (1200) exceeds the maximum number of tokens allowed (1000)."}, true},
		{"stream", &models.APIError{Code: "context_window_exceeded"}, true},
		{"auth", &models.APIError{StatusCode: 401, Message: "maximum context length"}, false},
		{"rate", &models.APIError{StatusCode: 429, Kind: models.KindRateLimited, Message: "too many tokens"}, false},
		{"quota", &models.APIError{StatusCode: 400, Kind: models.KindQuotaExhausted, Message: "input is too long"}, false},
		{"payload", &models.APIError{StatusCode: 413, Message: "request body too large"}, false},
		{"invalid", &models.APIError{StatusCode: 400, Message: "invalid tool schema"}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := models.IsContextOverflow(tc.api); got != tc.want {
				t.Fatalf("classification=%v want=%v", got, tc.want)
			}
			var classified *core.ModelError
			if !errors.As(models.ModelError(tc.api), &classified) || classified.ContextOverflow != tc.want {
				t.Fatalf("missing Core classification: %+v", classified)
			}
			if tc.want && classified.Transient {
				t.Fatal("overflow must not retry the same HTTP request")
			}
		})
	}
	if models.IsContextOverflow(errors.New("maximum context length")) {
		t.Fatal("local error misclassified")
	}
}
