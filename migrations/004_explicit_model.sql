-- NULL marks responses recorded before model selection was tracked.
ALTER TABLE responses ADD COLUMN explicit_model INTEGER CHECK (explicit_model IN (0, 1));
