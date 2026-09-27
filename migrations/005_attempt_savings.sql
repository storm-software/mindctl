-- Content-free savings telemetry for successful provider attempts. Rows outlive
-- content retention so cumulative savings stay reportable. Costs are USD, and
-- savings are derived at query time from the stored costs.
CREATE TABLE attempt_savings (
    attempt_id TEXT PRIMARY KEY REFERENCES provider_attempts(id) ON DELETE CASCADE,
    created_at INTEGER NOT NULL,
    input_tokens INTEGER NOT NULL CHECK (input_tokens >= 0),
    cached_input_tokens INTEGER NOT NULL CHECK (cached_input_tokens >= 0),
    output_tokens INTEGER NOT NULL CHECK (output_tokens >= 0),
    actual_cost REAL NOT NULL CHECK (actual_cost >= 0),
    baseline_provider TEXT NOT NULL,
    baseline_model_id TEXT NOT NULL,
    baseline_cost REAL NOT NULL CHECK (baseline_cost >= 0),
    compression_tokens_before INTEGER NOT NULL CHECK (compression_tokens_before >= 0),
    compression_tokens_saved INTEGER NOT NULL CHECK (compression_tokens_saved >= 0),
    compression_savings REAL NOT NULL CHECK (compression_savings >= 0)
);
CREATE INDEX attempt_savings_created_at ON attempt_savings(created_at);
