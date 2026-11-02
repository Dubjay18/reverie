CREATE TABLE reverie_meta (
    key   TEXT PRIMARY KEY,
    value TEXT NOT NULL
);

CREATE TABLE events (
    id         TEXT NOT NULL,
    tenant     TEXT NOT NULL,
    agent      TEXT NOT NULL,
    project    TEXT NOT NULL,
    usr        TEXT NOT NULL,
    task_id    TEXT NOT NULL,
    kind       TEXT NOT NULL,
    origin     TEXT NOT NULL,
    at         TEXT NOT NULL,
    payload    BLOB,
    token_cost INTEGER NOT NULL DEFAULT 0,
    PRIMARY KEY (tenant, agent, project, usr, id)
);
CREATE INDEX events_scope_task ON events (tenant, agent, project, usr, task_id, id);

CREATE TABLE episodes (
    tenant       TEXT NOT NULL,
    agent        TEXT NOT NULL,
    project      TEXT NOT NULL,
    usr          TEXT NOT NULL,
    task_id      TEXT NOT NULL,
    title        TEXT NOT NULL DEFAULT '',
    first_event  TEXT NOT NULL,
    last_event   TEXT NOT NULL,
    started_at   TEXT NOT NULL,
    ended_at     TEXT NOT NULL,
    closed       INTEGER NOT NULL DEFAULT 0,
    outcome      TEXT NOT NULL DEFAULT '',
    events       INTEGER NOT NULL DEFAULT 0,
    tool_calls   INTEGER NOT NULL DEFAULT 0,
    errors       INTEGER NOT NULL DEFAULT 0,
    feedback     INTEGER NOT NULL DEFAULT 0,
    implicit     INTEGER NOT NULL DEFAULT 0,
    importance   REAL NOT NULL DEFAULT 0.5,
    tier         TEXT NOT NULL DEFAULT 'hot',
    consolidated INTEGER NOT NULL DEFAULT 0,
    PRIMARY KEY (tenant, agent, project, usr, task_id)
);
CREATE INDEX episodes_scope_closed ON episodes (tenant, agent, project, usr, closed, ended_at);
