CREATE TABLE IF NOT EXISTS schema_migrations (
    version TEXT PRIMARY KEY,
    applied_at INTEGER NOT NULL
);

CREATE TABLE requests (
    id TEXT PRIMARY KEY,
    created_at INTEGER NOT NULL
);
CREATE INDEX requests_created_at ON requests(created_at);

CREATE TABLE routing_decisions (
    request_id TEXT PRIMARY KEY REFERENCES requests(id) ON DELETE CASCADE,
    tier INTEGER NOT NULL,
    model_id TEXT NOT NULL,
    provider TEXT NOT NULL,
    reasons_json TEXT NOT NULL,
    rejections_json TEXT NOT NULL,
    replay_json TEXT NOT NULL
);
CREATE INDEX routing_decisions_model ON routing_decisions(provider, model_id);

CREATE TABLE candidate_scores (
    request_id TEXT NOT NULL REFERENCES routing_decisions(request_id) ON DELETE CASCADE,
    position INTEGER NOT NULL CHECK (position >= 0),
    model_id TEXT NOT NULL,
    provider TEXT NOT NULL,
    direct_cost REAL NOT NULL,
    failure_probability REAL NOT NULL,
    escalation_cost REAL NOT NULL,
    success_probability REAL NOT NULL,
    latency_penalty REAL NOT NULL,
    expected_total_cost REAL NOT NULL,
    latency_ns INTEGER NOT NULL,
    PRIMARY KEY (request_id, position)
);
CREATE INDEX candidate_scores_model ON candidate_scores(provider, model_id);

CREATE TABLE jev_judgments (
    request_id TEXT PRIMARY KEY REFERENCES requests(id) ON DELETE CASCADE,
    judgment_json TEXT NOT NULL
);

CREATE TABLE content_blobs (
    request_id TEXT NOT NULL REFERENCES requests(id) ON DELETE CASCADE,
    kind TEXT NOT NULL CHECK (kind IN ('prompt', 'answer', 'rejected', 'raw')),
    position INTEGER NOT NULL CHECK (position >= 0),
    key_id TEXT NOT NULL,
    version INTEGER NOT NULL CHECK (version > 0),
    nonce BLOB NOT NULL,
    ciphertext BLOB NOT NULL,
    created_at INTEGER NOT NULL,
    PRIMARY KEY (request_id, kind, position)
);
CREATE INDEX content_blobs_created_at ON content_blobs(created_at);
