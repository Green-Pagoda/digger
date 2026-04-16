package github

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
)

// TestToken_RedactsAcrossAllFormatVerbs locks in the contract that the secret
// value cannot reach a log, error, or serialized payload via any standard Go
// formatting verb. If a future change adds a field, removes a method, or
// switches the struct printing, this test must catch it.
func TestToken_RedactsAcrossAllFormatVerbs(t *testing.T) {
	const secret = "ghp_supersecret_credential_value"
	tok := Token(secret)

	cases := map[string]string{
		"%s":  fmt.Sprintf("%s", tok),
		"%v":  fmt.Sprintf("%v", tok),
		"%+v": fmt.Sprintf("%+v", tok),
		"%#v": fmt.Sprintf("%#v", tok),
		"%q":  fmt.Sprintf("%q", tok),
	}
	for verb, formatted := range cases {
		assert.NotContains(t, formatted, secret,
			"Token must not leak via %s verb (got %q)", verb, formatted)
	}

	jsonOut, err := json.Marshal(tok)
	assert.NoError(t, err)
	assert.NotContains(t, string(jsonOut), secret,
		"Token must not leak via json.Marshal (got %s)", jsonOut)
}

// TestGithubService_RedactsTokenInFormatting verifies that printing the
// containing struct does not leak the token, which is the realistic vector
// (someone slog.Info("svc", "value", svc) or fmt.Sprintf("%+v", svc)).
func TestGithubService_RedactsTokenInFormatting(t *testing.T) {
	const secret = "ghp_supersecret_credential_value"
	svc := GithubService{
		Owner:    "testowner",
		RepoName: "testrepo",
		Token:    Token(secret),
	}

	for _, verb := range []string{"%v", "%+v", "%#v"} {
		formatted := fmt.Sprintf(verb, svc)
		assert.NotContains(t, formatted, secret,
			"GithubService must not leak Token via %s (got %q)", verb, formatted)
		assert.True(t, strings.Contains(formatted, "REDACTED"),
			"GithubService %s output should signal redaction (got %q)", verb, formatted)
	}

	jsonOut, err := json.Marshal(svc)
	assert.NoError(t, err)
	assert.NotContains(t, string(jsonOut), secret,
		"GithubService must not leak Token via json.Marshal (got %s)", jsonOut)
}

// TestToken_StringConversionStillUnwraps verifies the explicit-cast escape
// hatch keeps working for code paths that genuinely need the raw value (e.g.
// the GraphQL Authorization header).
func TestToken_StringConversionStillUnwraps(t *testing.T) {
	const secret = "ghp_supersecret_credential_value"
	tok := Token(secret)
	assert.Equal(t, secret, string(tok),
		"explicit string(Token) must still expose the raw value")
}
