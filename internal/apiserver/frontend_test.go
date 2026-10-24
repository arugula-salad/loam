package apiserver

import (
	"bytes"
	"testing"
)

// A front end's sealed credential opens for its own sandbox only, and not at
// all once a bit of it changes; and it is no project secret's ciphertext.
func TestSealOpen(t *testing.T) {
	h := newHarness(t)
	value := []byte("envd-token")
	sealed, err := h.srv.Seal("sb-aaaaaaaaaaaa", value)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(sealed, value) {
		t.Fatal("the value is in the clear")
	}
	if again, _ := h.srv.Seal("sb-aaaaaaaaaaaa", value); bytes.Equal(again, sealed) {
		t.Fatal("two seals of one value are the same: the nonce isn't fresh")
	}
	got, err := h.srv.Open("sb-aaaaaaaaaaaa", sealed)
	if err != nil || !bytes.Equal(got, value) {
		t.Fatalf("round trip: %q %v", got, err)
	}
	if _, err := h.srv.Open("sb-bbbbbbbbbbbb", sealed); err == nil {
		t.Fatal("opened as another sandbox's")
	}
	for i := range sealed {
		bad := append([]byte{}, sealed...)
		bad[i] ^= 0x80
		if _, err := h.srv.Open("sb-aaaaaaaaaaaa", bad); err == nil {
			t.Fatalf("opened with byte %d changed", i)
		}
	}
	if _, err := h.srv.Open("sb-aaaaaaaaaaaa", sealed[:5]); err == nil {
		t.Fatal("opened a truncated value")
	}
	// A project secret sealed under the same key is not a sandbox's credential.
	secret := h.srv.sealer.seal("p", "X", value)
	if _, err := h.srv.Open("sb-aaaaaaaaaaaa", secret); err == nil {
		t.Fatal("a project secret opened as a sandbox credential")
	}
	if !h.srv.CanSeal() {
		t.Fatal("CanSeal with a key")
	}
}

func TestTrafficToken(t *testing.T) {
	h := newHarness(t)
	a, err := h.srv.TrafficToken("sb-a")
	if err != nil || len(a) != 64 {
		t.Fatalf("token: %q %v", a, err)
	}
	again, _ := h.srv.TrafficToken("sb-a")
	b, _ := h.srv.TrafficToken("sb-b")
	if a != again || a == b {
		t.Fatal("tokens must be stable and sandbox-specific")
	}
	other, err := newSealer(bytes.Repeat([]byte{99}, SecretKeySize))
	if err != nil {
		t.Fatal(err)
	}
	srv := &Server{sealer: other}
	changed, _ := srv.TrafficToken("sb-a")
	if a == changed {
		t.Fatal("token survived key rotation")
	}
	if _, err := (&Server{}).TrafficToken("sb-a"); err == nil {
		t.Fatal("token without key")
	}
}
