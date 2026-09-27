-- Anthropic cache-creation input tokens are priced at the model's cache-write
-- rate. Rows recorded before this migration did not track them.
ALTER TABLE attempt_savings
    ADD COLUMN cache_write_input_tokens INTEGER NOT NULL DEFAULT 0 CHECK (cache_write_input_tokens >= 0);
