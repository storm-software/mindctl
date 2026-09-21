CREATE TABLE conversations (
    id TEXT PRIMARY KEY,
    client_id TEXT NOT NULL,
    created_at INTEGER NOT NULL,
    pin_provider TEXT,
    pin_model_id TEXT,
    pin_tier INTEGER,
    escalation_floor INTEGER NOT NULL CHECK (escalation_floor >= 0 AND escalation_floor <= 6)
);
CREATE INDEX conversations_client_created ON conversations(client_id, created_at);

CREATE TABLE responses (
    id TEXT PRIMARY KEY,
    conversation_id TEXT NOT NULL REFERENCES conversations(id) ON DELETE CASCADE,
    sequence INTEGER NOT NULL CHECK (sequence >= 0),
    created_at INTEGER NOT NULL,
    status TEXT NOT NULL CHECK (status IN ('pending', 'completed')),
    UNIQUE(conversation_id, sequence)
);
CREATE INDEX responses_conversation_created ON responses(conversation_id, created_at);

CREATE TABLE transcript_items (
    response_id TEXT NOT NULL REFERENCES responses(id) ON DELETE CASCADE,
    position INTEGER NOT NULL CHECK (position >= 0),
    provider TEXT NOT NULL,
    key_id TEXT NOT NULL,
    version INTEGER NOT NULL CHECK (version > 0),
    nonce BLOB NOT NULL,
    ciphertext BLOB NOT NULL,
    created_at INTEGER NOT NULL,
    PRIMARY KEY (response_id, position)
);

CREATE TABLE provider_attempts (
    id TEXT PRIMARY KEY,
    response_id TEXT NOT NULL REFERENCES responses(id) ON DELETE CASCADE,
    sequence INTEGER NOT NULL CHECK (sequence >= 0),
    provider TEXT NOT NULL,
    model_id TEXT NOT NULL,
    tier INTEGER NOT NULL CHECK (tier >= 0 AND tier <= 6),
    decision_json TEXT NOT NULL,
    status TEXT NOT NULL CHECK (status IN ('started', 'succeeded', 'failed')),
    provider_request_id TEXT NOT NULL DEFAULT '',
    key_id TEXT,
    version INTEGER,
    nonce BLOB,
    ciphertext BLOB,
    created_at INTEGER NOT NULL,
    completed_at INTEGER,
    UNIQUE(response_id, sequence)
);
CREATE INDEX provider_attempts_response ON provider_attempts(response_id, sequence);
