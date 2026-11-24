package connector

import (
	"testing"
	"time"
)

// Retries spread out: within their bounds, and not all the same.
func TestJitterBounds(t *testing.T) {
	d := 5 * time.Second
	seen := map[time.Duration]bool{}
	for range 200 {
		j, u, b := Jitter(d), JitterUp(d), Backoff(d)
		if j < 4*time.Second || j > 6*time.Second {
			t.Fatalf("Jitter(5s) = %v", j)
		}
		if u < d || u > 6*time.Second {
			t.Fatalf("JitterUp(5s) = %v: never shorter than asked", u)
		}
		if b < d/2 || b > d {
			t.Fatalf("Backoff(5s) = %v", b)
		}
		seen[j] = true
	}
	if len(seen) < 50 {
		t.Fatalf("only %d distinct pauses in 200", len(seen))
	}
	if Jitter(0) != 0 || JitterUp(0) != 0 || Backoff(0) != 0 {
		t.Fatal("a zero pause should stay zero")
	}
}
