# UX & Provider Completion — `{secret:NAME}` credential references in Core (stage C)

Date: 2026-10-19
Repo: LumenCortex Core (`~/IdeaProjects/me/LumenCortex`), branch `main`, local only.
Scope owner: CoreCredentials (config credential API + file credential store, provider Options wiring, CLI preflight, schema, security doc).
Stage 1 commit `465eda7` (references + resolver contract); stage 2 commit: the file-backed store below (owner decision: no macOS Keychain — API keys are `{env:NAME}` references, `{secret:NAME}` references into the file store, or nothing).

## Scope

- `config`: `CredentialResolver` contract, `SecretName`, `ErrCredentialNotFound`, resolver-aware
  `ResolveAPIKey`/`APIKeySet` on `ResolvedModel` and `ResolvedProvider`, and the pure-Go
  cross-platform `FileCredentialStore` (`config/credstore.go`).
- `provider` Options (`provider.Options.Credentials`, `openai`/`anthropic` adapters forward it;
  the dispatcher is CoreProtocol's, landed as `7b8c690`).
- `cmd/lcx-go`: resolves `{secret:NAME}` through the default file store; preflight reports an
  unresolvable reference naming only the reference and the store file.
- `docs/security-and-trust.md`: external-credentials section (the doc's own rule requires it).

## Reference contract

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

## The file-backed credential store (`config/credstore.go`)

```go
func DefaultCredentialStorePath() (string, error)            // GlobalDir/credentials.json (XDG-aware, config/dir.go)
func NewFileCredentialStore(path string) *FileCredentialStore
func (s *FileCredentialStore) Set(name, secret string) error // replace; validates name and value
func (s *FileCredentialStore) Get(name string) (string, error) // missing => wraps ErrCredentialNotFound
func (s *FileCredentialStore) Delete(name string) error      // idempotent
func (s *FileCredentialStore) Has(name string) bool
func (s *FileCredentialStore) Names() ([]string, error)      // sorted; names only, never values
func (s *FileCredentialStore) Resolve(name string) (string, error) // = Get; CredentialResolver
func (s *FileCredentialStore) Path() string
```

Properties, all regression-tested in `config/credstore_test.go`:

- **Trust level, honestly stated**: not encrypted at rest; protected by file permissions
  (0600) and the user account — the same trust level as an environment variable. Never
  exported, logged or sent to frontends; no API returns all values.
- **Permissions**: written 0600 through a temporary file that is chmod'ed before the rename;
  the directory is created 0700, and a loosened directory (group/other bits) is repaired by
  stripping those bits — owner bits are never widened. Every read verifies the file mode and
  fails closed on anything but 0600 (`credential store %s has mode %04o, want %04o: refusing to
  read it; run chmod 0600 %s`) — the error names the path and modes, never content. A loose file
  also fails the write (the merge refuses to read it). Windows skips mode checks.
- **Atomicity**: temporary file in the same directory, fsync, rename, directory fsync (the
  `save.go` pattern; `syncDir`). A failed write removes the temporary file and leaves the old
  value intact — asserted with a 0500 directory (the write fails, the old secret survives, no
  `.credentials.json.tmp-*` remains).
- **Concurrency**: writers hold the per-file advisory run lock (`internal/runlock`, like
  `Save`), keyed by the resolved path in the user cache dir; the run lock belongs to the open
  file description, so goroutines and processes conflict alike, and the kernel releases it when
  the holder dies. Readers need no lock: the rename makes every read see the complete old or new
  file. `TestFileCredentialStoreConcurrentSetsLoseNothing` runs 4 writers × 8 names through two
  store instances on one path and loses none, with a concurrent reader never seeing a torn file.
- **Validation**: names must fit the `SecretName` rules; values non-empty and ≤ 8 KiB. Both fail
  before anything is touched, naming the name (never the value).
- **Corruption**: unparseable JSON (or a non-object) is an error naming the file, never its
  bytes — asserted with a file whose broken content embeds the planted secret.
- **Redaction**: the struct holds only the path, so `%v`/`%+v`/`%#v`/JSON of the store cannot
  carry a value; `TestFileCredentialStoreNeverLeaksValues` plants
  `sk-store-NEVER-PRINT-13579` and asserts absence across every failure mode's error text.

## CLI (cross-platform, one store with Desktop)

`cmd/lcx-go` builds its providers with `provider.Options{Credentials: cliCredentials()}`, where
`cliCredentials()` is the default `FileCredentialStore` when `DefaultCredentialStorePath()` is
known (else nil: environment-only). `{env:NAME}` keeps its unset-variable diagnostic. Preflight
builds the client and, when the error wraps `config.ErrCredentialNotFound`, appends the store
file location — the message names only the reference and the path
(`provider p: secret "NAME": credential not found; store it in the credential file
…/lumencortex/credentials.json`). A missing store file is an empty store: the reference is
simply not found. `TestSecretReferenceResolvesThroughTheCredentialFile` covers the refusal (no
request, no value) and the stored case (the request authenticates with the stored value).

## Redaction guarantees and regression coverage

No error, warning, log line, ModelInfo/ProviderInfo JSON, config export, event or store dump may
contain a secret value; errors name only the reference. Tests plant distinctive values through a
test-only fake resolver (stage 1) and through the real file store (stage 2):

- `config/credentials_test.go`: `TestResolverErrorsNeverCarryTheSecretValue` (not-found, foreign
  resolver error, empty value, nil resolver), `TestSecretValueExposure` (JSON + `%+v` of Catalog,
  Effective, ResolvedModel, ResolvedProvider, warnings), `TestNilResolverWithSecretRefNamesOnlyTheReference`,
  `TestEachReferencePicksExactlyOneSource`, `TestSecretReferenceResolvesThroughTheCredentialResolver`
  (incl. same-named env var must not win), `TestSecretReferenceShapes`,
  `TestSaveRoundTripsASecretReference`, `TestCredentialRedirectTreatsSecretReferencesLikeEnvReferences`.
- `config/credstore_test.go`: the store properties above, plus value-absence in every error.
- `provider/openai/factory_test.go`: `TestFromModelSecretFailuresNameOnlyTheReference` — includes
  the client build failing *after* successful resolution (`ErrNoEndpoint`) with the key still
  absent from the error; `TestFromModelResolvesSecretReferencesThroughCredentials` proves the
  resolver value reaches the `Authorization` header.
- `cmd/lcx-go/provider_test.go`: `TestSecretReferenceResolvesThroughTheCredentialFile`.
- Removing any redaction (wrapping a resolver's error text, echoing values, printing the store)
  fails these tests.

## Files

Stage 1 (`465eda7` + `42d617c`): `config/credentials.go`, `config/resolve.go`, `config/types.go`,
`config/schema_test.go`, `config/secrets_test.go`, `config/credentials_test.go`,
`config/testdata/valid/secret-ref.json`, `config/testdata/invalid/secret-ref-bad-shape.json`,
`docs/lumencortex.schema.json` (embedded copy is the same file, `TestSchemaFileIsTheEmbeddedOne`),
`provider/openai/factory.go` + `factory_test.go`, `cmd/lcx-go/provider.go` + `provider_test.go`,
`docs/security-and-trust.md`, `config/save.go` (comment).

Stage 2 (file store): `config/credstore.go` (new), `config/credstore_test.go` (new),
`config/credentials.go` (doc: the store, no Keychain), `cmd/lcx-go/provider.go` (store wiring +
preflight), `cmd/lcx-go/provider_test.go` (file-store test replaces the env-only refusal),
`docs/security-and-trust.md`, this report.

## Validation

Commands run in Core: `go build ./...`, `go vet ./...`, `go test -count=1 ./...` — all green at
each commit (the stage 2 run covers `config`, `cmd/lcx-go`, `provider/openai` focused first).
Pre-existing gofmt findings in `cmd/lcx-go/main.go`/`main_test.go` are not this task's files.

## Decisions

- No `config.EnvResolver` chain helper: nil resolver already means env-only; the CLI now uses the
  file store, and `{env:NAME}` stays the zero-dependency option.
- `Catalog()`/`ModelInfo` unchanged: catalog entries stay env-only knowledge; a host with a
  resolver decides presentation (Desktop's call).
- File-mode fail-closed, directory-mode repair: a loosened file may already have been read, so
  core refuses it (read and write) until the user chmods it back; a loosened directory is
  repaired by stripping group/other bits on write. Owner bits are never widened.
- The store file is `GlobalDir/credentials.json`, one JSON object; bounded by `maxFileBytes`
  (1 MiB) on read, values bounded to 8 KiB on write.

## Handoffs

- CoreProtocol: dispatcher already forwards `Credentials` (their `7b8c690`); no further work.
- Desktop: pass `config.NewFileCredentialStore(path)` — or better, the same default
  `config.DefaultCredentialStorePath()` — as `provider.Options.Credentials` so Desktop and the
  CLI share one store. There is no Keychain; the store is file-permission protected.
