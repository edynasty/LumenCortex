# ADR-0016: External state, workspace binding and the protected-path guard

- **Status:** Accepted
- **Date:** 2026-10-25
- **Owners:** project owner
- **Supersedes:** the rule that a workspace's database lives in `<workspace>/.lumencortex` (data-model.md "SQLite persistence")
- **Superseded by:** none

## Context

The state database, its locks, run locks and backups lived in `<workspace>/.lumencortex`. Three
problems follow from that placement:

1. **Repository-carried state (SECURITY-002).** A repository can commit `.lumencortex/lumencortex.db`.
   Core opened it without asking where it came from, so a clone could ship history, Task State and
   approvals that this machine never produced.
2. **Two authorities.** A host that computes its own state path (Desktop passing `repoDir`) and a CLI
   that uses the default can open two databases for one workspace; the run locks that serialize
   work then protect nothing.
3. **State inside the file tools' reach.** `write_file` and `apply_patch` work anywhere in the
   workspace, which includes the database.

The cognition design (T0-S1 of the engineering plan) fixes all three before any new table is added.

## Decisions

### D1 — Core decides where state lives

State directory of a workspace, in order:

1. `Options.RepoDir`, when set (tests and self-checks). It is bound and checked like any other.
2. The **external** directory, when it holds `lumencortex.db`.
3. `<workspace>/.lumencortex`, when it holds `lumencortex.db` and the external one does not:
   **legacy** mode, opened in place, nothing moved or copied.
4. Otherwise the external directory, created on Open (`0700`).

External directory: `<root>/<name>-<hash>` where `<root>` is
`$XDG_STATE_HOME/lumencortex/workspaces` when `XDG_STATE_HOME` is an absolute path and
`~/.local/state/lumencortex/workspaces` otherwise (the convention `config.GlobalDir` uses for
`XDG_CONFIG_HOME`), `<name>` is the sanitized final component of the canonical path (letters, digits,
`.`, `-`, `_`; at most 40 characters; `workspace` when nothing is left) and `<hash>` the first 16 hex
digits of the SHA-256 of the canonical path.

The **canonical path** is the absolute path with symbolic links resolved and every component spelled
as the file system stores it. Aliases through `..`, symbolic links, case variants and Unicode NFC/NFD
variants therefore name one workspace and one state. Moving or renaming a workspace makes it a new
workspace; the old state is left where it is.

`Engine.StateInfo()` reports `Mode` (`external` or `legacy`), `Path`, the bound `Workspace`, and
`LegacyResidue` (the legacy directory, when a legacy database still exists beside an external state
in use). Hosts and the CLI do not pass `RepoDir`.

### D2 — The state database is bound to its workspace

After the schema migration, `metadata.workspace_binding` holds `{canonicalPath, createdAt,
coreVersion}`. No numbered migration is needed: `metadata` exists since schema 1.

- Binding for another workspace, or an unreadable row: `ErrStateBindingMismatch`. The row is not
  rewritten and no new database replaces the refused one.
- No row: a database that was at schema **≤ 5** before this open predates the rule and is adopted
  (the row is written); one at schema **≥ 6** is refused with `ErrStateBindingMismatch`, because
  every database this Core creates from schema 6 on carries a binding, so a missing one means a
  constructed file.
- `bind.lock` (`<state>/bind.lock`, exclusive) is held from before the migration until the binding is
  verified. Two processes that open a new workspace together therefore get one database and one
  binding; the writer reads the row back after `INSERT … ON CONFLICT DO NOTHING`.

### D3 — Repository-carried databases are refused

A legacy or `RepoDir` database that `git ls-files --error-unmatch` reports as tracked is refused with
`ErrRepositoryCarriedState` before anything is opened. Outside any repository, or without `git`, it
is untracked. A repository in which the probe fails for any other reason refuses the open: the file
is neither adopted nor assumed safe. When the external state exists the legacy file is not opened and
is not probed.

### D4 — Configuration is not state

`mcp.json`, `skills/` and `cognition.json` are read from `<workspace>/.lumencortex/` in every mode.
Only the database, its locks, run locks and backups moved.

### D5 — Protected-path guard for the file tools

`read_file`, `list_dir`, `write_file`, `replace_in_file` and `apply_patch` refuse
(`toolset.ErrProtectedPath`) the external state tree (and an explicit `RepoDir`) and, in
`<workspace>/.lumencortex`, the state files: `lumencortex.db*`, `db.lock`, `bind.lock`, `runs/`,
`backups/`, `checkpoint.pending*`, `legacy-state-*`. The legacy files stay protected after the external
state took over. Project configuration beside them is not protected.

The guard resolves the real path, compares names against those roots, then compares **file identity**
(`os.SameFile`: device and inode, or the Windows file index) of the existing part of the path and, after
the open, of the descriptor. Identity covers hard links, case and normalization spellings the file
system folds, and a path swapped between validation and open. A write opens without truncating and
truncates only after the identity check, so a refused write changes nothing. `apply_patch` validates a
delete again immediately before removing.

## Not provided

**This is not a sandbox.** `shell` runs with the user's permissions and can open the database, change
the environment, or create links with any name. The guard narrows what the file tools can do; it does
not isolate the process. OS-level isolation is outside this decision. The guard also leaves
`search_text`, `find_files`, the git tools and the `lsp_*` tools (`lsp_rename` writes files) unguarded, and does not close a swap of a parent
directory for the final `os.Remove` of a patch delete.

## Alternatives considered

- **Keep `.lumencortex` and reject tracked files only.** Leaves the two-authority problem and the file
  tools' reach unchanged.
- **Hash only the path string.** Case and normalization aliases and symlinks would create several
  states for one directory.
- **A numbered migration for the binding.** Not needed; it would add a migration owner to the first
  slice of the plan.
- **Migrate legacy databases on first open.** Needs an owner decision on retention (O-2) and a
  recoverable procedure; it is a separate slice. Legacy databases are opened in place until then.

## Consequences

### Positive

- One state per workspace across Desktop, CLI and tests; a clone cannot bring state.
- File tools cannot damage or read the database.

### Negative

- Moving or renaming a workspace starts with an empty history. A legacy workspace that moves with its
  `.lumencortex` directory is refused by the binding until it is rebound; there is no rebinding
  command yet.
- A crash between the migration that creates schema 6 and the binding write leaves a database that
  the ≥ 6 rule refuses; the window is the time of one `INSERT`, and the recovery is to delete that
  new, empty external directory.
- Every Open takes `bind.lock` and, for a legacy or `RepoDir` database inside a repository, runs one
  `git` process.

## Verification

`runtime/state_test.go`, `runtime/statedir_test.go`, `internal/session/binding_test.go`,
`internal/toolset/guard_test.go`; slice report `docs/reports/cognition/t0-s1-state-20261025.md`.
