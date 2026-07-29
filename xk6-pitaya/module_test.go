package pitaya

import (
	"strings"
	"testing"
)

func TestNewOptionsFromAcceptsDocumentedFields(t *testing.T) {
	opts, err := newOptionsFrom(map[string]interface{}{
		"handshakeData":    map[string]interface{}{},
		"requestTimeoutMs": 250,
	})
	if err != nil {
		t.Fatalf("newOptionsFrom() error = %v", err)
	}
	if opts.HandshakeData == nil {
		t.Fatal("newOptionsFrom() handshake data is nil")
	}
	if opts.RequestTimeoutMs != 250 {
		t.Fatalf("newOptionsFrom() request timeout = %d, want 250", opts.RequestTimeoutMs)
	}
}

func TestNewOptionsFromRejectsUnknownFields(t *testing.T) {
	for _, field := range []string{"logLevel", "serializer"} {
		t.Run(field, func(t *testing.T) {
			_, err := newOptionsFrom(map[string]interface{}{field: "value"})
			if err == nil {
				t.Fatalf("newOptionsFrom() accepted unknown field %q", field)
			}
			if !strings.Contains(err.Error(), `unknown field "`+field+`"`) {
				t.Fatalf("newOptionsFrom() error = %q, want unknown field %q", err, field)
			}
		})
	}
}
