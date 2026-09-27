-- Content-free routing affinity for stateless clients such as Claude Code.
-- session_key is a digest; raw client session IDs and prompt text are never
-- stored. Pin and floor follow the conversation ratchet rules.
CREATE TABLE routing_sessions (
    client_id TEXT NOT NULL,
    session_key TEXT NOT NULL,
    created_at INTEGER NOT NULL,
    updated_at INTEGER NOT NULL,
    pin_provider TEXT,
    pin_model_id TEXT,
    pin_tier INTEGER CHECK (pin_tier IS NULL OR (pin_tier >= 0 AND pin_tier <= 6)),
    escalation_floor INTEGER NOT NULL CHECK (escalation_floor >= 0 AND escalation_floor <= 6),
    last_usage_provider TEXT NOT NULL DEFAULT '',
    last_uncached_input_tokens INTEGER NOT NULL DEFAULT 0 CHECK (last_uncached_input_tokens >= 0),
    last_cache_read_tokens INTEGER NOT NULL DEFAULT 0 CHECK (last_cache_read_tokens >= 0),
    last_cache_write_tokens INTEGER NOT NULL DEFAULT 0 CHECK (last_cache_write_tokens >= 0),
    last_output_tokens INTEGER NOT NULL DEFAULT 0 CHECK (last_output_tokens >= 0),
    PRIMARY KEY (client_id, session_key)
);
CREATE INDEX routing_sessions_updated_at ON routing_sessions(updated_at);

-- NULL marks conversations that are not bound to a routing session.
ALTER TABLE conversations ADD COLUMN session_key TEXT;

-- Candidate scores gain the expected-output and session-horizon estimates.
-- Earlier scores ranked by the worst case, so it is their expected cost.
ALTER TABLE candidate_scores ADD COLUMN expected_turn_cost REAL;
UPDATE candidate_scores SET expected_turn_cost = direct_cost;
ALTER TABLE candidate_scores ADD COLUMN horizon_cost REAL NOT NULL DEFAULT 0;
