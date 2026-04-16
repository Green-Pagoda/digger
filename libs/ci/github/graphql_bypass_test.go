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
