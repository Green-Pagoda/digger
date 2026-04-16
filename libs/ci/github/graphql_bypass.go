package github

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// bypassHTTPTimeout bounds every GraphQL bypass call end-to-end. The whole
// feature exists to unblock apply; a hung GitHub endpoint with no deadline
// would wedge it indefinitely, which is strictly worse than the chicken-and-egg
// this code is fixing. Applied as the HTTP client's Timeout (covers
// connection, TLS handshake, headers, body). Callers' ctx handles upstream
// cancellation.
const bypassHTTPTimeout = 30 * time.Second

// defaultBypassHTTPClient is used when GithubService.HTTPClient is nil. It is
// a dedicated client so we do not share state with http.DefaultClient (which
// has no timeout and can be mutated elsewhere). Not mutated after init —
// per-call overrides go through GithubService.HTTPClient so tests can inject
// without racing on a shared global.
var defaultBypassHTTPClient = &http.Client{Timeout: bypassHTTPTimeout}

// bypassQuery is the GraphQL query used by InspectMergeability to fetch
// reviewDecision and the status check rollup in a single call.
const bypassQuery = `
query($owner: String!, $repo: String!, $number: Int!) {
  repository(owner: $owner, name: $repo) {
    pullRequest(number: $number) {
      reviewDecision
      commits(last: 1) {
        nodes {
          commit {
            statusCheckRollup {
              contexts(first: 100) {
                totalCount
                nodes {
                  ... on StatusContext {
                    context
                    state
                  }
                  ... on CheckRun {
                    name
                    conclusion
                    status
                  }
                }
              }
            }
          }
        }
      }
    }
  }
}`

// graphqlRequest is the JSON body sent to the GraphQL endpoint.
type graphqlRequest struct {
	Query     string         `json:"query"`
	Variables map[string]any `json:"variables"`
}

// graphqlResponse mirrors the shape of the GraphQL response we expect.
type graphqlResponse struct {
	Data struct {
		Repository struct {
			PullRequest struct {
				ReviewDecision string `json:"reviewDecision"`
				Commits        struct {
					Nodes []struct {
						Commit struct {
							StatusCheckRollup *struct {
								Contexts struct {
									TotalCount int               `json:"totalCount"`
									Nodes      []json.RawMessage `json:"nodes"`
								} `json:"contexts"`
							} `json:"statusCheckRollup"`
						} `json:"commit"`
					} `json:"nodes"`
				} `json:"commits"`
			} `json:"pullRequest"`
		} `json:"repository"`
	} `json:"data"`
	Errors []struct {
		Message string `json:"message"`
	} `json:"errors"`
}

// checkContext is a unified representation of either a StatusContext or a
// CheckRun from the GraphQL statusCheckRollup union type.
type checkContext struct {
	// StatusContext fields
	Context string `json:"context"`
	State   string `json:"state"`
	// CheckRun fields
	Name       string `json:"name"`
	Conclusion string `json:"conclusion"`
	Status     string `json:"status"`
}

// IsStatusContext returns true if this context came from the older commit
// statuses API (has a non-empty Context field).
func (c checkContext) IsStatusContext() bool {
	return c.Context != ""
}

// DisplayName returns the identifying name regardless of type.
func (c checkContext) DisplayName() string {
	if c.Context != "" {
		return c.Context
	}
	return c.Name
}

// IsPassing returns true if the check/status has a successful outcome.
// In-flight check runs (status IN_PROGRESS or QUEUED) have no Conclusion
// yet and therefore return false — intentional, because the bypass must
// treat any still-running check as a blocker until it completes. A hidden
// non-digger/apply failure that hadn't yet concluded must not let the
// bypass fire.
func (c checkContext) IsPassing() bool {
	if c.IsStatusContext() {
		return c.State == "SUCCESS"
	}
	return c.Conclusion == "SUCCESS" || c.Conclusion == "NEUTRAL" || c.Conclusion == "SKIPPED"
}

// bypassCheckResult holds the parsed result of the GraphQL bypass query.
type bypassCheckResult struct {
	ReviewDecision string
	Contexts       []checkContext
	TotalCount     int
}

// errorBodyMaxBytes caps the response body length we include in error
// messages. GHE proxies (Varnish, Fastly) can return multi-KB HTML pages on
// 4xx/5xx responses; propagating those verbatim through error wrapping bloats
// logs and Sentry payloads without adding diagnostic value beyond the first
// few hundred bytes.
const errorBodyMaxBytes = 512

// truncateForError returns body as a string, capped at errorBodyMaxBytes with
// an explicit suffix when truncation occurred so readers know something was
// cut.
func truncateForError(body []byte) string {
	if len(body) <= errorBodyMaxBytes {
		return string(body)
	}
	return string(body[:errorBodyMaxBytes]) + "...[truncated]"
}

// graphqlBaseURL derives the GraphQL endpoint from the REST client's base URL.
// For github.com it returns https://api.github.com/graphql. For GHE or
// httptest servers it appends /graphql to the existing base.
func graphqlBaseURL(restBaseURL string) string {
	restBaseURL = strings.TrimSuffix(restBaseURL, "/")
	if strings.HasSuffix(restBaseURL, "api.github.com") {
		return "https://api.github.com/graphql"
	}
	// GHE: https://hostname/api/v3 → https://hostname/api/graphql
	if strings.HasSuffix(restBaseURL, "/api/v3") {
		return strings.TrimSuffix(restBaseURL, "/v3") + "/graphql"
	}
	// httptest or other: just append /graphql
	return restBaseURL + "/graphql"
}

// queryBypassGraphQL executes the bypass GraphQL query and returns the parsed
// result. Returns an error if the request fails or the response is malformed.
// The client argument is the HTTP client used for the request; callers
// typically pass GithubService.HTTPClient (or defaultBypassHTTPClient when
// that field is nil).
func queryBypassGraphQL(ctx context.Context, client *http.Client, restBaseURL, token, owner, repo string, prNumber int) (*bypassCheckResult, error) {
	url := graphqlBaseURL(restBaseURL)

	reqBody := graphqlRequest{
		Query: bypassQuery,
		Variables: map[string]any{
			"owner":  owner,
			"repo":   repo,
			"number": prNumber,
		},
	}
	body, err := json.Marshal(reqBody)
	if err != nil {
		return nil, fmt.Errorf("error marshaling GraphQL request: %v", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("error creating GraphQL request: %v", err)
	}
	req.Header.Set("Authorization", "bearer "+token)
	req.Header.Set("Content-Type", "application/json")

	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("error executing GraphQL request: %v", err)
	}
	defer resp.Body.Close()

	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("error reading GraphQL response: %v", err)
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("GraphQL request failed with status %d: %s",
			resp.StatusCode, truncateForError(respBody))
	}

	var gqlResp graphqlResponse
	if err := json.Unmarshal(respBody, &gqlResp); err != nil {
		return nil, fmt.Errorf("error unmarshaling GraphQL response: %v", err)
	}
	if len(gqlResp.Errors) > 0 {
		// GitHub GraphQL routinely returns multiple errors (one per failed
		// field path, mixed auth/rate-limit/deprecation signals). Surfacing
		// only Errors[0] sends operators chasing the wrong cause.
		msgs := make([]string, len(gqlResp.Errors))
		for i, e := range gqlResp.Errors {
			msgs[i] = e.Message
		}
		return nil, fmt.Errorf("GraphQL error(s): %s", strings.Join(msgs, "; "))
	}

	result := &bypassCheckResult{
		ReviewDecision: gqlResp.Data.Repository.PullRequest.ReviewDecision,
	}

	commits := gqlResp.Data.Repository.PullRequest.Commits.Nodes
	if len(commits) == 0 || commits[0].Commit.StatusCheckRollup == nil {
		return result, nil
	}

	rollup := commits[0].Commit.StatusCheckRollup.Contexts
	result.TotalCount = rollup.TotalCount

	for _, raw := range rollup.Nodes {
		var cc checkContext
		if err := json.Unmarshal(raw, &cc); err != nil {
			return nil, fmt.Errorf("error unmarshaling check context: %v", err)
		}
		result.Contexts = append(result.Contexts, cc)
	}

	return result, nil
}
