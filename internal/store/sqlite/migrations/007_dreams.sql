CREATE TABLE dream_runs (
    tenant     TEXT NOT NULL,
    agent      TEXT NOT NULL,
    project    TEXT NOT NULL,
    usr        TEXT NOT NULL,
    id         TEXT NOT NULL,
    trigger    TEXT NOT NULL,
    status     TEXT NOT NULL,            -- running | done | failed | aborted
    started_at TEXT NOT NULL,
    ended_at   TEXT NOT NULL DEFAULT '',
    budget     INTEGER NOT NULL DEFAULT 0,
    spent      INTEGER NOT NULL DEFAULT 0,
    report     BLOB,
    PRIMARY KEY (tenant, agent, project, usr, id)
);

-- One row per scope: whoever holds an unexpired lease is the only dream for it.
CREATE TABLE dream_leases (
    tenant     TEXT NOT NULL,
    agent      TEXT NOT NULL,
    project    TEXT NOT NULL,
    usr        TEXT NOT NULL,
    run_id     TEXT NOT NULL,
    expires_at TEXT NOT NULL,
    PRIMARY KEY (tenant, agent, project, usr)
);
