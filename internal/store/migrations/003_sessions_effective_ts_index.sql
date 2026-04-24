-- Expression index on the "effective" session timestamp used by the
-- time-window queries in events.go (LoadRecentSessionDigests) and the
-- MCP list_sessions tool. Both filter and ORDER BY the same COALESCE
-- expression; without an index on it SQLite scans sessions every time.
-- Acceptable at tens of sessions; starts to hurt past a few thousand.

CREATE INDEX idx_sessions_effective_ts
    ON sessions(COALESCE(ended_at_ms, started_at_ms, 0) DESC);

UPDATE meta SET value='3' WHERE key='schema_version';
