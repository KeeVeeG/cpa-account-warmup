package main

import (
	"errors"
	"strings"
	"testing"
)

func TestHostErrorRetainsStatusWithoutSensitiveMessage(t *testing.T) {
	err := decodeHostResult([]byte(`{"ok":false,"error":{"http_status":429,"message":"secret-upstream-token"}}`), 1, &struct{}{})
	var status interface{ StatusCode() int }
	if !errors.As(err, &status) || status.StatusCode() != 429 {
		t.Fatalf("HTTP status was lost: %v", err)
	}
	if strings.Contains(err.Error(), "secret") {
		t.Fatal("Host error leaked raw upstream data")
	}
}

func TestHostResultRequiresSuccessfulEnvelopeAndNativeReturn(t *testing.T) {
	for _, test := range []struct {
		body string
		code int
	}{
		{`not JSON`, 0}, {`{"ok":false}`, 0}, {`{"ok":true,"result":{}}`, 1}, {`{"ok":true,"result":"wrong type"}`, 0},
	} {
		if err := decodeHostResult([]byte(test.body), test.code, &struct{}{}); err == nil {
			t.Errorf("Accepted invalid host result: %+v", test)
		}
	}
	var result struct {
		Value string `json:"value"`
	}
	if err := decodeHostResult([]byte(`{"ok":true,"result":{"value":"expected"}}`), 0, &result); err != nil || result.Value != "expected" {
		t.Fatalf("Valid result failed: %+v, %v", result, err)
	}
}
