package lyrics

import (
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
)

// recordedRequest is one request a test server received.
type recordedRequest struct {
	path   string
	query  url.Values
	header http.Header
	body   string
}

// calls records what a test server was asked, so a case can prove a lookup was
// or was not made and inspect what it carried. Both providers put the identity
// they ask with in headers and the body, which is only visible here.
type calls struct {
	mu   sync.Mutex
	reqs []recordedRequest
}

func (c *calls) record(r *http.Request) {
	body, _ := io.ReadAll(r.Body)
	c.mu.Lock()
	defer c.mu.Unlock()
	c.reqs = append(c.reqs, recordedRequest{
		path:   r.URL.Path,
		query:  r.URL.Query(),
		header: r.Header.Clone(),
		body:   string(body),
	})
}

// all returns a copy of the requests, in the order they arrived.
func (c *calls) all() []recordedRequest {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]recordedRequest(nil), c.reqs...)
}

// count is how many requests were made to a path ending in suffix.
func (c *calls) count(suffix string) int {
	n := 0
	for _, r := range c.all() {
		if strings.HasSuffix(r.path, suffix) {
			n++
		}
	}
	return n
}
