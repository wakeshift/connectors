package harnesses

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
)

// loopTestServer answers a loop's rounds: the first loop id's first round
// with one event (200), its second by dropping the connection mid-answer
// (a front end's reset), and any other loop id's round with 410 and a
// hub.closed line. It checks each round names the token alone after
// link=, and a loop id. ids is every loop id seen, in order.
func loopTestServer(t *testing.T) (string, func() []string) {
	t.Helper()
	var mu sync.Mutex
	var ids []string
	rounds := map[string]int{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		if r.URL.Path != "/v1/stream/wait" || q.Get("link") != "tok.sig" || !strings.Contains(q.Get("loop"), "-") {
			t.Errorf("a round: %s", r.URL.String())
			w.WriteHeader(400)
			return
		}
		mu.Lock()
		id := q.Get("loop")
		if rounds[id] == 0 {
			ids = append(ids, id)
		}
		rounds[id]++
		n, first := rounds[id], ids[0] == id
		mu.Unlock()
		switch {
		case first && n == 1:
			w.Write([]byte("\n" + `{"type":"event","subject":"one"}` + "\n"))
		case first:
			w.Header().Set("Content-Type", "application/x-ndjson")
			w.WriteHeader(200)
			w.Write([]byte("\n"))
			w.(http.Flusher).Flush()
			panic(http.ErrAbortHandler) // the connection drops mid-answer
		default:
			w.WriteHeader(http.StatusGone)
			w.Write([]byte(`{"type":"hub.closed","reason":"revoked"}` + "\n"))
		}
	}))
	t.Cleanup(srv.Close)
	return srv.URL, func() []string { mu.Lock(); defer mu.Unlock(); return append([]string(nil), ids...) }
}
