package verify_test

import (
	"testing"

	"github.com/kaulie/event-center/internal/model"
	"github.com/kaulie/event-center/internal/verify"
)

func TestSignAndVerifyHMACSHA256(t *testing.T) {
	body := []byte(`{"hello":"world"}`)
	secret := "s3cr3t"
	sig := verify.SignHMACSHA256(secret, body)

	if err := verify.HMACSHA256(secret, body, sig, ""); err != nil {
		t.Fatalf("valid signature rejected: %v", err)
	}
	if err := verify.HMACSHA256("other", body, sig, ""); err == nil {
		t.Fatal("signature from a different secret must be rejected")
	}
	if err := verify.HMACSHA256(secret, []byte("tampered"), sig, ""); err == nil {
		t.Fatal("tampered body must be rejected")
	}
	if err := verify.HMACSHA256(secret, body, "", ""); err == nil {
		t.Fatal("missing signature must be rejected")
	}
	if err := verify.HMACSHA256("", body, sig, ""); err == nil {
		t.Fatal("empty secret must be rejected")
	}
}

func TestBearerAcceptsRawAndPrefixedToken(t *testing.T) {
	if err := verify.Bearer("tok", "Bearer tok"); err != nil {
		t.Fatalf("prefixed token rejected: %v", err)
	}
	if err := verify.Bearer("tok", "tok"); err != nil {
		t.Fatalf("raw token rejected: %v", err)
	}
	if err := verify.Bearer("tok", "Bearer nope"); err == nil {
		t.Fatal("wrong token must be rejected")
	}
	if err := verify.Bearer("tok", ""); err == nil {
		t.Fatal("missing token must be rejected")
	}
}

func TestModeDispatchesOnVerifyMode(t *testing.T) {
	body := []byte(`{"a":1}`)
	sig := verify.SignHMACSHA256("s", body)

	hmacSource := &model.Source{VerifyMode: model.VerifyHMACSHA256, Secret: "s"}
	if err := verify.Mode(hmacSource, body, map[string]string{"x-hub-signature-256": sig}); err != nil {
		t.Fatalf("hmac mode rejected valid signature: %v", err)
	}
	if err := verify.Mode(hmacSource, body, map[string]string{"x-hub-signature-256": "sha256=bad"}); err == nil {
		t.Fatal("hmac mode accepted an invalid signature")
	}

	bearerSource := &model.Source{VerifyMode: model.VerifyBearer, Secret: "t"}
	if err := verify.Mode(bearerSource, body, map[string]string{"authorization": "Bearer t"}); err != nil {
		t.Fatalf("bearer mode rejected valid token: %v", err)
	}

	openSource := &model.Source{VerifyMode: model.VerifyNone}
	if err := verify.Mode(openSource, body, nil); err != nil {
		t.Fatalf("none mode rejected: %v", err)
	}

	weird := &model.Source{VerifyMode: "magic"}
	if err := verify.Mode(weird, body, nil); err == nil {
		t.Fatal("unsupported mode must be rejected")
	}
}
