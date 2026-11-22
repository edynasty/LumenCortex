package config

import (
	"errors"
	"strings"
	"testing"
)

func TestModelRefUTF8ByteBoundsEnforcedOnMultiByteCodepoints(t *testing.T) {
	// 16 four-byte emojis = 64 UTF-8 bytes (allowed); 17 four-byte emojis = 68 UTF-8 bytes (> 64, rejected
	// even though 17 codepoints <= JSON Schema maxLength 64!).
	provOK := strings.Repeat("🚀", 16)  // 64B
	provBad := strings.Repeat("🚀", 17) // 68B
	// 48 four-byte emojis = 192 UTF-8 bytes (allowed); 49 four-byte emojis = 196 UTF-8 bytes (> 192, rejected).
	modelOK := strings.Repeat("🧠", 48)  // 192B
	modelBad := strings.Repeat("🧠", 49) // 196B

	validDoc := `{"model":"` + provOK + `/` + modelOK + `","providers":{"` + provOK + `":{"settings":{"baseURL":"https://api.example.com/v1"},"models":{"` + modelOK + `":{"modelID":"m"}}}}}`
	f, err := Parse([]byte(validDoc))
	if err != nil {
		t.Fatalf("expected 64B/192B UTF-8 boundary to pass, got %v", err)
	}
	if err := Validate(f); err != nil {
		t.Fatalf("Validate failed on 64B/192B boundary: %v", err)
	}

	badProvDoc := `{"providers":{"` + provBad + `":{"settings":{"baseURL":"https://api.example.com/v1"},"models":{"m":{"modelID":"m"}}}}}`
	if _, err := Parse([]byte(badProvDoc)); !errors.Is(err, ErrInvalidConfig) {
		t.Fatalf("expected ErrInvalidConfig for 68-byte emoji provider id, got %v", err)
	}

	badModelDoc := `{"providers":{"p":{"settings":{"baseURL":"https://api.example.com/v1"},"models":{"` + modelBad + `":{"modelID":"m"}}}}}`
	if _, err := Parse([]byte(badModelDoc)); !errors.Is(err, ErrInvalidConfig) {
		t.Fatalf("expected ErrInvalidConfig for 196-byte emoji model key, got %v", err)
	}
}
