-- Summary: record workspace isolation and harness selection on tasks, and the
-- launcher identity on attempts, so the ready queue can claim work without
-- storing a derived "ready" state.
-- +goose Up
-- +goose StatementBegin
ALTER TABLE task ADD COLUMN workspace_key TEXT NOT NULL DEFAULT '';
-- +goose StatementEnd

-- +goose StatementBegin
ALTER TABLE task ADD COLUMN harness TEXT NOT NULL DEFAULT '';
-- +goose StatementEnd

-- +goose StatementBegin
ALTER TABLE task_attempt ADD COLUMN runtime_ref TEXT NOT NULL DEFAULT '';
-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
ALTER TABLE task_attempt DROP COLUMN runtime_ref;
-- +goose StatementEnd

-- +goose StatementBegin
ALTER TABLE task DROP COLUMN harness;
-- +goose StatementEnd

-- +goose StatementBegin
ALTER TABLE task DROP COLUMN workspace_key;
-- +goose StatementEnd
