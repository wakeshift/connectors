package beacon

import (
	"crypto/ed25519"
	"crypto/rand"
	"strings"
	"testing"
	"time"

	"github.com/wakeshift/connectors/internal/jws"
)

const iss = "https://app.wakeshift.ai"

func sign(t *testing.T, key ed25519.PrivateKey, c Claims) string {
	t.Helper()
	c.Issuer, c.Audience, c.Redeem, c.Ticket, c.App = iss, Audience, iss+"/v1/connector/redeem", "wst_t", "quizzer"
	if c.Expires == 0 {
		c.Expires = time.Now().Add(time.Minute).Unix()
	}
	tok, err := jws.Sign(key, "k1", Type, c)
	if err != nil {
		t.Fatal(err)
	}
	return tok
}

// VerifyBound: a beacon naming a conversation must name the hook's; one
// naming only a call must name the hook's call; one naming neither only
// for a harness that can't do better.
func TestVerifyBound(t *testing.T) {
	pub, priv, _ := ed25519.GenerateKey(rand.Reader)
	keys := func(string) ed25519.PublicKey { return pub }
	now := time.Now()
	for _, c := range []struct {
		name   string
		claims Claims
		bind   Binding
		ok     bool
	}{
		{"its conversation", Claims{Thread: "c1"}, Binding{Conversation: "c1"}, true},
		{"another conversation", Claims{Thread: "c2"}, Binding{Conversation: "c1", Unbound: true}, false},
		{"its call", Claims{Call: "toolu_1"}, Binding{Conversation: "c1", Call: "toolu_1"}, true},
		{"another call", Claims{Call: "toolu_2"}, Binding{Conversation: "c1", Call: "toolu_1"}, false},
		{"a call, no call here", Claims{Call: "toolu_1"}, Binding{Conversation: "c1", Unbound: true}, false},
		{"unbound, not accepted", Claims{}, Binding{Conversation: "c1", Call: "toolu_1"}, false},
		{"unbound, accepted", Claims{}, Binding{Conversation: "c1", Unbound: true}, true},
		{"no conversation", Claims{}, Binding{Unbound: true}, false},
		{"expired", Claims{Thread: "c1", Expires: now.Add(-time.Second).Unix()}, Binding{Conversation: "c1"}, false},
	} {
		_, err := VerifyBound(sign(t, priv, c.claims), c.bind, []string{iss}, keys, now)
		if (err == nil) != c.ok {
			t.Fatalf("%s: %v", c.name, err)
		}
	}
	_, other, _ := ed25519.GenerateKey(rand.Reader)
	if _, err := VerifyBound(sign(t, other, Claims{Thread: "c1"}), Binding{Conversation: "c1"}, []string{iss}, keys, now); err == nil {
		t.Fatal("another key's")
	}
	if _, err := VerifyBound(sign(t, priv, Claims{Thread: "c1"}), Binding{Conversation: "c1"}, []string{"https://other.example"}, keys, now); err == nil {
		t.Fatal("an issuer not trusted")
	}
}

// A beacon names the app it was minted for (hooks check it against the
// server the result came from), and redeems on its issuer's own origin
// (the ticket and the hook's earlier link go there).
func TestVerifyNeedsAppAndIssuerRedeem(t *testing.T) {
	pub, priv, _ := ed25519.GenerateKey(rand.Reader)
	keys := func(string) ed25519.PublicKey { return pub }
	now := time.Now()
	raw := func(c Claims) string {
		c.Issuer, c.Audience, c.Ticket, c.Thread = iss, Audience, "wst_t", "c1"
		c.Expires = now.Add(time.Minute).Unix()
		tok, err := jws.Sign(priv, "k1", Type, c)
		if err != nil {
			t.Fatal(err)
		}
		return tok
	}
	bind := Binding{Conversation: "c1"}
	if _, err := VerifyBound(raw(Claims{App: "quizzer", Redeem: iss + "/v1/connector/redeem"}), bind, []string{iss}, keys, now); err != nil {
		t.Fatalf("a good one: %v", err)
	}
	for name, c := range map[string]Claims{
		"no app":                 {Redeem: iss + "/v1/connector/redeem"},
		"redeem on another host": {App: "quizzer", Redeem: "https://evil.example/v1/connector/redeem"},
		"redeem over http":       {App: "quizzer", Redeem: "http://app.wakeshift.ai/v1/connector/redeem"},
	} {
		if _, err := VerifyBound(raw(c), bind, []string{iss}, keys, now); err == nil {
			t.Fatalf("%s: accepted", name)
		}
		if _, err := Verify(raw(c), "c1", []string{iss}, keys, now); err == nil {
			t.Fatalf("%s: Verify accepted", name)
		}
	}
}

func TestText(t *testing.T) {
	tok := "aaaaaaaa.bbbbbbbb.cccccccc"
	text := "joined #general\n\n" + Text(tok)
	if InText(text) != tok || InText("joined") != "" || InText("wakeshift-beacon: short.a.b") != "" {
		t.Fatal("InText")
	}
	if got := StripText(text); got != "joined #general" || strings.Contains(StripText(Text(tok)), "beacon") {
		t.Fatalf("StripText %q", got)
	}
}
