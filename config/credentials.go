package config

import (
	"errors"
	"fmt"
	"os"
	"regexp"
)

// ErrCredentialNotFound reports a {secret:NAME} reference whose name the CredentialResolver does
// not hold. It is matched with errors.Is.
var ErrCredentialNotFound = errors.New("credential not found")

// CredentialResolver resolves {secret:NAME} references when a provider client is built. Core stays
// OS-agnostic and ships no store: hosts pass their implementation (the macOS Keychain lives in
// Desktop) through the provider Options, and the CLI passes none.
//
// Resolve returns an error wrapping ErrCredentialNotFound when NAME is absent. The error text of a
// conforming implementation never contains the secret's value; resolveAPIKey nevertheless drops the
// resolver's text and names only the reference, so a misbehaving store cannot leak through.
type CredentialResolver interface {
	Resolve(name string) (string, error)
}

// secretNamePattern is the NAME of a {secret:NAME} reference: it matches the names a host secret
// store may use, which are stricter than environment variable names.
const secretNamePattern = `[A-Za-z0-9][A-Za-z0-9._-]{0,127}`

var secretRef = regexp.MustCompile(`^\{secret:(` + secretNamePattern + `)\}$`)

// SecretName returns NAME for a {secret:NAME} reference.
func SecretName(ref string) (string, bool) {
	m := secretRef.FindStringSubmatch(ref)
	if m == nil {
		return "", false
	}
	return m[1], true
}

// resolveAPIKey resolves one apiKey reference. An {env:NAME} reference reads the process
// environment; an {secret:NAME} reference reads r (nil r is an error naming only the reference).
// Anything else, a literal key included, is ErrInvalidConfig. Every error names only the reference,
// never a value.
func resolveAPIKey(providerID, subject, ref string, r CredentialResolver) (string, error) {
	if ref == "" {
		return "", nil
	}
	if name, ok := EnvName(ref); ok {
		if v := os.Getenv(name); v != "" {
			return v, nil
		}
		return "", fmt.Errorf("provider %s: environment variable %s is not set", providerID, name)
	}
	if name, ok := SecretName(ref); ok {
		if r == nil {
			return "", fmt.Errorf("provider %s: secret %q cannot be resolved: no credential resolver was given", providerID, name)
		}
		v, err := r.Resolve(name)
		if err != nil {
			if errors.Is(err, ErrCredentialNotFound) {
				return "", fmt.Errorf("provider %s: secret %q: %w", providerID, name, ErrCredentialNotFound)
			}
			return "", fmt.Errorf("provider %s: resolving secret %q failed", providerID, name)
		}
		if v == "" {
			return "", fmt.Errorf("provider %s: secret %q is empty", providerID, name)
		}
		return v, nil
	}
	return "", fmt.Errorf("%w: apiKey of %s is not an {env:NAME} or {secret:NAME} reference", ErrInvalidConfig, subject)
}
