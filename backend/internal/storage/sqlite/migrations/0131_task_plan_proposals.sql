-- A planner proposal is durable review material, not an executable task plan.
-- Only the explicit accept transaction copies its validated graph into
-- task_plan, where Phase 3 scheduling can see it.
-- +goose Up
-- +goose StatementBegin
CREATE TABLE task_plan_proposal (
    id              TEXT PRIMARY KEY NOT NULL,
    project_id      TEXT NOT NULL REFERENCES projects(id) ON DELETE CASCADE,
    request_key     TEXT NOT NULL,
    specification   TEXT NOT NULL,
    status          TEXT NOT NULL CHECK (status IN (
                        'queued', 'generating', 'ready', 'invalid', 'failed',
                        'accepted', 'rejected'
                    )),
    orchestrator_id TEXT NOT NULL DEFAULT '',
    turn_id         TEXT NOT NULL DEFAULT '',
    graph_json      TEXT NOT NULL DEFAULT '',
    error_code      TEXT NOT NULL DEFAULT '',
    error_message   TEXT NOT NULL DEFAULT '',
    accepted_at     TEXT,
    rejected_at     TEXT,
    created_at      TEXT NOT NULL,
    updated_at      TEXT NOT NULL,
    UNIQUE (project_id, request_key)
);
-- +goose StatementEnd

-- +goose StatementBegin
CREATE INDEX task_plan_proposal_project_page
    ON task_plan_proposal(project_id, created_at DESC, id);
-- +goose StatementEnd

-- +goose StatementBegin
CREATE INDEX task_plan_proposal_pending
    ON task_plan_proposal(status, created_at, id)
    WHERE status IN ('queued', 'generating');
-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
DROP TABLE task_plan_proposal;
-- +goose StatementEnd
