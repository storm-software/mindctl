-- The usage dashboard scans attempts by time window.
CREATE INDEX provider_attempts_created_at ON provider_attempts(created_at);
