-- Skills are shared by a (tenant, project); agent = '' means every agent in
-- the project may use it, otherwise it is narrowed to that agent.
CREATE TABLE skills (
    tenant      TEXT NOT NULL,
    project     TEXT NOT NULL,
    id          TEXT NOT NULL,
    version     INTEGER NOT NULL,
    agent       TEXT NOT NULL DEFAULT '',
    name        TEXT NOT NULL,
    trigger     TEXT NOT NULL DEFAULT '',
    steps       TEXT NOT NULL DEFAULT '[]',
    status      TEXT NOT NULL,             -- candidate | active | deprecated
    uses        INTEGER NOT NULL DEFAULT 0,
    successes   INTEGER NOT NULL DEFAULT 0,
    episode_ids TEXT NOT NULL DEFAULT '[]',
    created_at  TEXT NOT NULL,
    updated_at  TEXT NOT NULL,
    last_used   TEXT NOT NULL DEFAULT '',
    PRIMARY KEY (tenant, project, id, version)
);
CREATE INDEX skills_status ON skills (tenant, project, status, updated_at);

-- Speculative connections from Recombine. Never facts; surfaced only once confirmed.
CREATE TABLE insights (
    tenant       TEXT NOT NULL,
    agent        TEXT NOT NULL,
    project      TEXT NOT NULL,
    usr          TEXT NOT NULL,
    id           TEXT NOT NULL,
    body         TEXT NOT NULL,
    entities     TEXT NOT NULL DEFAULT '[]',
    confidence   REAL NOT NULL,
    status       TEXT NOT NULL,            -- speculative | confirmed | rejected
    episode_ids  TEXT NOT NULL DEFAULT '[]',
    confirmed_by TEXT NOT NULL DEFAULT '[]',
    created_at   TEXT NOT NULL,
    confirmed_at TEXT NOT NULL DEFAULT '',
    PRIMARY KEY (tenant, agent, project, usr, id)
);
CREATE INDEX insights_status ON insights (tenant, agent, project, usr, status, created_at);
