package transport

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"buddy/server/internal/asyncjob"
)

func mustPayload(v any) []byte { b, _ := json.Marshal(v); return b }

func TestJobHandlersRejectBadPayloadBeforeWork(t *testing.T) {
	for _, tc := range []struct {
		name        string
		handler     asyncjob.Handler
		invalidType string
	}{
		{"reply", ReplyJobHandler(nil, nil, nil, nil), `{"Turn":"invalid"}`},
		{"correction", CorrectionJobHandler(nil, nil, nil, nil, nil, nil, nil), `{"Turn":"invalid"}`},
		{"translation", TranslationJobHandler(nil, nil, nil), `{"Turn":"invalid"}`},
		{"title", TitleJobHandler(nil, nil), `{"Turn":"invalid"}`},
		{"nuance", NuanceJobHandler(nil, nil, nil), `{"LessonID":123}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			for _, payload := range []string{`{`, tc.invalidType} {
				err := tc.handler(context.Background(), asyncjob.Job{Payload: json.RawMessage(payload)})
				if err == nil || !strings.HasPrefix(err.Error(), tc.name+" job: bad payload: ") {
					t.Fatalf("handler(%q) error = %v, want labeled payload error", payload, err)
				}
				var syntaxErr *json.SyntaxError
				var typeErr *json.UnmarshalTypeError
				if !errors.As(err, &syntaxErr) && !errors.As(err, &typeErr) {
					t.Fatalf("payload error lost its JSON cause: %v", err)
				}
			}
		})
	}
}
