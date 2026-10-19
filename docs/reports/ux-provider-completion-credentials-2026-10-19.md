# UX & Provider Completion — `{secret:NAME}` credential references in Core (stage C)

Date: 2026-10-19
Repo: LumenCortex Core (`~/IdeaProjects/me/LumenCortex`), branch `main`, local only.
Scope owner: CoreCredentials (config credential API, openai factory wiring, CLI env-only preflight, schema, security doc).

## Scope

- `config`: `CredentialResolver` contract, `SecretName`, `ErrCredentialNotFound`, resolver-aware
  `ResolveAPIKey`/`APIKeySet` on `ResolvedModel` and `ResolvedProvider`, schema/testdata/tests.
- `provider/openai`: `Options.Credentials`, `FromModel` resolves through it.
- `cmd/lcx-go`: env-only (nil resolver), preflight refuses `{secret:NAME}` with a clear diagnostic.
- `docs/security-and-trust.md`: external-credentials section updated (the doc's own rule requires it).
- Not mine (deferred to CoreProtocol): `provider` dispatcher package, `provider/anthropic`,
  `Provider.Package` constant/validation/enum, Desktop Keychain resolver.

## Contract (as consumed by CoreProtocol, DesktopPlatform, Desktop)

`lumencortex.json` `providers.<id>.settings.apiKey` accepts, everywhere `{env:NAME}` was accepted
(load, merge, save, schema validation, credential-redirect trust rule):

- `{env:NAME}` — unchanged, read with `os.Getenv` at client-build time.
- `{secret:NAME}` — `NAME` matches `^[A-Za-z0-9][A-Za-z0-9._-]{0,127}$`, resolved through
  `config.CredentialResolver`. Literal keys remain rejected.

```go
var ErrCredentialNotFound = errors.New("credential not found")
type CredentialResolver interface { Resolve(name string) (string, error) }
func SecretName(ref string) (string, bool)
func (m ResolvedModel)  ResolveAPIKey(r CredentialResolver) (string, error)
func (m ResolvedModel)  APIKeySet(r CredentialResolver) bool
func (p ResolvedProvider) ResolveAPIKey(r CredentialResolver) (string, error)
func (p ResolvedProvider) APIKeySet(r CredentialResolver) bool
```

Semantics: `ResolveAPIKey` returns `("", nil)` for no apiKey; each reference picks exactly one
source (`{env:NAME}` never consults the resolver, `{secret:NAME}` never reads the environment);
nil resolver + secret ref is an error naming only the reference; a resolver error wrapping
`ErrCredentialNotFound` maps to an error that `errors.Is(err, ErrCredentialNotFound)`; any other
resolver error is reduced to "resolving secret NAME failed" (the resolver's text is dropped so a
misbehaving store cannot leak a value). `APIKeySet(r)` is `resolve succeeded and non-empty`;
`Catalog()` passes nil, so catalog `ModelInfo` stays env-only knowledge (`apiKeyEnv` empty,
`apiKeySet` false for secret refs).

Provider constructors: `openai.Options{..., Credentials config.CredentialResolver}`;
`openai.FromModel(m, o)` calls `m.ResolveAPIKey(o.Credentials)`. `runtime`'s default factory passes
an empty `Options` (nil resolver, env-only) and needs no change.

## Redaction guarantees and regression coverage

No error, warning, log line, ModelInfo/ProviderInfo JSON, config export or event may contain a
secret value; errors name only the reference. Tests plant
`sk-secret-NEVER-PRINT-9876543210` / `sk-secret-NEVER-PRINT-246810` through a test-only fake
resolver (Core ships no implementation) and assert absence in:

- `config/credentials_test.go`: `TestResolverErrorsNeverCarryTheSecretValue` (not-found, foreign
  resolver error, empty value, nil resolver), `TestSecretValueExposure` (JSON + `%+v` of Catalog,
  Effective, ResolvedModel, ResolvedProvider, warnings), `TestNilResolverWithSecretRefNamesOnlyTheReference`,
  `TestEachReferencePicksExactlyOneSource` (env ref never consults the resolver; secret ref never
  reads the environment), `TestSecretReferenceResolvesThroughTheCredentialResolver`
  (incl. same-named env var must not win), `TestSecretReferenceShapes` (bad names rejected),
  `TestSaveRoundTripsASecretReference`, `TestCredentialRedirectTreatsSecretReferencesLikeEnvReferences`
  (unit merge cases + end-to-end Load with a trusted workspace).
- `provider/openai/factory_test.go`: `TestFromModelSecretFailuresNameOnlyTheReference` — includes
  the client build failing *after* successful resolution (`ErrNoEndpoint`) with the key still
  absent from the error; `TestFromModelResolvesSecretReferencesThroughCredentials` proves the
  resolver value reaches the `Authorization` header.
- `cmd/lcx-go/provider_test.go`: `TestSecretReferenceIsRefusedWithAClearDiagnostic`.
- Removing the redaction (e.g. wrapping the resolver's error text, or echoing values) fails these
  tests.

## CLI (env-only, non-macOS safe)

`cmd/lcx-go` passes no resolver. Preflight detects `config.SecretName(model.APIKeyRef)` before
building a client and refuses with a message that names the model, the reference and the
`{env:NAME}` way out; no request is made. Unset env refs keep their existing diagnostic.

## Files

- `config/credentials.go` (new), `config/resolve.go`, `config/types.go`, `config/schema_test.go`,
  `config/secrets_test.go`, `config/credentials_test.go` (new),
  `config/testdata/valid/secret-ref.json` (new), `config/testdata/invalid/secret-ref-bad-shape.json` (new),
  `docs/lumencortex.schema.json` (apiKey pattern + description; the embedded copy in `docs/embed.go`
  is the same file, verified by `TestSchemaFileIsTheEmbeddedOne`),
  `provider/openai/factory.go`, `provider/openai/factory_test.go` (new),
  `cmd/lcx-go/provider.go`, `cmd/lcx-go/provider_test.go`,
  `docs/security-and-trust.md`, this report.

## Validation

Commands run in Core (results below): `go build ./...`, `go vet ./...`, `go test -count=1 ./...`.
Focused: `go test -count=1 ./config/ ./provider/openai/ ./cmd/lcx-go/` — all ok.
Pre-existing gofmt findings in `cmd/lcx-go/main.go`/`main_test.go` are not this task's files.

## Decisions

- No `config.EnvResolver` chain helper was needed: nil resolver already means env-only, which is
  the only mode the CLI has (kept minimal per task).
- `Catalog()`/`ModelInfo` intentionally unchanged: without a resolver a secret ref cannot be
  confirmed, so `apiKeySet` is false there; the Desktop passes its resolver where it matters
  (client build) — owner-approved contract shape, presentation is Desktop's call.

## Handoffs

- CoreProtocol: build against this commit; dispatcher calls `ResolveAPIKey(r)` forms; `cmd/lcx-go`
  and `config` testdata touched here — re-read before editing (`schema_test.go` counts fixtures).
- DesktopPlatform/Desktop: implement `config.CredentialResolver` over the Keychain and pass it via
  `openai.Options.Credentials` (or the dispatcher's Options once it lands).
