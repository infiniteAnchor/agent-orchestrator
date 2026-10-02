package sqlite

import (
	"database/sql"
	"strings"
	"testing"
)

// Schema-level guarantees for the durable task graph. These are asserted against
// raw SQL because they are properties of the database, not of any store method:
// a future caller that bypasses the store must still meet them.
//
// The subtests share one migrated database because each full migration chain is
// expensive; they use disjoint rows so they cannot interfere.

// Adding event types means rebuilding change_log (SQLite cannot ALTER a CHECK
// constraint) and recreating every CDC trigger, since dropping the table drops
// the triggers that reference it. This pins the result of that rebuild: the
// widened vocabulary is accepted, an unknown type is still refused, the
// pre-existing capture triggers survived, results cannot be rewritten, and the
// update triggers stay quiet unless something actually moved.
func TestTaskGraphSchema(t *testing.T) {
	db := openMigratedTestDB(t)
	seedProjectRow(t, db, "proj-1")
	seedStoredTaskGraph(t, db, "proj-1", "plan-1")
	seedStoredTaskGraph(t, db, "proj-1", "plan-2")

	countEvents := func(eventType string) int {
		t.Helper()
		var n int
		if err := db.QueryRow(`SELECT COUNT(*) FROM change_log WHERE event_type = ?`, eventType).Scan(&n); err != nil {
			t.Fatalf("count %s: %v", eventType, err)
		}
		return n
	}

	t.Run("change_log admits the task lifecycle vocabulary", func(t *testing.T) {
		for _, eventType := range []string{
			"task_plan_created",
			"task_created",
			"task_updated",
			"task_attempt_created",
			"task_attempt_updated",
			"task_result_recorded",
		} {
			if _, err := db.Exec(
				`INSERT INTO change_log (project_id, session_id, event_type, payload) VALUES (?, NULL, ?, '{}')`,
				"proj-1", eventType,
			); err != nil {
				t.Fatalf("change_log must admit %s: %v", eventType, err)
			}
		}

		// The CHECK constraint still has to reject anything outside the
		// vocabulary, or a typo in a trigger would be stored as a valid event
		// nobody handles.
		if _, err := db.Exec(
			`INSERT INTO change_log (project_id, session_id, event_type, payload) VALUES (?, NULL, 'task_not_a_real_event', '{}')`,
			"proj-1",
		); err == nil {
			t.Fatal("change_log accepted an unknown event type")
		}

		// Project-level task events have no session, which is exactly what the
		// nullable session_id column is for.
		var sessionID sql.NullString
		if err := db.QueryRow(
			`SELECT session_id FROM change_log WHERE event_type = 'task_plan_created'`,
		).Scan(&sessionID); err != nil {
			t.Fatalf("read task_plan_created: %v", err)
		}
		if sessionID.Valid {
			t.Fatalf("task_plan_created must have a NULL session, got %q", sessionID.String)
		}

		// The session-attached capture triggers must survive the table swap.
		for _, trigger := range []string{
			"sessions_cdc_insert",
			"sessions_cdc_update",
			"pr_cdc_update",
			"pr_checks_cdc_update",
			"review_run_cdc_insert",
			"task_plan_cdc_insert",
			"task_cdc_insert",
			"task_attempt_cdc_insert",
			"task_result_cdc_insert",
		} {
			var name string
			err := db.QueryRow(`SELECT name FROM sqlite_master WHERE type='trigger' AND name = ?`, trigger).Scan(&name)
			if err != nil {
				t.Fatalf("trigger %s missing after the change_log rebuild: %v", trigger, err)
			}
		}
	})

	// Results are evidence, so the database itself refuses to rewrite or remove
	// one. Enforced by triggers rather than convention so no future code path can
	// quietly mutate a result that already unlocked dependent work.
	t.Run("task results are append-only", func(t *testing.T) {
		seedStoredResult(t, db, "result-append-only")

		if _, err := db.Exec(`UPDATE task_result SET outcome = 'failed' WHERE id = 'result-append-only'`); err == nil {
			t.Fatal("task_result accepted an UPDATE: results must be append-only")
		}
		if _, err := db.Exec(`DELETE FROM task_result WHERE id = 'result-append-only'`); err == nil {
			t.Fatal("task_result accepted a DELETE: results must be append-only")
		}

		var outcome string
		if err := db.QueryRow(`SELECT outcome FROM task_result WHERE id = 'result-append-only'`).Scan(&outcome); err != nil {
			t.Fatalf("read result after refused mutations: %v", err)
		}
		if outcome != "verified" {
			t.Fatalf("result outcome changed to %q", outcome)
		}
	})

	// Consequences worth pinning: a plan that owns recorded evidence cannot be
	// destroyed at all. The plan delete cascades to its tasks and attempts and
	// then stops on the result's foreign keys; deleting the result directly is
	// refused by the append-only trigger. Either way the evidence survives.
	t.Run("a plan with recorded evidence cannot be deleted", func(t *testing.T) {
		seedStoredTaskGraph(t, db, "proj-1", "plan-with-result")
		if _, err := db.Exec(
			`INSERT INTO task_attempt (id, plan_id, task_id, attempt_number, state, claimed_at, created_at, updated_at)
			 VALUES ('attempt-evidenced', 'plan-with-result', 't1', 1, 'claimed', datetime('now'), datetime('now'), datetime('now'))`,
		); err != nil {
			t.Fatalf("seed attempt: %v", err)
		}
		if _, err := db.Exec(
			`INSERT INTO task_result (id, plan_id, task_id, attempt_id, outcome, evidence, recorded_at)
			 VALUES ('result-evidenced', 'plan-with-result', 't1', 'attempt-evidenced', 'verified', '{}', datetime('now'))`,
		); err != nil {
			t.Fatalf("seed result: %v", err)
		}
		_, err := db.Exec(`DELETE FROM task_plan WHERE id = 'plan-with-result'`)
		if err == nil {
			t.Fatal("deleting a plan with recorded evidence must be refused while that evidence exists")
		}
		// The refusal comes from the result's foreign keys, which have no ON
		// DELETE action: the plan delete cascades to tasks and attempts, and
		// stops dead there rather than reaching the append-only trigger. Deleting
		// the result directly is refused by that trigger instead.
		if !strings.Contains(err.Error(), "FOREIGN KEY") {
			t.Fatalf("expected the surviving evidence to block the delete via a foreign key, got %v", err)
		}
	})

	// The update triggers are guarded on real change, so a write that does not
	// move the lifecycle emits nothing. Without this, a re-read or re-probe would
	// look like progress to every live client.
	t.Run("update triggers ignore unchanged state", func(t *testing.T) {
		seedStoredAttempt(t, db, "attempt-quiet")
		// Count deltas, not absolutes: sibling subtests seed change_log rows
		// directly (including of these very types) to exercise the vocabulary.
		beforeTask, beforeAttempt := countEvents("task_updated"), countEvents("task_attempt_updated")

		if _, err := db.Exec(`UPDATE task SET state = 'queued' WHERE plan_id = 'plan-1' AND id = 't1'`); err != nil {
			t.Fatalf("same-state task update: %v", err)
		}
		if _, err := db.Exec(`UPDATE task SET title = 'Renamed' WHERE plan_id = 'plan-1' AND id = 't1'`); err != nil {
			t.Fatalf("unrelated task update: %v", err)
		}
		if _, err := db.Exec(`UPDATE task_attempt SET state = 'claimed' WHERE id = 'attempt-quiet'`); err != nil {
			t.Fatalf("same-state attempt update: %v", err)
		}
		if got := countEvents("task_updated") - beforeTask; got != 0 {
			t.Fatalf("unchanged task writes emitted %d task_updated events", got)
		}
		if got := countEvents("task_attempt_updated") - beforeAttempt; got != 0 {
			t.Fatalf("unchanged attempt writes emitted %d task_attempt_updated events", got)
		}

		// A real state change does emit, so the guard suppresses only no-ops.
		if _, err := db.Exec(`UPDATE task SET state = 'claimed' WHERE plan_id = 'plan-1' AND id = 't1'`); err != nil {
			t.Fatalf("task state change: %v", err)
		}
		if got := countEvents("task_updated") - beforeTask; got != 1 {
			t.Fatalf("a real task state change emitted %d events, want 1", got)
		}
	})

	// The graph is self-contained at the schema level: an edge cannot cross
	// plans, because both endpoints resolve against the same plan's task rows.
	t.Run("cross-plan edges and self-edges are unrepresentable", func(t *testing.T) {
		if _, err := db.Exec(
			`INSERT INTO task_dependency (plan_id, task_id, depends_on_task_id, position) VALUES ('plan-1', 't1', 'other', 0)`,
		); err == nil {
			t.Fatal("a dependency on a task outside the plan must be refused")
		}
		if _, err := db.Exec(
			`INSERT INTO task_dependency (plan_id, task_id, depends_on_task_id, position) VALUES ('plan-1', 't1', 't1', 0)`,
		); err == nil {
			t.Fatal("a self-dependency must be refused by the CHECK constraint")
		}

		if _, err := db.Exec(
			`INSERT INTO task_phase (plan_id, id, title, position) VALUES ('plan-2', 'p1', 'Phase', 0)`,
		); err != nil {
			t.Fatalf("seed phase in plan-2: %v", err)
		}
		if _, err := db.Exec(`UPDATE task SET phase_id = 'p1' WHERE plan_id = 'plan-1' AND id = 't1'`); err == nil {
			t.Fatal("a task must not adopt another plan's phase")
		}
	})
}

// The widened table must still roll back cleanly: a daemon that downgrades has
// to shed the new vocabulary without losing the events it already captured.
func TestTaskGraphSchema_DownMigrationShedsTaskVocabularyAndTables(t *testing.T) {
	db := openMigrationFixture(t, 128, pragmas)
	if _, err := db.Exec(`INSERT INTO projects (id, path, registered_at) VALUES ('proj-1', '/tmp/proj-1', datetime('now'))`); err != nil {
		t.Fatalf("seed project: %v", err)
	}
	if _, err := db.Exec(
		`INSERT INTO change_log (project_id, session_id, event_type, payload) VALUES ('proj-1', NULL, 'session_created', '{}')`,
	); err != nil {
		t.Fatalf("seed pre-existing event: %v", err)
	}

	upTo(t, db, 129)
	if _, err := db.Exec(
		`INSERT INTO change_log (project_id, session_id, event_type, payload) VALUES ('proj-1', NULL, 'task_plan_created', '{}')`,
	); err != nil {
		t.Fatalf("seed task event: %v", err)
	}

	downTo(t, db, 128)

	for _, table := range []string{"task_plan", "task_phase", "task", "task_dependency", "task_verification_command", "task_attempt", "task_result"} {
		var name string
		if err := db.QueryRow(`SELECT name FROM sqlite_master WHERE type='table' AND name = ?`, table).Scan(&name); err == nil {
			t.Fatalf("table %s survived the down migration", table)
		}
	}

	// The new event type is gone from the log and the narrowed CHECK constraint
	// refuses it again, while the events captured before still replay.
	var remaining int
	if err := db.QueryRow(`SELECT COUNT(*) FROM change_log WHERE event_type = 'session_created'`).Scan(&remaining); err != nil {
		t.Fatalf("count surviving events: %v", err)
	}
	if remaining != 1 {
		t.Fatalf("pre-existing events must survive the down migration, got %d", remaining)
	}
	if _, err := db.Exec(
		`INSERT INTO change_log (project_id, session_id, event_type, payload) VALUES ('proj-1', NULL, 'task_plan_created', '{}')`,
	); err == nil {
		t.Fatal("the narrowed change_log must reject task event types again")
	}

	// Capture triggers are back in place.
	var name string
	if err := db.QueryRow(`SELECT name FROM sqlite_master WHERE type='trigger' AND name = 'sessions_cdc_insert'`).Scan(&name); err != nil {
		t.Fatalf("sessions_cdc_insert missing after the down migration: %v", err)
	}
}

func seedProjectRow(t *testing.T, db *sql.DB, id string) {
	t.Helper()
	if _, err := db.Exec(
		`INSERT INTO projects (id, path, registered_at) VALUES (?, ?, datetime('now'))`, id, "/tmp/"+id,
	); err != nil {
		t.Fatalf("seed project %s: %v", id, err)
	}
}

func seedStoredTaskGraph(t *testing.T, db *sql.DB, projectID, planID string) {
	t.Helper()
	if _, err := db.Exec(
		`INSERT INTO task_plan (id, project_id, title, created_at, updated_at) VALUES (?, ?, 'Plan', datetime('now'), datetime('now'))`,
		planID, projectID,
	); err != nil {
		t.Fatalf("seed task plan %s: %v", planID, err)
	}
	if _, err := db.Exec(
		`INSERT INTO task (plan_id, id, title, prompt, position, state, created_at, updated_at)
		 VALUES (?, 't1', 'One', 'do it', 0, 'queued', datetime('now'), datetime('now'))`,
		planID,
	); err != nil {
		t.Fatalf("seed task in %s: %v", planID, err)
	}
}

func seedStoredAttempt(t *testing.T, db *sql.DB, attemptID string) {
	t.Helper()
	if _, err := db.Exec(
		`INSERT INTO task_attempt (id, plan_id, task_id, attempt_number, state, claimed_at, created_at, updated_at)
		 VALUES (?, 'plan-1', 't1',
		         (SELECT COALESCE(MAX(attempt_number), 0) + 1 FROM task_attempt WHERE plan_id = 'plan-1' AND task_id = 't1'),
		         'claimed', datetime('now'), datetime('now'), datetime('now'))`,
		attemptID,
	); err != nil {
		t.Fatalf("seed attempt %s: %v", attemptID, err)
	}
}

func seedStoredResult(t *testing.T, db *sql.DB, resultID string) {
	t.Helper()
	attemptID := "attempt-" + resultID
	seedStoredAttempt(t, db, attemptID)
	if _, err := db.Exec(
		`INSERT INTO task_result (id, plan_id, task_id, attempt_id, outcome, evidence, recorded_at)
		 VALUES (?, 'plan-1', 't1', ?, 'verified', '{}', datetime('now'))`,
		resultID, attemptID,
	); err != nil {
		t.Fatalf("seed result %s: %v", resultID, err)
	}
}
