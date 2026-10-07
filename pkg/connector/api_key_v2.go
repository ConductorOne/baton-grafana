package connector

import (
	"bytes"
	"encoding/json"
	"fmt"
	"time"
)

// apiKeyV2ContentType is the native secret profile a vended Grafana
// service-account token is typed as.
//
// It is the `content_type` of the `api_key_v2()` definition in the shipped
// Latchkey client catalog (ductone/multipass,
// crates/latchkey-client-sdk/src/secret_types/definitions.rs): value codec
// `JsonV1`, one required field `key_value`, and optional `provider`, `base_url`,
// `scopes`, `key_id`, `header_name` and `expires_at`. The identifier is frozen
// once a secret of that type exists, so it is spelled once, here.
const apiKeyV2ContentType = "api_key_v2" //nolint:gosec // Not a credential: the profile's content-type identifier.

// apiKeyV2Provider is the `provider` field for a Grafana credential.
const apiKeyV2Provider = "grafana"

// apiKeyV2HeaderName is the HTTP header a Grafana service-account token is
// presented in: `Authorization: Bearer <token>`. Reporting it is what makes the
// document usable without out-of-band knowledge of the vendor's convention.
const apiKeyV2HeaderName = "Authorization"

// apiKeyV2Document is the `JsonV1` document for an `api_key_v2` value.
//
// The fields are declared in the profile's own order (key_value, provider,
// base_url, scopes, key_id, header_name, expires_at) with the empty optionals
// omitted, which is the canonical encoding the client's `encode_json_v1`
// produces. Order matters for byte-for-byte comparison against that encoder,
// not for decoding: `decode_json_v1` parses an ordered object.
//
// `scopes` is deliberately absent. A Grafana service-account token inherits its
// service account's permissions and cannot be scoped — this connector rejects a
// scoped request before minting — so it has no scopes to report and never emits
// the field.
//
// Every name must be spelled exactly as the profile declares it. The decoder
// refuses an unknown key, a duplicate key and a null value, so a stray or
// renamed field makes the whole value undecodable rather than being ignored.
type apiKeyV2Document struct {
	KeyValue   string `json:"key_value"`
	Provider   string `json:"provider,omitempty"`
	BaseURL    string `json:"base_url,omitempty"`
	KeyID      string `json:"key_id,omitempty"`
	HeaderName string `json:"header_name,omitempty"`
	ExpiresAt  string `json:"expires_at,omitempty"`
}

// apiKeyV2Value encodes the `api_key_v2` document for a freshly issued Grafana
// service-account token.
//
// keyValue is the provider's one-time token value, keyID is the provider's own
// id for the token, baseURL is the Grafana instance the token is used against,
// and expiresAt is the expiry the provider reported for it.
//
// The returned bytes are what the connector delivers as the credential's
// plaintext; the token id also stays the resource's revocation handle, outside
// this document and outside the plaintext.
func apiKeyV2Value(keyValue, keyID, baseURL string, expiresAt time.Time) ([]byte, error) {
	document := apiKeyV2Document{
		KeyValue:   keyValue,
		Provider:   apiKeyV2Provider,
		BaseURL:    baseURL,
		KeyID:      keyID,
		HeaderName: apiKeyV2HeaderName,
	}
	// The profile's `expires_at` is a date — an RFC 3339 full-date of at most
	// ten characters — not a timestamp, and the profile omits an empty optional
	// rather than writing a zero value into it. A caller with no expiry must
	// therefore produce no `expires_at` at all: formatting a zero `time.Time`
	// would emit "0001-01-01", which is a date the profile never sanctioned and
	// which a reader would take for a real deadline. The issuance path always
	// has the provider's own expiry, which is the value verified against the
	// approved deadline; this branch is the empty case.
	if !expiresAt.IsZero() {
		document.ExpiresAt = expiresAt.UTC().Format(time.DateOnly)
	}

	// Not json.Marshal: an Encoder with HTML escaping off writes the token and
	// the base URL verbatim, so `&` stays `&` instead of becoming `\u0026` and
	// the bytes match the profile's canonical form.
	var buffer bytes.Buffer
	encoder := json.NewEncoder(&buffer)
	encoder.SetEscapeHTML(false)
	if err := encoder.Encode(document); err != nil {
		return nil, fmt.Errorf("baton-grafana: encode the api_key_v2 credential document: %w", err)
	}
	// Encode terminates the document with a newline; the value is the document.
	return bytes.TrimSuffix(buffer.Bytes(), []byte("\n")), nil
}
