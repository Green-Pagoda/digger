package github

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

// TestGraphqlBaseURL covers the URL transformations applied to derive the
// GraphQL endpoint from the REST client's base URL. The GHE case is the one
// most likely to break in production at a customer site, since github.com and
// httptest are exercised end-to-end by the bypass tests.
func TestGraphqlBaseURL(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want string
	}{
		{
			name: "github.com without trailing slash",
			in:   "https://api.github.com",
			want: "https://api.github.com/graphql",
		},
		{
			name: "github.com with trailing slash (go-github default)",
			in:   "https://api.github.com/",
			want: "https://api.github.com/graphql",
		},
		{
			name: "GHE with trailing slash (go-github SetBaseURL default)",
			in:   "https://ghe.example.com/api/v3/",
			want: "https://ghe.example.com/api/graphql",
		},
		{
			name: "GHE without trailing slash",
			in:   "https://ghe.example.com/api/v3",
			want: "https://ghe.example.com/api/graphql",
		},
		{
			name: "httptest server (used by tests)",
			in:   "http://127.0.0.1:54321/",
			want: "http://127.0.0.1:54321/graphql",
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			assert.Equal(t, c.want, graphqlBaseURL(c.in))
		})
	}
}

// TestCheckContextIsPassing locks in the conclusion/state allowlist across
// both halves of the checkContext union. The enumerated non-passing
// CheckRun conclusions (CANCELLED, TIMED_OUT, ACTION_REQUIRED, STALE,
// STARTUP_FAILURE) are intentionally blocking — see the IsPassing doc
// comment for rationale. If GitHub adds a new CheckRun conclusion, the
// default-block behavior of this test's "future enum value" case must
// continue to hold: an unrecognized conclusion blocks, never passes.
func TestCheckContextIsPassing(t *testing.T) {
	cases := []struct {
		name string
		cc   checkContext
		want bool
	}{
		// StatusContext (legacy commit statuses)
		{"statusContext SUCCESS passes", checkContext{Context: "ci/build", State: "SUCCESS"}, true},
		{"statusContext PENDING blocks", checkContext{Context: "ci/build", State: "PENDING"}, false},
		{"statusContext FAILURE blocks", checkContext{Context: "ci/build", State: "FAILURE"}, false},
		{"statusContext ERROR blocks", checkContext{Context: "ci/build", State: "ERROR"}, false},
		{"statusContext EXPECTED blocks", checkContext{Context: "ci/build", State: "EXPECTED"}, false},
		{"statusContext empty state blocks", checkContext{Context: "ci/build", State: ""}, false},

		// CheckRun terminal conclusions treated as passing
		{"checkRun SUCCESS passes", checkContext{Name: "digger/apply", Conclusion: "SUCCESS"}, true},
		{"checkRun NEUTRAL passes", checkContext{Name: "digger/apply", Conclusion: "NEUTRAL"}, true},
		{"checkRun SKIPPED passes", checkContext{Name: "digger/apply", Conclusion: "SKIPPED"}, true},

		// CheckRun terminal conclusions treated as non-passing
		{"checkRun FAILURE blocks", checkContext{Name: "digger/apply", Conclusion: "FAILURE"}, false},
		{"checkRun CANCELLED blocks", checkContext{Name: "digger/apply", Conclusion: "CANCELLED"}, false},
		{"checkRun TIMED_OUT blocks", checkContext{Name: "digger/apply", Conclusion: "TIMED_OUT"}, false},
		{"checkRun ACTION_REQUIRED blocks", checkContext{Name: "digger/apply", Conclusion: "ACTION_REQUIRED"}, false},
		{"checkRun STALE blocks", checkContext{Name: "digger/apply", Conclusion: "STALE"}, false},
		{"checkRun STARTUP_FAILURE blocks", checkContext{Name: "digger/apply", Conclusion: "STARTUP_FAILURE"}, false},

		// CheckRun in-flight (Conclusion unpopulated until terminal)
		{"checkRun IN_PROGRESS (no conclusion) blocks", checkContext{Name: "digger/apply", Status: "IN_PROGRESS"}, false},
		{"checkRun QUEUED (no conclusion) blocks", checkContext{Name: "digger/apply", Status: "QUEUED"}, false},

		// Future-proofing: unrecognized CheckRun conclusion must block.
		{"checkRun unknown future conclusion blocks", checkContext{Name: "digger/apply", Conclusion: "SOME_FUTURE_STATE"}, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			assert.Equal(t, c.want, c.cc.IsPassing())
		})
	}
}
