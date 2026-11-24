// Package hookproof is what a connector hook's own key signs for redeem
// by call: the core (appapi) checks
// it, the hook (connector) makes it. Exported with the connector.
package hookproof

import (
	"crypto/ed25519"
	"encoding/base64"
	"fmt"
	"strings"
	"time"
)

const (
	// Call: a redeem by call ID.
	Call = "wakeshift-redeem-call"
	// Claim: a claim of a call before it goes out.
	Claim = "wakeshift-claim-call"
	// Register: a session's key, registered for its conversation when the
	// session starts (the call is "").
	Register = "wakeshift-register-session"
	// Skew is how far a proof's time may be from the core's, either way
	// (laptops' and VMs' clocks drift; the proof also covers the call and
	// the conversation, and a binding is single use).
	Skew = 5 * time.Minute
)

// Message is what the key signs: the purpose, the issuer (a proof for one
// Wakeshift isn't one for another), the call, the conversation and the
// time (Unix seconds).
func Message(purpose, issuer, call, thread string, ts int64) []byte {
	return fmt.Appendf(nil, "%s\n%s\n%s\n%s\n%d", purpose, strings.TrimSuffix(issuer, "/"), call, thread, ts)
}

// Proof is a signed proof's fields, as a redeem's body carries them.
type Proof struct {
	Key string `json:"key"` // base64url Ed25519 public key
	Sig string `json:"sig"` // base64url signature of Message
	TS  int64  `json:"ts"`
}

// Sign makes a proof with priv.
func Sign(priv ed25519.PrivateKey, purpose, issuer, call, thread string, now time.Time) Proof {
	ts := now.Unix()
	return Proof{
		Key: base64.RawURLEncoding.EncodeToString(priv.Public().(ed25519.PublicKey)),
		Sig: base64.RawURLEncoding.EncodeToString(ed25519.Sign(priv, Message(purpose, issuer, call, thread, ts))),
		TS:  ts,
	}
}

// Verify checks p signs purpose's message within Skew of now, returning
// the public key.
func Verify(p Proof, purpose, issuer, call, thread string, now time.Time) (ed25519.PublicKey, bool) {
	key, err1 := base64.RawURLEncoding.DecodeString(p.Key)
	sig, err2 := base64.RawURLEncoding.DecodeString(p.Sig)
	if err1 != nil || err2 != nil || len(key) != ed25519.PublicKeySize || len(sig) != ed25519.SignatureSize {
		return nil, false
	}
	// The message joins its fields with newlines: a field with one could
	// make two different (call, thread) pairs sign the same bytes.
	if strings.ContainsAny(call+thread+issuer, "\n\r") {
		return nil, false
	}
	at := time.Unix(p.TS, 0)
	if at.Before(now.Add(-Skew)) || at.After(now.Add(Skew)) {
		return nil, false
	}
	if !ed25519.Verify(key, Message(purpose, issuer, call, thread, p.TS), sig) {
		return nil, false
	}
	return key, true
}
