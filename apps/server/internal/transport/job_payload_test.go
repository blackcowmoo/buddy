package transport

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"buddy/server/internal/asyncjob"
)

func TestLiveJobHandlersRejectBadPayloadBeforeWork(t *testing.T) {
	for _, tc := range []struct {
		name    string
		handler asyncjob.Handler
	}{
		{"reply", ReplyJobHandler(nil, nil, nil, nil)},
		{"correction", CorrectionJobHandler(nil, nil, nil, nil, nil, nil, nil)},
		{"translation", TranslationJobHandler(nil, nil, nil)},
		{"title", TitleJobHandler(nil, nil)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			for _, payload := range []string{`{`, `{"Turn":"invalid"}`} {
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
