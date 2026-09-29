-- Preserve the task and feedback inputs that make a routing decision replayable.
ALTER TABLE routing_decisions ADD COLUMN task_type TEXT NOT NULL DEFAULT 'unknown';

ALTER TABLE candidate_scores ADD COLUMN configured_success_probability REAL NOT NULL DEFAULT 0;
ALTER TABLE candidate_scores ADD COLUMN feedback_thumbs_up_count INTEGER NOT NULL DEFAULT 0;
ALTER TABLE candidate_scores ADD COLUMN feedback_rating_count INTEGER NOT NULL DEFAULT 0;
ALTER TABLE candidate_scores ADD COLUMN feedback_applied INTEGER NOT NULL DEFAULT 0;
