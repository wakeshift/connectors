// Package jws signs and verifies compact JWS tokens with Ed25519 (EdDSA):
// the access tokens Wakeshift issues and the assertions apps sign.
package jws

import (
	"crypto/ed25519"
	"encoding/base64"
	"encoding/json"
	"errors"
	"strings"
)

// Header is a token's protected header.
type Header struct {
	Alg string `json:"alg"`
	Typ string `json:"typ,omitempty"`
	Kid string `json:"kid,omitempty"`
}

// Sign returns header.claims.signature for claims, signed by key.
func Sign(key ed25519.PrivateKey, kid, typ string, claims any) (string, error) {
	h, _ := json.Marshal(Header{Alg: "EdDSA", Typ: typ, Kid: kid})
	c, err := json.Marshal(claims)
	if err != nil {
		return "", err
	}
	input := enc(h) + "." + enc(c)
	return input + "." + enc(ed25519.Sign(key, []byte(input))), nil
}

var ErrInvalid = errors.New("jws: invalid token")

// Verify checks token's signature with the key keyFor returns for its
// header (nil: unknown key), requires typ, and decodes the claims into v.
func Verify(token, typ string, keyFor func(Header) ed25519.PublicKey, v any) (Header, error) {
	var h Header
	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		return h, ErrInvalid
	}
	hb, err := base64.RawURLEncoding.DecodeString(parts[0])
	if err != nil || json.Unmarshal(hb, &h) != nil || h.Alg != "EdDSA" || h.Typ != typ {
		return h, ErrInvalid
	}
	pub := keyFor(h)
	sig, err := base64.RawURLEncoding.DecodeString(parts[2])
	if pub == nil || err != nil || !ed25519.Verify(pub, []byte(parts[0]+"."+parts[1]), sig) {
		return h, ErrInvalid
	}
	cb, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil || json.Unmarshal(cb, v) != nil {
		return h, ErrInvalid
	}
	return h, nil
}

func enc(b []byte) string { return base64.RawURLEncoding.EncodeToString(b) }
