package hookproof

import (
	"crypto/ed25519"
	"crypto/rand"
	"testing"
	"time"
)

func TestVerify(t *testing.T) {
	_, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	const iss = "https://core.example"
	p := Sign(priv, Claim, iss, "toolu_1", "conv", now)
	if _, ok := Verify(p, Claim, iss, "toolu_1", "conv", now); !ok {
		t.Fatal("a good proof refused")
	}
	if _, ok := Verify(p, Call, iss, "toolu_1", "conv", now); ok {
		t.Error("another purpose: accepted")
	}
	if _, ok := Verify(p, Claim, iss, "toolu_2", "conv", now); ok {
		t.Error("another call: accepted")
	}
	if _, ok := Verify(p, Claim, iss, "toolu_1", "conv", now.Add(Skew+time.Minute)); ok {
		t.Error("too old: accepted")
	}
	// A newline in a field could make two (call, thread) pairs sign the
	// same message: refused even when the signature matches.
	for _, f := range [][3]string{{iss, "toolu_1\nconv", ""}, {iss, "toolu_1", "c\rx"}, {iss + "\n", "toolu_1", "conv"}} {
		q := Sign(priv, Claim, f[0], f[1], f[2], now)
		if _, ok := Verify(q, Claim, f[0], f[1], f[2], now); ok {
			t.Errorf("a newline accepted: %q", f)
		}
	}
}
