package connector

import (
	"math/rand/v2"
	"time"
)

// Jitter is d moved by up to a fifth either way, at random. Clients that
// retry on a fixed pause (5 s) otherwise come back in step: after a deploy
// or a database blip, every waiter that lost its round retries in the same
// second.
func Jitter(d time.Duration) time.Duration {
	if d <= 0 {
		return d
	}
	return d - d/5 + rand.N(2*(d/5)+1)
}

// JitterUp is d plus up to a fifth, at random: for a wait the server asked
// for (Retry-After), which a client never shortens.
func JitterUp(d time.Duration) time.Duration {
	if d <= 0 {
		return d
	}
	return d + rand.N(d/5+1)
}

// Backoff is an exponential backoff's pause of d with jitter: between half
// of d and d, at random, so clients that failed together spread out
// instead of retrying in step at every doubling.
func Backoff(d time.Duration) time.Duration {
	if d <= 0 {
		return d
	}
	return d/2 + rand.N(d/2+1)
}
