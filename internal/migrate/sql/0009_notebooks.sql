-- 0009_notebooks.sql: the agent's persistent working notebook (ADR-0020). A notebook is a Markdown
-- document the agent maintains for itself: Plan, Checklist, Findings, Experiments, Decisions, Failures,
-- Verification and Handoff sections. It is external working memory, not Task State: nothing in the
-- runtime reads it to decide whether a run may finish, and no state transition depends on it.
--
-- A notebook belongs to one session (a subagent's session has its own notebooks; the parent reads them
-- through the session relation). title is unique per session, so the model can name a notebook the way
-- the context index lists it. version counts writes: an update is one UPDATE guarded by the version it
-- read (compare-and-set), so two writers never both win. body is content: it is deleted with its
-- session (ON DELETE CASCADE and session purgeTables).
--
-- notebook_revisions keeps the last 50 bodies of each notebook (older ones are pruned in the same
-- transaction as the write). op names how the version came to be: create, replace, append, section.
CREATE TABLE notebooks (
  id TEXT PRIMARY KEY CHECK (length(id) BETWEEN 1 AND 64),
  session_id TEXT NOT NULL,
  title TEXT NOT NULL CHECK (length(title) BETWEEN 1 AND 120),
  body TEXT NOT NULL CHECK (length(body) <= 262144),
  -- bytes and headings_json are derived from body at every write: a list (and the run-start index)
  -- then reads metadata only and never loads a body.
  bytes INTEGER NOT NULL CHECK (bytes >= 0),
  headings_json TEXT NOT NULL CHECK (json_valid(headings_json) AND json_type(headings_json) = 'array'),
  version INTEGER NOT NULL CHECK (version >= 1),
  created_at TEXT NOT NULL,
  updated_at TEXT NOT NULL,
  updated_by TEXT NOT NULL CHECK (length(updated_by) BETWEEN 1 AND 80),
  UNIQUE(session_id, title),
  FOREIGN KEY (session_id) REFERENCES sessions(id) ON DELETE CASCADE
) STRICT;
CREATE INDEX idx_notebooks_session ON notebooks(session_id, updated_at DESC);

CREATE TABLE notebook_revisions (
  notebook_id TEXT NOT NULL,
  version INTEGER NOT NULL CHECK (version >= 1),
  body TEXT NOT NULL CHECK (length(body) <= 262144),
  op TEXT NOT NULL CHECK (op IN ('create','replace','append','section')),
  updated_by TEXT NOT NULL CHECK (length(updated_by) BETWEEN 1 AND 80),
  created_at TEXT NOT NULL,
  PRIMARY KEY (notebook_id, version),
  FOREIGN KEY (notebook_id) REFERENCES notebooks(id) ON DELETE CASCADE
) STRICT;
