CREATE TABLE facts (
    tenant        TEXT NOT NULL,
    agent         TEXT NOT NULL,
    project       TEXT NOT NULL,
    usr           TEXT NOT NULL,
    id            TEXT NOT NULL,
    entity        TEXT NOT NULL,
    entity_type   TEXT NOT NULL DEFAULT '',
    relation      TEXT NOT NULL,
    value         TEXT NOT NULL,
    confidence    REAL NOT NULL,
    source        TEXT NOT NULL,
    status        TEXT NOT NULL,
    pinned        INTEGER NOT NULL DEFAULT 0,
    valid_from    TEXT NOT NULL,
    valid_to      TEXT NOT NULL DEFAULT '',
    recorded_at   TEXT NOT NULL,
    superseded_by TEXT NOT NULL DEFAULT '',
    PRIMARY KEY (tenant, agent, project, usr, id)
);
-- the conflict-detection lookup used by every Assert
CREATE INDEX facts_key ON facts (tenant, agent, project, usr, entity, relation, status);

CREATE TABLE fact_sources (
    tenant     TEXT NOT NULL,
    agent      TEXT NOT NULL,
    project    TEXT NOT NULL,
    usr        TEXT NOT NULL,
    fact_id    TEXT NOT NULL,
    episode_id TEXT NOT NULL,
    PRIMARY KEY (tenant, agent, project, usr, fact_id, episode_id)
);

-- Every change to long-term memory leaves a row here with what it replaced,
-- which is what makes a change undoable. Dream runs reuse it (run_id).
CREATE TABLE journal (
    id              INTEGER PRIMARY KEY AUTOINCREMENT,
    tenant          TEXT NOT NULL,
    agent           TEXT NOT NULL,
    project         TEXT NOT NULL,
    usr             TEXT NOT NULL,
    run_id          TEXT NOT NULL DEFAULT '',
    phase           TEXT NOT NULL DEFAULT '',
    op              TEXT NOT NULL,
    target_kind     TEXT NOT NULL,
    target_id       TEXT NOT NULL,
    before          BLOB,
    after           BLOB,
    rationale       TEXT NOT NULL DEFAULT '',
    source_episodes TEXT NOT NULL DEFAULT '[]',
    tokens          INTEGER NOT NULL DEFAULT 0,
    at              TEXT NOT NULL,
    reverted        INTEGER NOT NULL DEFAULT 0
);
CREATE INDEX journal_scope_run ON journal (tenant, agent, project, usr, run_id, id);
