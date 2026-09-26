-- Phase 4 automation: one durable cursor over task-result events, one
-- follow-up turn per event, task-linked handoffs, human gates, and retry
-- decisions. Gates are the durable notification; this migration does not
-- widen change_log.
-- +goose Up
-- +goose StatementBegin
CREATE TABLE planner_event_cursor (
    consumer   TEXT PRIMARY KEY,
    last_seq   INTEGER NOT NULL CHECK (last_seq >= 0),
    updated_at TEXT NOT NULL
);
-- +goose StatementEnd

-- +goose StatementBegin
CREATE TABLE planner_followup (
    id              TEXT PRIMARY KEY,
    project_id      TEXT NOT NULL REFERENCES projects (id) ON DELETE CASCADE,
    plan_id         TEXT NOT NULL,
    task_id         TEXT NOT NULL,
    attempt_id      TEXT NOT NULL,
    source_seq      INTEGER NOT NULL UNIQUE,
    state           TEXT NOT NULL CHECK (state IN ('queued', 'running', 'completed', 'failed', 'skipped')),
    turn_id         TEXT NOT NULL DEFAULT '',
    orchestrator_id TEXT NOT NULL DEFAULT '',
    error_code      TEXT NOT NULL DEFAULT '',
    summary         TEXT NOT NULL DEFAULT '',
    created_at      TEXT NOT NULL,
    updated_at      TEXT NOT NULL,
    CHECK (length(CAST(summary AS BLOB)) <= 4096)
);
-- +goose StatementEnd

-- +goose StatementBegin
CREATE INDEX planner_followup_pending
    ON planner_followup (state, source_seq, id)
    WHERE state IN ('queued', 'running');
-- +goose StatementEnd

-- +goose StatementBegin
CREATE TABLE task_handoff (
    id         TEXT PRIMARY KEY,
    project_id TEXT NOT NULL REFERENCES projects (id) ON DELETE CASCADE,
    plan_id    TEXT NOT NULL,
    task_id    TEXT NOT NULL,
    attempt_id TEXT NOT NULL,
    summary    TEXT NOT NULL,
    created_at TEXT NOT NULL,
    FOREIGN KEY (plan_id, task_id) REFERENCES task (plan_id, id) ON DELETE CASCADE,
    CHECK (length(CAST(summary AS BLOB)) BETWEEN 1 AND 4096)
);
-- +goose StatementEnd

-- +goose StatementBegin
CREATE INDEX task_handoff_task ON task_handoff (plan_id, task_id, created_at, id);
-- +goose StatementEnd

-- +goose StatementBegin
CREATE TABLE task_retry_policy (
    project_id       TEXT PRIMARY KEY REFERENCES projects (id) ON DELETE CASCADE,
    max_attempts     INTEGER NOT NULL CHECK (max_attempts BETWEEN 1 AND 8),
    fallback_harness TEXT NOT NULL DEFAULT '',
    updated_at       TEXT NOT NULL
);
-- +goose StatementEnd

-- +goose StatementBegin
CREATE TABLE task_retry_decision (
    id         TEXT PRIMARY KEY,
    project_id TEXT NOT NULL,
    plan_id    TEXT NOT NULL,
    task_id    TEXT NOT NULL,
    attempt_id TEXT NOT NULL UNIQUE,
    action     TEXT NOT NULL CHECK (action IN ('retry', 'fallback', 'escalate', 'hold', 'none')),
    harness    TEXT NOT NULL DEFAULT '',
    reason     TEXT NOT NULL DEFAULT '',
    created_at TEXT NOT NULL
);
-- +goose StatementEnd

-- +goose StatementBegin
CREATE TABLE task_human_gate (
    id          TEXT PRIMARY KEY,
    project_id  TEXT NOT NULL REFERENCES projects (id) ON DELETE CASCADE,
    plan_id     TEXT NOT NULL,
    task_id     TEXT NOT NULL,
    attempt_id  TEXT NOT NULL DEFAULT '',
    request_key TEXT NOT NULL,
    state       TEXT NOT NULL CHECK (state IN ('pending', 'approved', 'rejected')),
    summary     TEXT NOT NULL,
    created_at  TEXT NOT NULL,
    updated_at  TEXT NOT NULL,
    resolved_at TEXT,
    UNIQUE (project_id, request_key),
    FOREIGN KEY (plan_id, task_id) REFERENCES task (plan_id, id) ON DELETE CASCADE,
    CHECK (length(CAST(summary AS BLOB)) BETWEEN 1 AND 4096)
);
-- +goose StatementEnd

-- +goose StatementBegin
CREATE UNIQUE INDEX task_human_gate_pending
    ON task_human_gate (plan_id, task_id)
    WHERE state = 'pending';
-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
DROP INDEX IF EXISTS task_human_gate_pending;
DROP TABLE IF EXISTS task_human_gate;
DROP TABLE IF EXISTS task_retry_decision;
DROP TABLE IF EXISTS task_retry_policy;
DROP INDEX IF EXISTS task_handoff_task;
DROP TABLE IF EXISTS task_handoff;
DROP INDEX IF EXISTS planner_followup_pending;
DROP TABLE IF EXISTS planner_followup;
DROP TABLE IF EXISTS planner_event_cursor;
-- +goose StatementEnd
