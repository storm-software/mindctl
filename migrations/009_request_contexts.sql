-- Request-level content sent ahead of the transcript: instructions (including
-- Anthropic system text) and tool schemas. Clients resend it unchanged on every
-- turn, so it is stored once per keyed digest. created_at is refreshed on
-- reuse, so retention keeps contexts that are still being sent.
CREATE TABLE request_contexts (
    digest TEXT PRIMARY KEY,
    key_id TEXT NOT NULL,
    version INTEGER NOT NULL CHECK (version > 0),
    nonce BLOB NOT NULL,
    ciphertext BLOB NOT NULL,
    created_at INTEGER NOT NULL
);
CREATE INDEX request_contexts_created_at ON request_contexts(created_at);

-- NULL marks responses recorded without a request context.
ALTER TABLE responses ADD COLUMN context_digest TEXT;
