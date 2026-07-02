package scout

import (
	"io"
	"net/http"
	"strings"
)

// mockDoer routes requests to a test function.
type mockDoer struct {
	fn func(*http.Request) (*http.Response, error)
}

func (m mockDoer) Do(r *http.Request) (*http.Response, error) { return m.fn(r) }

func resp(status int, body string) *http.Response {
	return &http.Response{StatusCode: status, Body: io.NopCloser(strings.NewReader(body)), Header: make(http.Header)}
}
