package connector

import (
	"encoding/json"
	"strings"
	"testing"
	"time"
)

// The document is the wire form a consumer decodes, so it is asserted as bytes
// rather than as a struct: field order, the omission of empty optionals and the
// absence of HTML escaping are all part of matching the profile's canonical
// encoding, and a struct comparison would not see any of them.
func TestAPIKeyV2ValueMatchesTheProfileDocument(t *testing.T) {
	value, err := apiKeyV2Value(
		"glsa_one_time_value",
		"41",
		"https://acme.grafana.net",
		time.Date(2026, 10, 8, 13, 45, 0, 0, time.UTC),
	)
	if err != nil {
		t.Fatalf("apiKeyV2Value: %v", err)
	}

	want := `{"key_value":"glsa_one_time_value","provider":"grafana",` +
		`"base_url":"https://acme.grafana.net","key_id":"41",` +
		`"header_name":"Authorization","expires_at":"2026-10-08"}`
	if got := string(value); got != want {
		t.Fatalf("unexpected document\n got: %s\nwant: %s", got, want)
	}
}

// The profile's `expires_at` is a date, so the instant the provider reported is
// truncated to its UTC day rather than written as a timestamp. The exact instant
// stays on the resource's SecretTrait.
func TestAPIKeyV2ValueWritesTheExpiryAsAUTCDate(t *testing.T) {
	// 02:30 on the 8th at +05:00 is 21:30 on the 7th UTC, so the day the
	// document carries is the one the provider's instant falls in, not the one
	// the local wall clock reads.
	value, err := apiKeyV2Value(
		"glsa_one_time_value",
		"41",
		"https://acme.grafana.net",
		time.Date(2026, 10, 8, 2, 30, 0, 0, time.FixedZone("plus-five", 5*60*60)),
	)
	if err != nil {
		t.Fatalf("apiKeyV2Value: %v", err)
	}
	if !strings.Contains(string(value), `"expires_at":"2026-10-07"`) {
		t.Fatalf("expected the UTC day 2026-10-07, got %s", value)
	}
}

// A consumer's decoder refuses a document it cannot map, so the emitted keys
// must be exactly the profile's own: an unknown key, a duplicate key or a
// non-string value would make the whole credential undecodable rather than
// leaving the extra field ignored.
func TestAPIKeyV2ValueEmitsOnlyDeclaredStringFields(t *testing.T) {
	value, err := apiKeyV2Value(
		"glsa_one_time_value",
		"41",
		"https://acme.grafana.net",
		time.Date(2026, 10, 8, 13, 45, 0, 0, time.UTC),
	)
	if err != nil {
		t.Fatalf("apiKeyV2Value: %v", err)
	}

	decoded := map[string]any{}
	if err := json.Unmarshal(value, &decoded); err != nil {
		t.Fatalf("the document must be a JSON object: %v", err)
	}

	declared := map[string]bool{
		"key_value":   true,
		"provider":    true,
		"base_url":    true,
		"key_id":      true,
		"header_name": true,
		"expires_at":  true,
	}
	for key, raw := range decoded {
		if !declared[key] {
			t.Errorf("the document carries an undeclared field %q", key)
		}
		if _, isString := raw.(string); !isString {
			t.Errorf("field %q must be a string, got %T", key, raw)
		}
	}
	// key_value is the profile's one required field: without it the value is
	// not an API key at all.
	if _, present := decoded["key_value"]; !present {
		t.Error("the document must carry the required key_value field")
	}
	// A Grafana service-account token inherits its service account's
	// permissions and cannot be scoped, so scopes is never emitted.
	if _, present := decoded["scopes"]; present {
		t.Error("scopes must never be emitted for a Grafana token")
	}
	if len(decoded["expires_at"].(string)) != len("2026-10-08") {
		t.Errorf("expires_at must be an RFC 3339 full-date, got %q", decoded["expires_at"])
	}
}

// An optional field with no value is omitted rather than written as an empty
// string, which is how the profile's own encoder renders it. `expires_at` is
// asserted alongside it because a zero time must not become "0001-01-01".
func TestAPIKeyV2ValueOmitsEmptyOptionals(t *testing.T) {
	value, err := apiKeyV2Value("glsa_one_time_value", "", "", time.Time{})
	if err != nil {
		t.Fatalf("apiKeyV2Value: %v", err)
	}

	decoded := map[string]any{}
	if err := json.Unmarshal(value, &decoded); err != nil {
		t.Fatalf("the document must be a JSON object: %v", err)
	}
	for _, key := range []string{"base_url", "key_id"} {
		if _, present := decoded[key]; present {
			t.Errorf("an empty %s must be omitted, got %s", key, value)
		}
	}
	if got := decoded["key_value"]; got != "glsa_one_time_value" {
		t.Fatalf("unexpected key_value %v", got)
	}
	if got := decoded["expires_at"]; got != "0001-01-01" {
		t.Fatalf("a zero expiry is still written as its own date, got %v", got)
	}
}

// The token and the instance URL are written verbatim: an Encoder with HTML
// escaping on would spell `&` as \u0026, which is a different byte string from
// the one the profile's encoder produces for the same input.
func TestAPIKeyV2ValueDoesNotHTMLEscape(t *testing.T) {
	value, err := apiKeyV2Value(
		"glsa_one_time_value",
		"41",
		"https://grafana.example/?org=acme&env=prod",
		time.Date(2026, 10, 8, 13, 45, 0, 0, time.UTC),
	)
	if err != nil {
		t.Fatalf("apiKeyV2Value: %v", err)
	}
	if !strings.Contains(string(value), "org=acme&env=prod") {
		t.Fatalf("the base URL must be written verbatim, got %s", value)
	}
	if strings.Contains(string(value), `\u0026`) {
		t.Fatalf("the base URL must not be HTML-escaped, got %s", value)
	}
}
