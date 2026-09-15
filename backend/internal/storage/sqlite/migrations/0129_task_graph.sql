-- +goose Up
-- Widen change_log for the task-graph lifecycle vocabulary. SQLite cannot ALTER a
-- CHECK constraint, so this is the 12-step rebuild (as in 0004/0006/0103): drop
-- every trigger that writes change_log, swap the table, replay the rows, then
-- recreate all triggers plus the six new task-graph capture triggers.
-- +goose StatementBegin
DROP TRIGGER IF EXISTS agent_switches_cdc_insert;
DROP TRIGGER IF EXISTS agent_switches_cdc_update;
DROP TRIGGER IF EXISTS agent_switches_failed_recovery_marker_insert;
DROP TRIGGER IF EXISTS agent_switches_failed_recovery_marker_update;
DROP TRIGGER IF EXISTS agent_switches_target_native_scope_insert;
DROP TRIGGER IF EXISTS agent_switches_target_native_scope_update;
DROP TRIGGER IF EXISTS codex_account_switch_sessions_cdc;
DROP TRIGGER IF EXISTS codex_account_switch_sessions_cdc_insert;
DROP TRIGGER IF EXISTS codex_account_switches_cdc_insert;
DROP TRIGGER IF EXISTS codex_account_switches_cdc_update;
DROP TRIGGER IF EXISTS conversation_activities_branch_insert;
DROP TRIGGER IF EXISTS conversation_activities_cdc_insert;
DROP TRIGGER IF EXISTS conversation_activities_cdc_update;
DROP TRIGGER IF EXISTS conversation_branch_root_provider_update;
DROP TRIGGER IF EXISTS conversation_messages_branch_insert;
DROP TRIGGER IF EXISTS conversation_messages_cdc_insert;
DROP TRIGGER IF EXISTS conversation_messages_cdc_update;
DROP TRIGGER IF EXISTS conversation_provider_events_branch_insert;
DROP TRIGGER IF EXISTS conversation_turns_branch_insert;
DROP TRIGGER IF EXISTS conversation_turns_cdc_update;
DROP TRIGGER IF EXISTS pr_cdc_insert;
DROP TRIGGER IF EXISTS pr_cdc_update;
DROP TRIGGER IF EXISTS pr_checks_cdc_insert;
DROP TRIGGER IF EXISTS pr_checks_cdc_update;
DROP TRIGGER IF EXISTS pr_review_threads_cdc_insert;
DROP TRIGGER IF EXISTS pr_review_threads_cdc_update;
DROP TRIGGER IF EXISTS pr_session_cdc_update;
DROP TRIGGER IF EXISTS review_run_cdc_insert;
DROP TRIGGER IF EXISTS review_run_cdc_update;
DROP TRIGGER IF EXISTS session_cleanup_facts_cdc_insert;
DROP TRIGGER IF EXISTS session_cleanup_facts_cdc_update;
DROP TRIGGER IF EXISTS session_interface_transitions_cdc_insert;
DROP TRIGGER IF EXISTS session_interface_transitions_cdc_update;
DROP TRIGGER IF EXISTS sessions_cdc_insert;
DROP TRIGGER IF EXISTS sessions_cdc_update;
DROP TRIGGER IF EXISTS usage_bindings_cdc_insert;
DROP TRIGGER IF EXISTS usage_bindings_cdc_update;
DROP TRIGGER IF EXISTS usage_sources_cdc_update;

DROP TRIGGER IF EXISTS change_log_old_insert;
DROP VIEW IF EXISTS change_log_old;

CREATE TABLE change_log_new (
    seq        INTEGER PRIMARY KEY AUTOINCREMENT,
    project_id TEXT NOT NULL REFERENCES projects (id),
    session_id TEXT REFERENCES sessions (id),
    event_type TEXT NOT NULL
        CHECK (event_type IN (
            'session_created',
            'session_updated',
            'pr_created',
            'pr_updated',
            'pr_check_recorded',
            'pr_session_changed',
            'pr_review_thread_added',
            'pr_review_thread_resolved',
            'review_run_created',
            'review_run_updated',
            'task_plan_created',
            'task_created',
            'task_updated',
            'task_attempt_created',
            'task_attempt_updated',
            'task_result_recorded'
        )),
    payload    TEXT NOT NULL CHECK (json_valid(payload)),
    created_at TIMESTAMP NOT NULL DEFAULT (datetime('now'))
);

INSERT INTO change_log_new (seq, project_id, session_id, event_type, payload, created_at)
SELECT seq, project_id, session_id, event_type, payload, created_at
FROM change_log;

DROP INDEX IF EXISTS idx_change_log_project;
DROP TABLE change_log;
ALTER TABLE change_log_new RENAME TO change_log;
CREATE INDEX idx_change_log_project ON change_log (project_id, seq);
-- +goose StatementEnd

-- +goose StatementBegin
-- Task-graph tables. Slice 2 of the durable task graph (see
-- docs/plans/phase-3-durable-task-graph.md): persistence only. No HTTP surface,
-- no dispatch, no scheduler - readiness is DERIVED from verified results, so no
-- "ready" state is stored (see AGENTS.md "do not store derived status").
--
-- Ordering: the domain contract (domain.TaskPlan) carries order in slice
-- position, so every collection table stores an explicit `position` instead of
-- relying on input order or rowid.

CREATE TABLE task_plan (
    id         TEXT PRIMARY KEY,
    project_id TEXT NOT NULL REFERENCES projects (id) ON DELETE CASCADE,
    title      TEXT NOT NULL CHECK (length(trim(title)) > 0),
    created_at TIMESTAMP NOT NULL,
    updated_at TIMESTAMP NOT NULL
);

CREATE INDEX idx_task_plan_project ON task_plan (project_id, created_at);

CREATE TABLE task_phase (
    plan_id  TEXT NOT NULL REFERENCES task_plan (id) ON DELETE CASCADE,
    id       TEXT NOT NULL,
    title    TEXT NOT NULL CHECK (length(trim(title)) > 0),
    position INTEGER NOT NULL,
    PRIMARY KEY (plan_id, id)
);

CREATE UNIQUE INDEX idx_task_phase_position ON task_phase (plan_id, position);

CREATE TABLE task (
    plan_id    TEXT NOT NULL REFERENCES task_plan (id) ON DELETE CASCADE,
    id         TEXT NOT NULL,
    phase_id   TEXT,
    title      TEXT NOT NULL CHECK (length(trim(title)) > 0),
    prompt     TEXT NOT NULL CHECK (length(trim(prompt)) > 0),
    position   INTEGER NOT NULL,
    state      TEXT NOT NULL DEFAULT 'queued'
        CHECK (state IN ('queued', 'claimed', 'running', 'collecting',
                         'completed', 'failed', 'blocked', 'cancelled')),
    created_at TIMESTAMP NOT NULL,
    updated_at TIMESTAMP NOT NULL,
    PRIMARY KEY (plan_id, id),
    -- Composite FK keeps a task's phase inside its own plan. NULL phase_id is
    -- allowed (phases are optional grouping).
    FOREIGN KEY (plan_id, phase_id) REFERENCES task_phase (plan_id, id)
);

CREATE UNIQUE INDEX idx_task_position ON task (plan_id, position);

CREATE TABLE task_dependency (
    plan_id            TEXT NOT NULL,
    task_id            TEXT NOT NULL,
    depends_on_task_id TEXT NOT NULL,
    position           INTEGER NOT NULL,
    PRIMARY KEY (plan_id, task_id, depends_on_task_id),
    CHECK (task_id <> depends_on_task_id),
    -- Both endpoints resolve against the same plan's task table, so a
    -- cross-plan edge is unrepresentable rather than merely rejected.
    FOREIGN KEY (plan_id, task_id) REFERENCES task (plan_id, id) ON DELETE CASCADE,
    FOREIGN KEY (plan_id, depends_on_task_id) REFERENCES task (plan_id, id) ON DELETE CASCADE
);

CREATE UNIQUE INDEX idx_task_dependency_position
    ON task_dependency (plan_id, task_id, position);

CREATE TABLE task_verification_command (
    plan_id  TEXT NOT NULL,
    task_id  TEXT NOT NULL,
    position INTEGER NOT NULL,
    command  TEXT NOT NULL CHECK (length(trim(command)) > 0),
    PRIMARY KEY (plan_id, task_id, position),
    FOREIGN KEY (plan_id, task_id) REFERENCES task (plan_id, id) ON DELETE CASCADE
);

-- A durable dispatch identity. Persisted BEFORE the runtime is launched so an
-- ambiguous dispatch (crash after launch, before recording) can be reconciled
-- instead of duplicated. session_id stays NULL until a worker session is
-- durably associated; it is never back-mapped from a task.
CREATE TABLE task_attempt (
    id             TEXT PRIMARY KEY,
    plan_id        TEXT NOT NULL,
    task_id        TEXT NOT NULL,
    attempt_number INTEGER NOT NULL CHECK (attempt_number > 0),
    state          TEXT NOT NULL
        CHECK (state IN ('claimed', 'running', 'collecting',
                         'completed', 'failed', 'blocked', 'cancelled')),
    session_id     TEXT REFERENCES sessions (id),
    harness        TEXT,
    claimed_at     TIMESTAMP NOT NULL,
    started_at     TIMESTAMP,
    finished_at    TIMESTAMP,
    created_at     TIMESTAMP NOT NULL,
    updated_at     TIMESTAMP NOT NULL,
    UNIQUE (plan_id, task_id, attempt_number),
    FOREIGN KEY (plan_id, task_id) REFERENCES task (plan_id, id) ON DELETE CASCADE
);

CREATE INDEX idx_task_attempt_task ON task_attempt (plan_id, task_id, attempt_number);

-- Append-only verification evidence. One terminal result per attempt, so
-- "verified" is unambiguous when deriving readiness. Evidence is stored as
-- structured JSON: remote projection (slice 3) narrows it rather than scraping
-- command output.
--
-- No ON DELETE CASCADE on purpose, and the triggers below refuse both UPDATE
-- and DELETE: a plan whose task has recorded evidence cannot be destroyed at
-- all. Deleting the plan cascades to tasks and attempts and stops there on the
-- result's foreign key, and deleting the result directly is refused as well, so
-- removing evidence would take a deliberate schema-level operation. Plan
-- deletion is not a slice-2 operation.
CREATE TABLE task_result (
    id          TEXT PRIMARY KEY,
    plan_id     TEXT NOT NULL,
    task_id     TEXT NOT NULL,
    attempt_id  TEXT NOT NULL REFERENCES task_attempt (id),
    outcome     TEXT NOT NULL CHECK (outcome IN ('verified', 'failed', 'inconclusive')),
    summary     TEXT,
    evidence    TEXT NOT NULL CHECK (json_valid(evidence)),
    recorded_at TIMESTAMP NOT NULL,
    UNIQUE (attempt_id),
    FOREIGN KEY (plan_id, task_id) REFERENCES task (plan_id, id)
);

CREATE INDEX idx_task_result_task ON task_result (plan_id, task_id, recorded_at);

CREATE TRIGGER task_result_no_update
BEFORE UPDATE ON task_result
BEGIN
    SELECT RAISE(ABORT, 'task results are append-only');
END;

CREATE TRIGGER task_result_no_delete
BEFORE DELETE ON task_result
BEGIN
    SELECT RAISE(ABORT, 'task results are append-only');
END;
-- +goose StatementEnd

-- +goose StatementBegin
CREATE TRIGGER agent_switches_cdc_insert
AFTER INSERT ON agent_switches
BEGIN
    INSERT INTO change_log (project_id, session_id, event_type, payload, created_at)
    VALUES (
        (SELECT project_id FROM sessions WHERE id = NEW.session_id),
        NEW.session_id, 'session_updated', json_object('id', NEW.session_id), NEW.updated_at
    );
END;;

CREATE TRIGGER agent_switches_cdc_update
AFTER UPDATE ON agent_switches
BEGIN
    INSERT INTO change_log (project_id, session_id, event_type, payload, created_at)
    VALUES (
        (SELECT project_id FROM sessions WHERE id = NEW.session_id),
        NEW.session_id, 'session_updated', json_object('id', NEW.session_id), NEW.updated_at
    );
END;;

CREATE TRIGGER agent_switches_failed_recovery_marker_insert
BEFORE INSERT ON agent_switches
WHEN NEW.state = 'failed'
    AND NEW.error_code IN (
        'source_stop_unconfirmed',
        'source_restore_unconfirmed',
        'target_start_unconfirmed'
    )
BEGIN
    SELECT RAISE(ABORT, 'agent switch recovery marker requires a nonterminal state');
END;;

CREATE TRIGGER agent_switches_failed_recovery_marker_update
BEFORE UPDATE ON agent_switches
WHEN NEW.state = 'failed'
    AND NEW.error_code IN (
        'source_stop_unconfirmed',
        'source_restore_unconfirmed',
        'target_start_unconfirmed'
    )
BEGIN
    SELECT RAISE(ABORT, 'agent switch recovery marker requires a nonterminal state');
END;;

CREATE TRIGGER agent_switches_target_native_scope_insert
BEFORE INSERT ON agent_switches
WHEN NEW.target_native_session_ref IS NOT NULL
    AND NOT EXISTS (
        SELECT 1 FROM agent_native_sessions
        WHERE id = NEW.target_native_session_ref
          AND ao_session_id = NEW.session_id
          AND harness = NEW.target_harness
    )
BEGIN
    SELECT RAISE(ABORT, 'agent switch target native session scope mismatch');
END;;

CREATE TRIGGER agent_switches_target_native_scope_update
BEFORE UPDATE OF session_id, target_harness, target_native_session_ref ON agent_switches
WHEN NEW.target_native_session_ref IS NOT NULL
    AND NOT EXISTS (
        SELECT 1 FROM agent_native_sessions
        WHERE id = NEW.target_native_session_ref
          AND ao_session_id = NEW.session_id
          AND harness = NEW.target_harness
    )
BEGIN
    SELECT RAISE(ABORT, 'agent switch target native session scope mismatch');
END;;

CREATE TRIGGER codex_account_switch_sessions_cdc
AFTER UPDATE ON codex_account_switch_sessions
BEGIN
    INSERT INTO change_log (project_id, session_id, event_type, payload, created_at)
    SELECT s.project_id, s.id, 'session_updated',
           json_object('id', s.id, 'sessionId', s.id),
           COALESCE(NEW.restarted_at, NEW.stopped_at, CURRENT_TIMESTAMP)
    FROM sessions s WHERE s.id = NEW.session_id;
END;;

CREATE TRIGGER codex_account_switch_sessions_cdc_insert
AFTER INSERT ON codex_account_switch_sessions
BEGIN
    INSERT INTO change_log (project_id, session_id, event_type, payload, created_at)
    SELECT s.project_id, s.id, 'session_updated',
           json_object('id', s.id, 'sessionId', s.id), CURRENT_TIMESTAMP
    FROM sessions s WHERE s.id = NEW.session_id;
END;;

CREATE TRIGGER codex_account_switches_cdc_insert
AFTER INSERT ON codex_account_switches
BEGIN
    INSERT INTO change_log (project_id, session_id, event_type, payload, created_at)
    SELECT s.project_id, ss.session_id, 'session_updated',
           json_object('id', ss.session_id, 'sessionId', ss.session_id), NEW.created_at
    FROM codex_account_switch_sessions ss
    JOIN sessions s ON s.id = ss.session_id
    WHERE ss.switch_id = NEW.id;
END;;

CREATE TRIGGER codex_account_switches_cdc_update
AFTER UPDATE OF phase, failure_code ON codex_account_switches
BEGIN
    INSERT INTO change_log (project_id, session_id, event_type, payload, created_at)
    SELECT s.project_id, ss.session_id, 'session_updated',
           json_object('id', ss.session_id, 'sessionId', ss.session_id), NEW.updated_at
    FROM codex_account_switch_sessions ss
    JOIN sessions s ON s.id = ss.session_id
    WHERE ss.switch_id = NEW.id;
END;;

CREATE TRIGGER conversation_activities_branch_insert
AFTER INSERT ON conversation_activities
WHEN NEW.branch_id = ''
BEGIN
    UPDATE conversation_activities
    SET branch_id = (SELECT active_branch_id FROM conversations WHERE id = NEW.conversation_id)
    WHERE id = NEW.id;
END;;

CREATE TRIGGER conversation_activities_cdc_insert
AFTER INSERT ON conversation_activities
BEGIN
    INSERT INTO change_log (project_id, session_id, event_type, payload, created_at)
    SELECT s.project_id, s.id, 'session_updated',
           json_object('id', s.id, 'sessionId', s.id, 'conversationId', c.id,
                       'activity', s.activity_state,
                       'isTerminated', json(CASE WHEN s.is_terminated THEN 'true' ELSE 'false' END)),
           NEW.updated_at
    FROM conversations c
    JOIN sessions s ON s.id = c.current_session_id
    WHERE c.id = NEW.conversation_id;
END;;

CREATE TRIGGER conversation_activities_cdc_update
AFTER UPDATE ON conversation_activities
WHEN OLD.revision <> NEW.revision
BEGIN
    INSERT INTO change_log (project_id, session_id, event_type, payload, created_at)
    SELECT s.project_id, s.id, 'session_updated',
           json_object('id', s.id, 'sessionId', s.id, 'conversationId', c.id,
                       'activity', s.activity_state,
                       'isTerminated', json(CASE WHEN s.is_terminated THEN 'true' ELSE 'false' END)),
           NEW.updated_at
    FROM conversations c
    JOIN sessions s ON s.id = c.current_session_id
    WHERE c.id = NEW.conversation_id;
END;;

CREATE TRIGGER conversation_branch_root_provider_update
AFTER UPDATE OF provider_conversation_id ON sessions
WHEN OLD.provider_conversation_id = '' AND NEW.provider_conversation_id <> ''
BEGIN
    UPDATE conversation_branches
    SET provider_conversation_id = NEW.provider_conversation_id
    WHERE parent_branch_id IS NULL
      AND provider_conversation_id = ''
      AND id IN (
          SELECT active_branch_id
          FROM conversations
          WHERE current_session_id = NEW.id
      );
END;;

CREATE TRIGGER conversation_messages_branch_insert
AFTER INSERT ON conversation_messages
WHEN NEW.branch_id = ''
BEGIN
    UPDATE conversation_messages
    SET branch_id = (SELECT active_branch_id FROM conversations WHERE id = NEW.conversation_id)
    WHERE id = NEW.id;
END;;

CREATE TRIGGER conversation_messages_cdc_insert
AFTER INSERT ON conversation_messages
BEGIN
    INSERT INTO change_log (project_id, session_id, event_type, payload, created_at)
    SELECT s.project_id, s.id, 'session_updated',
           json_object('id', s.id, 'sessionId', s.id, 'conversationId', NEW.conversation_id,
                       'activity', s.activity_state,
                       'isTerminated', json(CASE WHEN s.is_terminated THEN 'true' ELSE 'false' END)),
           NEW.updated_at
    FROM conversations c
    JOIN sessions s ON s.id = c.current_session_id
    WHERE c.id = NEW.conversation_id;
END;;

CREATE TRIGGER conversation_messages_cdc_update
AFTER UPDATE ON conversation_messages
WHEN OLD.revision <> NEW.revision
BEGIN
    INSERT INTO change_log (project_id, session_id, event_type, payload, created_at)
    SELECT s.project_id, s.id, 'session_updated',
           json_object('id', s.id, 'sessionId', s.id, 'conversationId', c.id,
                       'activity', s.activity_state,
                       'isTerminated', json(CASE WHEN s.is_terminated THEN 'true' ELSE 'false' END)),
           NEW.updated_at
    FROM conversations c
    JOIN sessions s ON s.id = c.current_session_id
    WHERE c.id = NEW.conversation_id;
END;;

CREATE TRIGGER conversation_provider_events_branch_insert
AFTER INSERT ON conversation_provider_events
WHEN NEW.branch_id = ''
BEGIN
    UPDATE conversation_provider_events
    SET branch_id = (SELECT active_branch_id FROM conversations WHERE id = NEW.conversation_id)
    WHERE id = NEW.id;
END;;

CREATE TRIGGER conversation_turns_branch_insert
AFTER INSERT ON conversation_turns
WHEN NEW.branch_id = ''
BEGIN
    UPDATE conversation_turns
    SET branch_id = (SELECT active_branch_id FROM conversations WHERE id = NEW.conversation_id)
    WHERE id = NEW.id;
END;;

CREATE TRIGGER conversation_turns_cdc_update
AFTER UPDATE ON conversation_turns
WHEN OLD.state <> NEW.state
BEGIN
    INSERT INTO change_log (project_id, session_id, event_type, payload, created_at)
    SELECT s.project_id, s.id, 'session_updated',
           json_object('id', s.id, 'sessionId', s.id, 'conversationId', NEW.conversation_id,
                       'activity', s.activity_state,
                       'isTerminated', json(CASE WHEN s.is_terminated THEN 'true' ELSE 'false' END)),
           COALESCE(NEW.completed_at, NEW.started_at, NEW.requested_at)
    FROM sessions s
    WHERE s.id = NEW.handled_by_session_id;
END;;

CREATE TRIGGER pr_cdc_insert
AFTER INSERT ON pr
BEGIN
    INSERT INTO change_log (project_id, session_id, event_type, payload, created_at)
    VALUES ((SELECT project_id FROM sessions WHERE id = NEW.session_id), NEW.session_id, 'pr_created',
        json_object('url', NEW.url, 'session', NEW.session_id, 'state', NEW.pr_state,
                    'ci', NEW.ci_state, 'review', NEW.review_decision, 'mergeability', NEW.mergeability),
        NEW.updated_at);
END;;

CREATE TRIGGER pr_cdc_update
AFTER UPDATE ON pr
WHEN OLD.pr_state <> NEW.pr_state
    OR OLD.ci_state <> NEW.ci_state
    OR OLD.review_decision <> NEW.review_decision
    OR OLD.mergeability <> NEW.mergeability
    OR OLD.auto_inject_ci <> NEW.auto_inject_ci
BEGIN
    INSERT INTO change_log (project_id, session_id, event_type, payload, created_at)
    VALUES ((SELECT project_id FROM sessions WHERE id = NEW.session_id), NEW.session_id, 'pr_updated',
        json_object('url', NEW.url, 'session', NEW.session_id, 'state', NEW.pr_state,
                    'ci', NEW.ci_state, 'review', NEW.review_decision, 'mergeability', NEW.mergeability,
                    'autoInjectCI', json(CASE WHEN NEW.auto_inject_ci THEN 'true' ELSE 'false' END)),
        NEW.updated_at);
END;;

CREATE TRIGGER pr_checks_cdc_insert
AFTER INSERT ON pr_checks
BEGIN
    INSERT INTO change_log (project_id, session_id, event_type, payload, created_at)
    VALUES (
        (SELECT s.project_id FROM pr p JOIN sessions s ON s.id = p.session_id WHERE p.url = NEW.pr_url),
        (SELECT session_id FROM pr WHERE url = NEW.pr_url),
        'pr_check_recorded',
        json_object('pr', NEW.pr_url, 'name', NEW.name, 'commit', NEW.commit_hash, 'status', NEW.status),
        NEW.created_at);
END;;

CREATE TRIGGER pr_checks_cdc_update
AFTER UPDATE ON pr_checks
WHEN OLD.status <> NEW.status
BEGIN
    INSERT INTO change_log (project_id, session_id, event_type, payload, created_at)
    VALUES (
        (SELECT s.project_id FROM pr p JOIN sessions s ON s.id = p.session_id WHERE p.url = NEW.pr_url),
        (SELECT session_id FROM pr WHERE url = NEW.pr_url),
        'pr_check_recorded',
        json_object('pr', NEW.pr_url, 'name', NEW.name, 'commit', NEW.commit_hash, 'status', NEW.status),
        datetime('now'));
END;;

CREATE TRIGGER pr_review_threads_cdc_insert
AFTER INSERT ON pr_review_threads
BEGIN
    INSERT INTO change_log (project_id, session_id, event_type, payload, created_at)
    VALUES (
        (SELECT s.project_id FROM pr p JOIN sessions s ON s.id = p.session_id WHERE p.url = NEW.pr_url),
        (SELECT session_id FROM pr WHERE url = NEW.pr_url),
        'pr_review_thread_added',
        json_object(
            'pr', NEW.pr_url,
            'thread', NEW.thread_id,
            'path', NEW.path,
            'line', NEW.line,
            'resolved', json(CASE WHEN NEW.resolved THEN 'true' ELSE 'false' END),
            'isBot', json(CASE WHEN NEW.is_bot THEN 'true' ELSE 'false' END)
        ),
        NEW.updated_at);
END;;

CREATE TRIGGER pr_review_threads_cdc_update
AFTER UPDATE ON pr_review_threads
WHEN OLD.resolved <> NEW.resolved
BEGIN
    INSERT INTO change_log (project_id, session_id, event_type, payload, created_at)
    VALUES (
        (SELECT s.project_id FROM pr p JOIN sessions s ON s.id = p.session_id WHERE p.url = NEW.pr_url),
        (SELECT session_id FROM pr WHERE url = NEW.pr_url),
        'pr_review_thread_resolved',
        json_object(
            'pr', NEW.pr_url,
            'thread', NEW.thread_id,
            'path', NEW.path,
            'line', NEW.line,
            'resolved', json(CASE WHEN NEW.resolved THEN 'true' ELSE 'false' END)
        ),
        NEW.updated_at);
END;;

CREATE TRIGGER pr_session_cdc_update
AFTER UPDATE ON pr
WHEN OLD.session_id <> NEW.session_id
BEGIN
    INSERT INTO change_log (project_id, session_id, event_type, payload, created_at)
    VALUES (
        (SELECT project_id FROM sessions WHERE id = NEW.session_id),
        NEW.session_id,
        'pr_session_changed',
        json_object(
            'url', NEW.url,
            'fromSession', OLD.session_id,
            'toSession', NEW.session_id),
        NEW.updated_at);
END;;

CREATE TRIGGER review_run_cdc_insert
AFTER INSERT ON review_run
BEGIN
    INSERT INTO change_log (project_id, session_id, event_type, payload, created_at)
    VALUES (
        (SELECT project_id FROM sessions WHERE id = NEW.session_id),
        NEW.session_id,
        'review_run_created',
        json_object(
            'id', NEW.id,
            'reviewId', NEW.review_id,
            'sessionId', NEW.session_id,
            'pr', NEW.pr_url,
            'targetSha', NEW.target_sha,
            'status', NEW.status,
            'verdict', NEW.verdict,
            'triggerSource', NEW.trigger_source,
            'githubReviewId', NEW.github_review_id,
            'autoInjectReview', json(CASE WHEN NEW.auto_inject_review THEN 'true' ELSE 'false' END)
        ),
        NEW.created_at);
END;;

CREATE TRIGGER review_run_cdc_update
AFTER UPDATE ON review_run
WHEN OLD.status <> NEW.status
    OR OLD.verdict <> NEW.verdict
    OR OLD.body <> NEW.body
    OR OLD.github_review_id <> NEW.github_review_id
    OR OLD.auto_inject_review <> NEW.auto_inject_review
BEGIN
    INSERT INTO change_log (project_id, session_id, event_type, payload, created_at)
    VALUES (
        (SELECT project_id FROM sessions WHERE id = NEW.session_id),
        NEW.session_id,
        'review_run_updated',
        json_object(
            'id', NEW.id,
            'reviewId', NEW.review_id,
            'sessionId', NEW.session_id,
            'pr', NEW.pr_url,
            'targetSha', NEW.target_sha,
            'status', NEW.status,
            'verdict', NEW.verdict,
            'triggerSource', NEW.trigger_source,
            'githubReviewId', NEW.github_review_id,
            'autoInjectReview', json(CASE WHEN NEW.auto_inject_review THEN 'true' ELSE 'false' END)
        ),
        datetime('now'));
END;;

CREATE TRIGGER session_cleanup_facts_cdc_insert
AFTER INSERT ON session_cleanup_facts
BEGIN
    INSERT INTO change_log (project_id, session_id, event_type, payload, created_at)
    VALUES ((SELECT project_id FROM sessions WHERE id = NEW.session_id), NEW.session_id, 'session_updated',
        json_object('id', NEW.session_id),
        datetime('now'));
END;;

CREATE TRIGGER session_cleanup_facts_cdc_update
AFTER UPDATE ON session_cleanup_facts
WHEN OLD.workspace_disposition <> NEW.workspace_disposition
    OR (OLD.runtime_released_at IS NULL) <> (NEW.runtime_released_at IS NULL)
BEGIN
    INSERT INTO change_log (project_id, session_id, event_type, payload, created_at)
    VALUES ((SELECT project_id FROM sessions WHERE id = NEW.session_id), NEW.session_id, 'session_updated',
        json_object('id', NEW.session_id),
        datetime('now'));
END;;

CREATE TRIGGER session_interface_transitions_cdc_insert
AFTER INSERT ON session_interface_transitions
BEGIN
    INSERT INTO change_log (project_id, session_id, event_type, payload, created_at)
    SELECT s.project_id, s.id, 'session_updated',
           json_object('id', s.id, 'sessionId', s.id,
                       'interfaceTransitionId', NEW.id,
                       'interfaceTransitionPhase', NEW.phase,
                       'activity', s.activity_state,
                       'isTerminated', json(CASE WHEN s.is_terminated THEN 'true' ELSE 'false' END)),
           NEW.updated_at
    FROM sessions s WHERE s.id = NEW.session_id;
END;;

CREATE TRIGGER session_interface_transitions_cdc_update
AFTER UPDATE ON session_interface_transitions
WHEN OLD.phase <> NEW.phase
    OR OLD.error_code <> NEW.error_code
    OR OLD.error_detail <> NEW.error_detail
    OR OLD.notice_acknowledged_at IS NOT NEW.notice_acknowledged_at
BEGIN
    INSERT INTO change_log (project_id, session_id, event_type, payload, created_at)
    SELECT s.project_id, s.id, 'session_updated',
           json_object('id', s.id, 'sessionId', s.id,
                       'interfaceTransitionId', NEW.id,
                       'interfaceTransitionPhase', NEW.phase,
                       'activity', s.activity_state,
                       'isTerminated', json(CASE WHEN s.is_terminated THEN 'true' ELSE 'false' END)),
           COALESCE(NEW.notice_acknowledged_at, NEW.updated_at)
    FROM sessions s WHERE s.id = NEW.session_id;
END;;

CREATE TRIGGER sessions_cdc_insert
AFTER INSERT ON sessions
BEGIN
    INSERT INTO change_log (project_id, session_id, event_type, payload, created_at)
    VALUES (NEW.project_id, NEW.id, 'session_created',
        json_object('id', NEW.id, 'activity', NEW.activity_state, 'isTerminated', json(CASE WHEN NEW.is_terminated THEN 'true' ELSE 'false' END)),
        NEW.updated_at);
END;;

CREATE TRIGGER sessions_cdc_update
AFTER UPDATE ON sessions
WHEN OLD.activity_state <> NEW.activity_state
    OR OLD.is_terminated <> NEW.is_terminated
    OR (OLD.first_signal_at IS NULL AND NEW.first_signal_at IS NOT NULL)
    OR OLD.preview_url <> NEW.preview_url
    OR OLD.preview_revision <> NEW.preview_revision
    OR OLD.display_name <> NEW.display_name
    OR OLD.terminate_on_pr_merge <> NEW.terminate_on_pr_merge
    OR OLD.is_pinned <> NEW.is_pinned
    OR OLD.pinned_at <> NEW.pinned_at
    OR (OLD.pinned_at IS NULL AND NEW.pinned_at IS NOT NULL)
    OR (OLD.pinned_at IS NOT NULL AND NEW.pinned_at IS NULL)
    OR OLD.session_mode <> NEW.session_mode
    OR OLD.auto_inject_review <> NEW.auto_inject_review
    OR OLD.auto_review_enabled <> NEW.auto_review_enabled
    OR OLD.harness <> NEW.harness
    OR OLD.runtime_launch_id <> NEW.runtime_launch_id
    OR OLD.agent_session_id <> NEW.agent_session_id
    OR OLD.native_transcript_path <> NEW.native_transcript_path
    OR OLD.auto_inject_ci <> NEW.auto_inject_ci
    OR OLD.latest_user_prompt_at IS NOT NEW.latest_user_prompt_at
BEGIN
    INSERT INTO change_log (project_id, session_id, event_type, payload, created_at)
    VALUES (NEW.project_id, NEW.id, 'session_updated',
        json_object(
            'id', NEW.id,
            'activity', NEW.activity_state,
            'isTerminated', json(CASE WHEN NEW.is_terminated THEN 'true' ELSE 'false' END),
            'terminateOnPrMerge', json(CASE WHEN NEW.terminate_on_pr_merge THEN 'true' ELSE 'false' END),
            'previewUrl', NEW.preview_url,
            'previewRevision', NEW.preview_revision,
            'isPinned', json(CASE WHEN NEW.is_pinned THEN 'true' ELSE 'false' END),
            'mode', NEW.session_mode,
            'autoInjectReview', json(CASE WHEN NEW.auto_inject_review THEN 'true' ELSE 'false' END),
            'autoInjectCI', json(CASE WHEN NEW.auto_inject_ci THEN 'true' ELSE 'false' END),
            'autoReviewEnabled', json(CASE WHEN NEW.auto_review_enabled THEN 'true' ELSE 'false' END)
        ),
        NEW.updated_at);
END;;

CREATE TRIGGER usage_bindings_cdc_insert AFTER INSERT ON usage_bindings BEGIN
    INSERT INTO change_log (project_id, session_id, event_type, payload, created_at)
    VALUES ((SELECT project_id FROM sessions WHERE id = NEW.session_id),
            NEW.session_id, 'session_updated', json_object('id', NEW.session_id), NEW.updated_at);
END;;

CREATE TRIGGER usage_bindings_cdc_update AFTER UPDATE ON usage_bindings BEGIN
    INSERT INTO change_log (project_id, session_id, event_type, payload, created_at)
    VALUES ((SELECT project_id FROM sessions WHERE id = NEW.session_id),
            NEW.session_id, 'session_updated', json_object('id', NEW.session_id), NEW.updated_at);
END;;

CREATE TRIGGER usage_sources_cdc_update AFTER UPDATE ON usage_sources
WHEN OLD.anomaly_count IS NOT NEW.anomaly_count
  OR OLD.last_error_code IS NOT NEW.last_error_code
BEGIN
    INSERT INTO change_log (project_id, session_id, event_type, payload, created_at)
    SELECT s.project_id, ub.session_id, 'session_updated', json_object('id', ub.session_id), NEW.updated_at
    FROM usage_bindings ub JOIN sessions s ON s.id = ub.session_id WHERE ub.id = NEW.binding_id;
END;;

CREATE TRIGGER task_plan_cdc_insert
AFTER INSERT ON task_plan
BEGIN
    INSERT INTO change_log (project_id, session_id, event_type, payload, created_at)
    VALUES (NEW.project_id, NULL, 'task_plan_created',
        json_object('id', NEW.id, 'planId', NEW.id, 'title', NEW.title),
        NEW.created_at);
END;

CREATE TRIGGER task_cdc_insert
AFTER INSERT ON task
BEGIN
    INSERT INTO change_log (project_id, session_id, event_type, payload, created_at)
    VALUES ((SELECT project_id FROM task_plan WHERE id = NEW.plan_id), NULL, 'task_created',
        json_object('id', NEW.id, 'planId', NEW.plan_id, 'taskId', NEW.id, 'state', NEW.state),
        NEW.created_at);
END;

CREATE TRIGGER task_cdc_update
AFTER UPDATE ON task
WHEN OLD.state <> NEW.state
BEGIN
    INSERT INTO change_log (project_id, session_id, event_type, payload, created_at)
    VALUES ((SELECT project_id FROM task_plan WHERE id = NEW.plan_id), NULL, 'task_updated',
        json_object('id', NEW.id, 'planId', NEW.plan_id, 'taskId', NEW.id, 'state', NEW.state),
        NEW.updated_at);
END;

CREATE TRIGGER task_attempt_cdc_insert
AFTER INSERT ON task_attempt
BEGIN
    INSERT INTO change_log (project_id, session_id, event_type, payload, created_at)
    VALUES ((SELECT project_id FROM task_plan WHERE id = NEW.plan_id), NEW.session_id, 'task_attempt_created',
        json_object('id', NEW.id, 'planId', NEW.plan_id, 'taskId', NEW.task_id, 'attemptId', NEW.id,
                    'sessionId', NEW.session_id, 'state', NEW.state),
        NEW.created_at);
END;

-- Guarded on state or on a newly durable session association, so reopening or
-- re-probing an attempt emits nothing. The session association is the only
-- non-state transition worth an event.
CREATE TRIGGER task_attempt_cdc_update
AFTER UPDATE ON task_attempt
WHEN OLD.state <> NEW.state
    OR (OLD.session_id IS NULL AND NEW.session_id IS NOT NULL)
BEGIN
    INSERT INTO change_log (project_id, session_id, event_type, payload, created_at)
    VALUES ((SELECT project_id FROM task_plan WHERE id = NEW.plan_id), NEW.session_id, 'task_attempt_updated',
        json_object('id', NEW.id, 'planId', NEW.plan_id, 'taskId', NEW.task_id, 'attemptId', NEW.id,
                    'sessionId', NEW.session_id, 'state', NEW.state),
        NEW.updated_at);
END;

CREATE TRIGGER task_result_cdc_insert
AFTER INSERT ON task_result
BEGIN
    INSERT INTO change_log (project_id, session_id, event_type, payload, created_at)
    VALUES (
        (SELECT project_id FROM task_plan WHERE id = NEW.plan_id),
        (SELECT session_id FROM task_attempt WHERE id = NEW.attempt_id),
        'task_result_recorded',
        json_object('id', NEW.id, 'planId', NEW.plan_id, 'taskId', NEW.task_id,
                    'attemptId', NEW.attempt_id,
                    'sessionId', (SELECT session_id FROM task_attempt WHERE id = NEW.attempt_id),
                    'outcome', NEW.outcome),
        NEW.recorded_at);
END;
-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
DROP TRIGGER IF EXISTS task_plan_cdc_insert;
DROP TRIGGER IF EXISTS task_cdc_insert;
DROP TRIGGER IF EXISTS task_cdc_update;
DROP TRIGGER IF EXISTS task_attempt_cdc_insert;
DROP TRIGGER IF EXISTS task_attempt_cdc_update;
DROP TRIGGER IF EXISTS task_result_cdc_insert;

DROP TABLE IF EXISTS task_result;
DROP TABLE IF EXISTS task_attempt;
DROP TABLE IF EXISTS task_verification_command;
DROP TABLE IF EXISTS task_dependency;
DROP TABLE IF EXISTS task;
DROP TABLE IF EXISTS task_phase;
DROP TABLE IF EXISTS task_plan;

DROP TRIGGER IF EXISTS agent_switches_cdc_insert;
DROP TRIGGER IF EXISTS agent_switches_cdc_update;
DROP TRIGGER IF EXISTS agent_switches_failed_recovery_marker_insert;
DROP TRIGGER IF EXISTS agent_switches_failed_recovery_marker_update;
DROP TRIGGER IF EXISTS agent_switches_target_native_scope_insert;
DROP TRIGGER IF EXISTS agent_switches_target_native_scope_update;
DROP TRIGGER IF EXISTS codex_account_switch_sessions_cdc;
DROP TRIGGER IF EXISTS codex_account_switch_sessions_cdc_insert;
DROP TRIGGER IF EXISTS codex_account_switches_cdc_insert;
DROP TRIGGER IF EXISTS codex_account_switches_cdc_update;
DROP TRIGGER IF EXISTS conversation_activities_branch_insert;
DROP TRIGGER IF EXISTS conversation_activities_cdc_insert;
DROP TRIGGER IF EXISTS conversation_activities_cdc_update;
DROP TRIGGER IF EXISTS conversation_branch_root_provider_update;
DROP TRIGGER IF EXISTS conversation_messages_branch_insert;
DROP TRIGGER IF EXISTS conversation_messages_cdc_insert;
DROP TRIGGER IF EXISTS conversation_messages_cdc_update;
DROP TRIGGER IF EXISTS conversation_provider_events_branch_insert;
DROP TRIGGER IF EXISTS conversation_turns_branch_insert;
DROP TRIGGER IF EXISTS conversation_turns_cdc_update;
DROP TRIGGER IF EXISTS pr_cdc_insert;
DROP TRIGGER IF EXISTS pr_cdc_update;
DROP TRIGGER IF EXISTS pr_checks_cdc_insert;
DROP TRIGGER IF EXISTS pr_checks_cdc_update;
DROP TRIGGER IF EXISTS pr_review_threads_cdc_insert;
DROP TRIGGER IF EXISTS pr_review_threads_cdc_update;
DROP TRIGGER IF EXISTS pr_session_cdc_update;
DROP TRIGGER IF EXISTS review_run_cdc_insert;
DROP TRIGGER IF EXISTS review_run_cdc_update;
DROP TRIGGER IF EXISTS session_cleanup_facts_cdc_insert;
DROP TRIGGER IF EXISTS session_cleanup_facts_cdc_update;
DROP TRIGGER IF EXISTS session_interface_transitions_cdc_insert;
DROP TRIGGER IF EXISTS session_interface_transitions_cdc_update;
DROP TRIGGER IF EXISTS sessions_cdc_insert;
DROP TRIGGER IF EXISTS sessions_cdc_update;
DROP TRIGGER IF EXISTS usage_bindings_cdc_insert;
DROP TRIGGER IF EXISTS usage_bindings_cdc_update;
DROP TRIGGER IF EXISTS usage_sources_cdc_update;

DROP TRIGGER IF EXISTS change_log_old_insert;
DROP VIEW IF EXISTS change_log_old;

CREATE TABLE change_log_new (
    seq        INTEGER PRIMARY KEY AUTOINCREMENT,
    project_id TEXT NOT NULL REFERENCES projects (id),
    session_id TEXT REFERENCES sessions (id),
    event_type TEXT NOT NULL
        CHECK (event_type IN (
            'session_created',
            'session_updated',
            'pr_created',
            'pr_updated',
            'pr_check_recorded',
            'pr_session_changed',
            'pr_review_thread_added',
            'pr_review_thread_resolved',
            'review_run_created',
            'review_run_updated'
        )),
    payload    TEXT NOT NULL CHECK (json_valid(payload)),
    created_at TIMESTAMP NOT NULL DEFAULT (datetime('now'))
);

INSERT INTO change_log_new (seq, project_id, session_id, event_type, payload, created_at)
SELECT seq, project_id, session_id, event_type, payload, created_at
FROM change_log
WHERE event_type NOT IN (
    'task_plan_created', 'task_created', 'task_updated',
    'task_attempt_created', 'task_attempt_updated', 'task_result_recorded'
);

DROP INDEX IF EXISTS idx_change_log_project;
DROP TABLE change_log;
ALTER TABLE change_log_new RENAME TO change_log;
CREATE INDEX idx_change_log_project ON change_log (project_id, seq);
-- +goose StatementEnd

-- +goose StatementBegin
CREATE TRIGGER agent_switches_cdc_insert
AFTER INSERT ON agent_switches
BEGIN
    INSERT INTO change_log (project_id, session_id, event_type, payload, created_at)
    VALUES (
        (SELECT project_id FROM sessions WHERE id = NEW.session_id),
        NEW.session_id, 'session_updated', json_object('id', NEW.session_id), NEW.updated_at
    );
END;;

CREATE TRIGGER agent_switches_cdc_update
AFTER UPDATE ON agent_switches
BEGIN
    INSERT INTO change_log (project_id, session_id, event_type, payload, created_at)
    VALUES (
        (SELECT project_id FROM sessions WHERE id = NEW.session_id),
        NEW.session_id, 'session_updated', json_object('id', NEW.session_id), NEW.updated_at
    );
END;;

CREATE TRIGGER agent_switches_failed_recovery_marker_insert
BEFORE INSERT ON agent_switches
WHEN NEW.state = 'failed'
    AND NEW.error_code IN (
        'source_stop_unconfirmed',
        'source_restore_unconfirmed',
        'target_start_unconfirmed'
    )
BEGIN
    SELECT RAISE(ABORT, 'agent switch recovery marker requires a nonterminal state');
END;;

CREATE TRIGGER agent_switches_failed_recovery_marker_update
BEFORE UPDATE ON agent_switches
WHEN NEW.state = 'failed'
    AND NEW.error_code IN (
        'source_stop_unconfirmed',
        'source_restore_unconfirmed',
        'target_start_unconfirmed'
    )
BEGIN
    SELECT RAISE(ABORT, 'agent switch recovery marker requires a nonterminal state');
END;;

CREATE TRIGGER agent_switches_target_native_scope_insert
BEFORE INSERT ON agent_switches
WHEN NEW.target_native_session_ref IS NOT NULL
    AND NOT EXISTS (
        SELECT 1 FROM agent_native_sessions
        WHERE id = NEW.target_native_session_ref
          AND ao_session_id = NEW.session_id
          AND harness = NEW.target_harness
    )
BEGIN
    SELECT RAISE(ABORT, 'agent switch target native session scope mismatch');
END;;

CREATE TRIGGER agent_switches_target_native_scope_update
BEFORE UPDATE OF session_id, target_harness, target_native_session_ref ON agent_switches
WHEN NEW.target_native_session_ref IS NOT NULL
    AND NOT EXISTS (
        SELECT 1 FROM agent_native_sessions
        WHERE id = NEW.target_native_session_ref
          AND ao_session_id = NEW.session_id
          AND harness = NEW.target_harness
    )
BEGIN
    SELECT RAISE(ABORT, 'agent switch target native session scope mismatch');
END;;

CREATE TRIGGER codex_account_switch_sessions_cdc
AFTER UPDATE ON codex_account_switch_sessions
BEGIN
    INSERT INTO change_log (project_id, session_id, event_type, payload, created_at)
    SELECT s.project_id, s.id, 'session_updated',
           json_object('id', s.id, 'sessionId', s.id),
           COALESCE(NEW.restarted_at, NEW.stopped_at, CURRENT_TIMESTAMP)
    FROM sessions s WHERE s.id = NEW.session_id;
END;;

CREATE TRIGGER codex_account_switch_sessions_cdc_insert
AFTER INSERT ON codex_account_switch_sessions
BEGIN
    INSERT INTO change_log (project_id, session_id, event_type, payload, created_at)
    SELECT s.project_id, s.id, 'session_updated',
           json_object('id', s.id, 'sessionId', s.id), CURRENT_TIMESTAMP
    FROM sessions s WHERE s.id = NEW.session_id;
END;;

CREATE TRIGGER codex_account_switches_cdc_insert
AFTER INSERT ON codex_account_switches
BEGIN
    INSERT INTO change_log (project_id, session_id, event_type, payload, created_at)
    SELECT s.project_id, ss.session_id, 'session_updated',
           json_object('id', ss.session_id, 'sessionId', ss.session_id), NEW.created_at
    FROM codex_account_switch_sessions ss
    JOIN sessions s ON s.id = ss.session_id
    WHERE ss.switch_id = NEW.id;
END;;

CREATE TRIGGER codex_account_switches_cdc_update
AFTER UPDATE OF phase, failure_code ON codex_account_switches
BEGIN
    INSERT INTO change_log (project_id, session_id, event_type, payload, created_at)
    SELECT s.project_id, ss.session_id, 'session_updated',
           json_object('id', ss.session_id, 'sessionId', ss.session_id), NEW.updated_at
    FROM codex_account_switch_sessions ss
    JOIN sessions s ON s.id = ss.session_id
    WHERE ss.switch_id = NEW.id;
END;;

CREATE TRIGGER conversation_activities_branch_insert
AFTER INSERT ON conversation_activities
WHEN NEW.branch_id = ''
BEGIN
    UPDATE conversation_activities
    SET branch_id = (SELECT active_branch_id FROM conversations WHERE id = NEW.conversation_id)
    WHERE id = NEW.id;
END;;

CREATE TRIGGER conversation_activities_cdc_insert
AFTER INSERT ON conversation_activities
BEGIN
    INSERT INTO change_log (project_id, session_id, event_type, payload, created_at)
    SELECT s.project_id, s.id, 'session_updated',
           json_object('id', s.id, 'sessionId', s.id, 'conversationId', c.id,
                       'activity', s.activity_state,
                       'isTerminated', json(CASE WHEN s.is_terminated THEN 'true' ELSE 'false' END)),
           NEW.updated_at
    FROM conversations c
    JOIN sessions s ON s.id = c.current_session_id
    WHERE c.id = NEW.conversation_id;
END;;

CREATE TRIGGER conversation_activities_cdc_update
AFTER UPDATE ON conversation_activities
WHEN OLD.revision <> NEW.revision
BEGIN
    INSERT INTO change_log (project_id, session_id, event_type, payload, created_at)
    SELECT s.project_id, s.id, 'session_updated',
           json_object('id', s.id, 'sessionId', s.id, 'conversationId', c.id,
                       'activity', s.activity_state,
                       'isTerminated', json(CASE WHEN s.is_terminated THEN 'true' ELSE 'false' END)),
           NEW.updated_at
    FROM conversations c
    JOIN sessions s ON s.id = c.current_session_id
    WHERE c.id = NEW.conversation_id;
END;;

CREATE TRIGGER conversation_branch_root_provider_update
AFTER UPDATE OF provider_conversation_id ON sessions
WHEN OLD.provider_conversation_id = '' AND NEW.provider_conversation_id <> ''
BEGIN
    UPDATE conversation_branches
    SET provider_conversation_id = NEW.provider_conversation_id
    WHERE parent_branch_id IS NULL
      AND provider_conversation_id = ''
      AND id IN (
          SELECT active_branch_id
          FROM conversations
          WHERE current_session_id = NEW.id
      );
END;;

CREATE TRIGGER conversation_messages_branch_insert
AFTER INSERT ON conversation_messages
WHEN NEW.branch_id = ''
BEGIN
    UPDATE conversation_messages
    SET branch_id = (SELECT active_branch_id FROM conversations WHERE id = NEW.conversation_id)
    WHERE id = NEW.id;
END;;

CREATE TRIGGER conversation_messages_cdc_insert
AFTER INSERT ON conversation_messages
BEGIN
    INSERT INTO change_log (project_id, session_id, event_type, payload, created_at)
    SELECT s.project_id, s.id, 'session_updated',
           json_object('id', s.id, 'sessionId', s.id, 'conversationId', NEW.conversation_id,
                       'activity', s.activity_state,
                       'isTerminated', json(CASE WHEN s.is_terminated THEN 'true' ELSE 'false' END)),
           NEW.updated_at
    FROM conversations c
    JOIN sessions s ON s.id = c.current_session_id
    WHERE c.id = NEW.conversation_id;
END;;

CREATE TRIGGER conversation_messages_cdc_update
AFTER UPDATE ON conversation_messages
WHEN OLD.revision <> NEW.revision
BEGIN
    INSERT INTO change_log (project_id, session_id, event_type, payload, created_at)
    SELECT s.project_id, s.id, 'session_updated',
           json_object('id', s.id, 'sessionId', s.id, 'conversationId', c.id,
                       'activity', s.activity_state,
                       'isTerminated', json(CASE WHEN s.is_terminated THEN 'true' ELSE 'false' END)),
           NEW.updated_at
    FROM conversations c
    JOIN sessions s ON s.id = c.current_session_id
    WHERE c.id = NEW.conversation_id;
END;;

CREATE TRIGGER conversation_provider_events_branch_insert
AFTER INSERT ON conversation_provider_events
WHEN NEW.branch_id = ''
BEGIN
    UPDATE conversation_provider_events
    SET branch_id = (SELECT active_branch_id FROM conversations WHERE id = NEW.conversation_id)
    WHERE id = NEW.id;
END;;

CREATE TRIGGER conversation_turns_branch_insert
AFTER INSERT ON conversation_turns
WHEN NEW.branch_id = ''
BEGIN
    UPDATE conversation_turns
    SET branch_id = (SELECT active_branch_id FROM conversations WHERE id = NEW.conversation_id)
    WHERE id = NEW.id;
END;;

CREATE TRIGGER conversation_turns_cdc_update
AFTER UPDATE ON conversation_turns
WHEN OLD.state <> NEW.state
BEGIN
    INSERT INTO change_log (project_id, session_id, event_type, payload, created_at)
    SELECT s.project_id, s.id, 'session_updated',
           json_object('id', s.id, 'sessionId', s.id, 'conversationId', NEW.conversation_id,
                       'activity', s.activity_state,
                       'isTerminated', json(CASE WHEN s.is_terminated THEN 'true' ELSE 'false' END)),
           COALESCE(NEW.completed_at, NEW.started_at, NEW.requested_at)
    FROM sessions s
    WHERE s.id = NEW.handled_by_session_id;
END;;

CREATE TRIGGER pr_cdc_insert
AFTER INSERT ON pr
BEGIN
    INSERT INTO change_log (project_id, session_id, event_type, payload, created_at)
    VALUES ((SELECT project_id FROM sessions WHERE id = NEW.session_id), NEW.session_id, 'pr_created',
        json_object('url', NEW.url, 'session', NEW.session_id, 'state', NEW.pr_state,
                    'ci', NEW.ci_state, 'review', NEW.review_decision, 'mergeability', NEW.mergeability),
        NEW.updated_at);
END;;

CREATE TRIGGER pr_cdc_update
AFTER UPDATE ON pr
WHEN OLD.pr_state <> NEW.pr_state
    OR OLD.ci_state <> NEW.ci_state
    OR OLD.review_decision <> NEW.review_decision
    OR OLD.mergeability <> NEW.mergeability
    OR OLD.auto_inject_ci <> NEW.auto_inject_ci
BEGIN
    INSERT INTO change_log (project_id, session_id, event_type, payload, created_at)
    VALUES ((SELECT project_id FROM sessions WHERE id = NEW.session_id), NEW.session_id, 'pr_updated',
        json_object('url', NEW.url, 'session', NEW.session_id, 'state', NEW.pr_state,
                    'ci', NEW.ci_state, 'review', NEW.review_decision, 'mergeability', NEW.mergeability,
                    'autoInjectCI', json(CASE WHEN NEW.auto_inject_ci THEN 'true' ELSE 'false' END)),
        NEW.updated_at);
END;;

CREATE TRIGGER pr_checks_cdc_insert
AFTER INSERT ON pr_checks
BEGIN
    INSERT INTO change_log (project_id, session_id, event_type, payload, created_at)
    VALUES (
        (SELECT s.project_id FROM pr p JOIN sessions s ON s.id = p.session_id WHERE p.url = NEW.pr_url),
        (SELECT session_id FROM pr WHERE url = NEW.pr_url),
        'pr_check_recorded',
        json_object('pr', NEW.pr_url, 'name', NEW.name, 'commit', NEW.commit_hash, 'status', NEW.status),
        NEW.created_at);
END;;

CREATE TRIGGER pr_checks_cdc_update
AFTER UPDATE ON pr_checks
WHEN OLD.status <> NEW.status
BEGIN
    INSERT INTO change_log (project_id, session_id, event_type, payload, created_at)
    VALUES (
        (SELECT s.project_id FROM pr p JOIN sessions s ON s.id = p.session_id WHERE p.url = NEW.pr_url),
        (SELECT session_id FROM pr WHERE url = NEW.pr_url),
        'pr_check_recorded',
        json_object('pr', NEW.pr_url, 'name', NEW.name, 'commit', NEW.commit_hash, 'status', NEW.status),
        datetime('now'));
END;;

CREATE TRIGGER pr_review_threads_cdc_insert
AFTER INSERT ON pr_review_threads
BEGIN
    INSERT INTO change_log (project_id, session_id, event_type, payload, created_at)
    VALUES (
        (SELECT s.project_id FROM pr p JOIN sessions s ON s.id = p.session_id WHERE p.url = NEW.pr_url),
        (SELECT session_id FROM pr WHERE url = NEW.pr_url),
        'pr_review_thread_added',
        json_object(
            'pr', NEW.pr_url,
            'thread', NEW.thread_id,
            'path', NEW.path,
            'line', NEW.line,
            'resolved', json(CASE WHEN NEW.resolved THEN 'true' ELSE 'false' END),
            'isBot', json(CASE WHEN NEW.is_bot THEN 'true' ELSE 'false' END)
        ),
        NEW.updated_at);
END;;

CREATE TRIGGER pr_review_threads_cdc_update
AFTER UPDATE ON pr_review_threads
WHEN OLD.resolved <> NEW.resolved
BEGIN
    INSERT INTO change_log (project_id, session_id, event_type, payload, created_at)
    VALUES (
        (SELECT s.project_id FROM pr p JOIN sessions s ON s.id = p.session_id WHERE p.url = NEW.pr_url),
        (SELECT session_id FROM pr WHERE url = NEW.pr_url),
        'pr_review_thread_resolved',
        json_object(
            'pr', NEW.pr_url,
            'thread', NEW.thread_id,
            'path', NEW.path,
            'line', NEW.line,
            'resolved', json(CASE WHEN NEW.resolved THEN 'true' ELSE 'false' END)
        ),
        NEW.updated_at);
END;;

CREATE TRIGGER pr_session_cdc_update
AFTER UPDATE ON pr
WHEN OLD.session_id <> NEW.session_id
BEGIN
    INSERT INTO change_log (project_id, session_id, event_type, payload, created_at)
    VALUES (
        (SELECT project_id FROM sessions WHERE id = NEW.session_id),
        NEW.session_id,
        'pr_session_changed',
        json_object(
            'url', NEW.url,
            'fromSession', OLD.session_id,
            'toSession', NEW.session_id),
        NEW.updated_at);
END;;

CREATE TRIGGER review_run_cdc_insert
AFTER INSERT ON review_run
BEGIN
    INSERT INTO change_log (project_id, session_id, event_type, payload, created_at)
    VALUES (
        (SELECT project_id FROM sessions WHERE id = NEW.session_id),
        NEW.session_id,
        'review_run_created',
        json_object(
            'id', NEW.id,
            'reviewId', NEW.review_id,
            'sessionId', NEW.session_id,
            'pr', NEW.pr_url,
            'targetSha', NEW.target_sha,
            'status', NEW.status,
            'verdict', NEW.verdict,
            'triggerSource', NEW.trigger_source,
            'githubReviewId', NEW.github_review_id,
            'autoInjectReview', json(CASE WHEN NEW.auto_inject_review THEN 'true' ELSE 'false' END)
        ),
        NEW.created_at);
END;;

CREATE TRIGGER review_run_cdc_update
AFTER UPDATE ON review_run
WHEN OLD.status <> NEW.status
    OR OLD.verdict <> NEW.verdict
    OR OLD.body <> NEW.body
    OR OLD.github_review_id <> NEW.github_review_id
    OR OLD.auto_inject_review <> NEW.auto_inject_review
BEGIN
    INSERT INTO change_log (project_id, session_id, event_type, payload, created_at)
    VALUES (
        (SELECT project_id FROM sessions WHERE id = NEW.session_id),
        NEW.session_id,
        'review_run_updated',
        json_object(
            'id', NEW.id,
            'reviewId', NEW.review_id,
            'sessionId', NEW.session_id,
            'pr', NEW.pr_url,
            'targetSha', NEW.target_sha,
            'status', NEW.status,
            'verdict', NEW.verdict,
            'triggerSource', NEW.trigger_source,
            'githubReviewId', NEW.github_review_id,
            'autoInjectReview', json(CASE WHEN NEW.auto_inject_review THEN 'true' ELSE 'false' END)
        ),
        datetime('now'));
END;;

CREATE TRIGGER session_cleanup_facts_cdc_insert
AFTER INSERT ON session_cleanup_facts
BEGIN
    INSERT INTO change_log (project_id, session_id, event_type, payload, created_at)
    VALUES ((SELECT project_id FROM sessions WHERE id = NEW.session_id), NEW.session_id, 'session_updated',
        json_object('id', NEW.session_id),
        datetime('now'));
END;;

CREATE TRIGGER session_cleanup_facts_cdc_update
AFTER UPDATE ON session_cleanup_facts
WHEN OLD.workspace_disposition <> NEW.workspace_disposition
    OR (OLD.runtime_released_at IS NULL) <> (NEW.runtime_released_at IS NULL)
BEGIN
    INSERT INTO change_log (project_id, session_id, event_type, payload, created_at)
    VALUES ((SELECT project_id FROM sessions WHERE id = NEW.session_id), NEW.session_id, 'session_updated',
        json_object('id', NEW.session_id),
        datetime('now'));
END;;

CREATE TRIGGER session_interface_transitions_cdc_insert
AFTER INSERT ON session_interface_transitions
BEGIN
    INSERT INTO change_log (project_id, session_id, event_type, payload, created_at)
    SELECT s.project_id, s.id, 'session_updated',
           json_object('id', s.id, 'sessionId', s.id,
                       'interfaceTransitionId', NEW.id,
                       'interfaceTransitionPhase', NEW.phase,
                       'activity', s.activity_state,
                       'isTerminated', json(CASE WHEN s.is_terminated THEN 'true' ELSE 'false' END)),
           NEW.updated_at
    FROM sessions s WHERE s.id = NEW.session_id;
END;;

CREATE TRIGGER session_interface_transitions_cdc_update
AFTER UPDATE ON session_interface_transitions
WHEN OLD.phase <> NEW.phase
    OR OLD.error_code <> NEW.error_code
    OR OLD.error_detail <> NEW.error_detail
    OR OLD.notice_acknowledged_at IS NOT NEW.notice_acknowledged_at
BEGIN
    INSERT INTO change_log (project_id, session_id, event_type, payload, created_at)
    SELECT s.project_id, s.id, 'session_updated',
           json_object('id', s.id, 'sessionId', s.id,
                       'interfaceTransitionId', NEW.id,
                       'interfaceTransitionPhase', NEW.phase,
                       'activity', s.activity_state,
                       'isTerminated', json(CASE WHEN s.is_terminated THEN 'true' ELSE 'false' END)),
           COALESCE(NEW.notice_acknowledged_at, NEW.updated_at)
    FROM sessions s WHERE s.id = NEW.session_id;
END;;

CREATE TRIGGER sessions_cdc_insert
AFTER INSERT ON sessions
BEGIN
    INSERT INTO change_log (project_id, session_id, event_type, payload, created_at)
    VALUES (NEW.project_id, NEW.id, 'session_created',
        json_object('id', NEW.id, 'activity', NEW.activity_state, 'isTerminated', json(CASE WHEN NEW.is_terminated THEN 'true' ELSE 'false' END)),
        NEW.updated_at);
END;;

CREATE TRIGGER sessions_cdc_update
AFTER UPDATE ON sessions
WHEN OLD.activity_state <> NEW.activity_state
    OR OLD.is_terminated <> NEW.is_terminated
    OR (OLD.first_signal_at IS NULL AND NEW.first_signal_at IS NOT NULL)
    OR OLD.preview_url <> NEW.preview_url
    OR OLD.preview_revision <> NEW.preview_revision
    OR OLD.display_name <> NEW.display_name
    OR OLD.terminate_on_pr_merge <> NEW.terminate_on_pr_merge
    OR OLD.is_pinned <> NEW.is_pinned
    OR OLD.pinned_at <> NEW.pinned_at
    OR (OLD.pinned_at IS NULL AND NEW.pinned_at IS NOT NULL)
    OR (OLD.pinned_at IS NOT NULL AND NEW.pinned_at IS NULL)
    OR OLD.session_mode <> NEW.session_mode
    OR OLD.auto_inject_review <> NEW.auto_inject_review
    OR OLD.auto_review_enabled <> NEW.auto_review_enabled
    OR OLD.harness <> NEW.harness
    OR OLD.runtime_launch_id <> NEW.runtime_launch_id
    OR OLD.agent_session_id <> NEW.agent_session_id
    OR OLD.native_transcript_path <> NEW.native_transcript_path
    OR OLD.auto_inject_ci <> NEW.auto_inject_ci
    OR OLD.latest_user_prompt_at IS NOT NEW.latest_user_prompt_at
BEGIN
    INSERT INTO change_log (project_id, session_id, event_type, payload, created_at)
    VALUES (NEW.project_id, NEW.id, 'session_updated',
        json_object(
            'id', NEW.id,
            'activity', NEW.activity_state,
            'isTerminated', json(CASE WHEN NEW.is_terminated THEN 'true' ELSE 'false' END),
            'terminateOnPrMerge', json(CASE WHEN NEW.terminate_on_pr_merge THEN 'true' ELSE 'false' END),
            'previewUrl', NEW.preview_url,
            'previewRevision', NEW.preview_revision,
            'isPinned', json(CASE WHEN NEW.is_pinned THEN 'true' ELSE 'false' END),
            'mode', NEW.session_mode,
            'autoInjectReview', json(CASE WHEN NEW.auto_inject_review THEN 'true' ELSE 'false' END),
            'autoInjectCI', json(CASE WHEN NEW.auto_inject_ci THEN 'true' ELSE 'false' END),
            'autoReviewEnabled', json(CASE WHEN NEW.auto_review_enabled THEN 'true' ELSE 'false' END)
        ),
        NEW.updated_at);
END;;

CREATE TRIGGER usage_bindings_cdc_insert AFTER INSERT ON usage_bindings BEGIN
    INSERT INTO change_log (project_id, session_id, event_type, payload, created_at)
    VALUES ((SELECT project_id FROM sessions WHERE id = NEW.session_id),
            NEW.session_id, 'session_updated', json_object('id', NEW.session_id), NEW.updated_at);
END;;

CREATE TRIGGER usage_bindings_cdc_update AFTER UPDATE ON usage_bindings BEGIN
    INSERT INTO change_log (project_id, session_id, event_type, payload, created_at)
    VALUES ((SELECT project_id FROM sessions WHERE id = NEW.session_id),
            NEW.session_id, 'session_updated', json_object('id', NEW.session_id), NEW.updated_at);
END;;

CREATE TRIGGER usage_sources_cdc_update AFTER UPDATE ON usage_sources
WHEN OLD.anomaly_count IS NOT NEW.anomaly_count
  OR OLD.last_error_code IS NOT NEW.last_error_code
BEGIN
    INSERT INTO change_log (project_id, session_id, event_type, payload, created_at)
    SELECT s.project_id, ub.session_id, 'session_updated', json_object('id', ub.session_id), NEW.updated_at
    FROM usage_bindings ub JOIN sessions s ON s.id = ub.session_id WHERE ub.id = NEW.binding_id;
END;;
-- +goose StatementEnd
