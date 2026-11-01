CREATE TABLE items (
    tenant    TEXT NOT NULL,
    agent     TEXT NOT NULL,
    project   TEXT NOT NULL,
    usr       TEXT NOT NULL,
    kind      TEXT NOT NULL,
    ref_id    TEXT NOT NULL,
    text      TEXT NOT NULL,
    embedding BLOB,
    PRIMARY KEY (tenant, agent, project, usr, kind, ref_id)
);

-- Full-text index. The scope columns are stored but not indexed, and every
-- query filters on them, so one scope's words never match another scope's rows.
CREATE VIRTUAL TABLE items_fts USING fts5(
    text,
    tenant  UNINDEXED,
    agent   UNINDEXED,
    project UNINDEXED,
    usr     UNINDEXED,
    kind    UNINDEXED,
    ref_id  UNINDEXED
);
