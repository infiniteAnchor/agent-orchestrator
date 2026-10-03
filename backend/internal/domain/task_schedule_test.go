package domain

import (
	"fmt"
	"testing"
)

func TestDerivedReadyUsesVerifiedResultsOnly(t *testing.T) {
	t.Parallel()
	nodes := []ScheduleNode{
		{ID: "root", State: TaskStateQueued, Position: 0},
		{ID: "child", State: TaskStateQueued, Position: 1, DependsOn: []string{"root"}},
		{ID: "done-unverified", State: TaskStateCompleted, Position: 2},
		{ID: "after-unverified", State: TaskStateQueued, Position: 3, DependsOn: []string{"done-unverified"}},
		{ID: "running-verified", State: TaskStateRunning, Position: 4},
		{ID: "after-verified", State: TaskStateQueued, Position: 5, DependsOn: []string{"running-verified"}},
	}
	verified := map[string]struct{}{"running-verified": {}}

	got := DerivedReady(nodes, verified)
	want := []string{"root", "after-verified"}
	if len(got) != len(want) {
		t.Fatalf("ready = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("ready = %v, want %v", got, want)
		}
	}
}

func TestDerivedReadyKeepsDeepChainsIterative(t *testing.T) {
	t.Parallel()
	const n = 64
	nodes := make([]ScheduleNode, n)
	for i := 0; i < n; i++ {
		nodes[i] = ScheduleNode{ID: fmt.Sprintf("t-%02d", i), State: TaskStateQueued, Position: i}
		if i > 0 {
			nodes[i].DependsOn = []string{nodes[i-1].ID}
		}
	}
	got := DerivedReady(nodes, map[string]struct{}{})
	if len(got) != 1 || got[0] != nodes[0].ID {
		t.Fatalf("only the root is ready, got %v", got)
	}
	verified := map[string]struct{}{}
	for i := 0; i < n-1; i++ {
		nodes[i].State = TaskStateCompleted
		verified[nodes[i].ID] = struct{}{}
	}
	got = DerivedReady(nodes, verified)
	if len(got) != 1 || got[0] != nodes[n-1].ID {
		t.Fatalf("tail ready = %v, want %s", got, nodes[n-1].ID)
	}
}

func TestClaimRefusalNamesTheFailingConstraint(t *testing.T) {
	t.Parallel()
	limits := ScheduleLimits{PerProject: 2, PerHarness: 1}
	node := ScheduleNode{PlanID: "p", ID: "t", State: TaskStateQueued, WorkspaceKey: "ws-a", Harness: "codex", DependsOn: []string{"dep"}}

	cases := []struct {
		name     string
		node     ScheduleNode
		verified map[string]struct{}
		active   []ScheduleNode
		limits   ScheduleLimits
		want     string
	}{
		{
			name: "dependency before capacity",
			node: node, limits: limits, want: ClaimDependency,
		},
		{
			name:     "shared empty workspace",
			node:     ScheduleNode{PlanID: "p", ID: "t", State: TaskStateQueued, Harness: "codex"},
			verified: map[string]struct{}{},
			active: []ScheduleNode{{
				PlanID: "p", ID: "other", State: TaskStateRunning, Harness: "grok",
			}},
			limits: ScheduleLimits{PerProject: 2, PerHarness: 2},
			want:   ClaimWorkspace,
		},
		{
			name:     "project limit when harnesses differ",
			node:     ScheduleNode{PlanID: "p", ID: "t", State: TaskStateQueued, WorkspaceKey: "ws-t", Harness: "codex"},
			verified: map[string]struct{}{},
			active: []ScheduleNode{
				{PlanID: "p", ID: "a", State: TaskStateRunning, WorkspaceKey: "ws-a", Harness: "grok"},
				{PlanID: "other", ID: "b", State: TaskStateBlocked, WorkspaceKey: "ws-b", Harness: "amp"},
			},
			limits: ScheduleLimits{PerProject: 2, PerHarness: 2},
			want:   ClaimProjectLimit,
		},
		{
			name:     "harness limit is not the project limit",
			node:     ScheduleNode{PlanID: "p", ID: "t", State: TaskStateQueued, WorkspaceKey: "ws-t", Harness: "codex"},
			verified: map[string]struct{}{},
			active: []ScheduleNode{
				{PlanID: "p", ID: "a", State: TaskStateClaimed, WorkspaceKey: "ws-a", Harness: "codex"},
			},
			limits: ScheduleLimits{PerProject: 2, PerHarness: 1},
			want:   ClaimHarnessLimit,
		},
		{
			name:     "isolated keys under the caps",
			node:     ScheduleNode{PlanID: "p", ID: "t", State: TaskStateQueued, WorkspaceKey: "ws-t", Harness: "codex"},
			verified: map[string]struct{}{},
			active: []ScheduleNode{
				{PlanID: "p", ID: "a", State: TaskStateRunning, WorkspaceKey: "ws-a", Harness: "grok"},
			},
			limits: ScheduleLimits{PerProject: 2, PerHarness: 2},
			want:   "",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			// Copy the active slice so a future mutation cannot leak across cases.
			active := append([]ScheduleNode(nil), tc.active...)
			if got := ClaimRefusal(tc.node, tc.verified, active, tc.limits); got != tc.want {
				t.Fatalf("refusal = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestAdvancePathsStayInsideTheTransitionTables(t *testing.T) {
	t.Parallel()
	taskPairs := [][2]TaskState{
		{TaskStateClaimed, TaskStateRunning},
		{TaskStateClaimed, TaskStateCompleted},
		{TaskStateRunning, TaskStateCompleted},
		{TaskStateCollecting, TaskStateCompleted},
		{TaskStateBlocked, TaskStateCompleted},
		{TaskStateQueued, TaskStateBlocked},
		{TaskStateFailed, TaskStateCompleted},
	}
	for _, pair := range taskPairs {
		steps, ok := CheckAdvanceTask(pair[0], pair[1])
		legal := pair[0] != TaskStateFailed
		if ok != legal {
			t.Fatalf("AdvanceTask(%s, %s) ok=%v steps=%v", pair[0], pair[1], ok, steps)
		}
	}
	if _, ok := CheckAdvanceAttempt(TaskAttemptStateRunning, TaskAttemptStateCompleted); ok {
		t.Fatal("an attempt must pass through collecting before it can complete")
	}
	if _, ok := CheckAdvanceAttempt(TaskAttemptStateCollecting, TaskAttemptStateCompleted); !ok {
		t.Fatal("collecting to completed is the verification edge")
	}
}

func TestScheduleLimitsDoNotBorrowEachOthersBound(t *testing.T) {
	t.Parallel()
	got := (ScheduleLimits{PerProject: 100, PerHarness: 0}).Normalized()
	if got.PerProject != MaxScheduleLimit {
		t.Fatalf("project cap = %d, want %d", got.PerProject, MaxScheduleLimit)
	}
	if got.PerHarness != DefaultHarnessTaskLimit {
		t.Fatalf("unset harness cap = %d, want the harness default %d", got.PerHarness, DefaultHarnessTaskLimit)
	}
}

func TestGraphRejectsWorkspaceAndHarnessIdentity(t *testing.T) {
	t.Parallel()
	base := TaskPlan{
		ID: "plan", ProjectID: "proj", Title: "T",
		Tasks: []PlannedTask{{ID: "t", Title: "T", Prompt: "P"}},
	}
	cases := []struct {
		name string
		edit func(*TaskPlan)
	}{
		{"workspace path", func(p *TaskPlan) { p.Tasks[0].WorkspaceKey = "/tmp/ws" }},
		{"workspace traversal", func(p *TaskPlan) { p.Tasks[0].WorkspaceKey = ".." }},
		{"workspace whitespace", func(p *TaskPlan) { p.Tasks[0].WorkspaceKey = " ws" }},
		{"unknown harness", func(p *TaskPlan) { p.Tasks[0].Harness = "not-a-harness" }},
		{"harness whitespace", func(p *TaskPlan) { p.Tasks[0].Harness = "codex " }},
	}
	okPlan := base
	okPlan.Tasks = []PlannedTask{{
		ID: "t", Title: "T", Prompt: "P", WorkspaceKey: "ws-a", Harness: string(HarnessCodex),
	}}
	if err := okPlan.Validate(); err != nil {
		t.Fatalf("opaque key and known harness should validate: %v", err)
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			plan := base
			plan.Tasks = append([]PlannedTask(nil), base.Tasks...)
			tc.edit(&plan)
			if err := plan.Validate(); err == nil {
				t.Fatal("expected validation to fail")
			}
		})
	}
}
