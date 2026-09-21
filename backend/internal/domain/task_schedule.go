package domain

// Schedule limits and the derived ready queue. Readiness is not a stored task
// state: it is computed from queued tasks plus verified results. Capacity and
// workspace conflicts are claim-time refusals, not a second stored status.

const (
	// DefaultProjectTaskLimit is the per-project cap on tasks that are claimed,
	// running, collecting, or held for reconciliation.
	DefaultProjectTaskLimit = 2
	// DefaultHarnessTaskLimit is the same cap grouped by harness. An empty
	// harness shares one bucket so unspecified tasks cannot bypass the cap.
	DefaultHarnessTaskLimit = 2
	// MaxScheduleLimit is the largest cap a caller can raise either limit to.
	MaxScheduleLimit = 32

	// TaskAttemptDispatchLease is the runtime ref written before a launcher is
	// called. It is not a worker identity: recovery treats it as "launch may
	// have started" and must not replace the attempt.
	TaskAttemptDispatchLease = "dispatching"
)

// Claim refusal reasons. Empty means the task may be claimed.
const (
	ClaimMissing      = "missing"
	ClaimState        = "state"
	ClaimDependency   = "dependency"
	ClaimWorkspace    = "workspace"
	ClaimProjectLimit = "project_limit"
	ClaimHarnessLimit = "harness_limit"
	ClaimInactivePlan = "project"
)

// TaskScheduleView is the durable snapshot the ready queue derives from.
// Active includes in-flight tasks from every plan in the project.
type TaskScheduleView struct {
	ProjectID string
	Plan      TaskPlan
	Nodes     []ScheduleNode
	Attempts  []TaskAttempt
	Verified  map[string]struct{}
	Active    []ScheduleNode
}

// TaskClaimResult is one compare-and-swap claim. Reason is empty only when
// Attempt was inserted.
type TaskClaimResult struct {
	Attempt TaskAttempt
	Reason  string
}

// ScheduleLimits caps how many tasks in one project may be in flight.
type ScheduleLimits struct {
	PerProject int
	PerHarness int
}

// Normalized replaces non-positive limits with the defaults and clamps each
// cap at MaxScheduleLimit. The two caps are independent: a value meant for one
// must not be reused as the other.
func (l ScheduleLimits) Normalized() ScheduleLimits {
	if l.PerProject < 1 {
		l.PerProject = DefaultProjectTaskLimit
	}
	if l.PerHarness < 1 {
		l.PerHarness = DefaultHarnessTaskLimit
	}
	if l.PerProject > MaxScheduleLimit {
		l.PerProject = MaxScheduleLimit
	}
	if l.PerHarness > MaxScheduleLimit {
		l.PerHarness = MaxScheduleLimit
	}
	return l
}

// ScheduleNode is one task plus the durable facts the ready queue needs.
// DependsOn is plan-local. Active nodes from other plans in the same project
// carry an empty DependsOn and still count toward workspace and harness caps.
type ScheduleNode struct {
	PlanID       string
	ID           string
	State        TaskState
	DependsOn    []string
	WorkspaceKey string
	Harness      string
	Position     int
}

// ScheduleOccupies reports whether a stored state holds a concurrency slot.
// Blocked holds the slot so a replacement is not launched beside an attempt
// that recovery has not reconciled.
func ScheduleOccupies(state TaskState) bool {
	switch state {
	case TaskStateClaimed, TaskStateRunning, TaskStateCollecting, TaskStateBlocked:
		return true
	default:
		return false
	}
}

// DerivedReady returns queued task IDs in position order whose dependencies all
// have a verified result. The verified set is task IDs in the same plan.
// Upstream task state is not consulted: a verified result unlocks dependents
// even when the upstream worker is still running, and a completed task with no
// verified result does not.
func DerivedReady(nodes []ScheduleNode, verified map[string]struct{}) []string {
	type ranked struct {
		id       string
		position int
	}
	ready := make([]ranked, 0, len(nodes))
	for _, node := range nodes {
		if node.State != TaskStateQueued {
			continue
		}
		satisfied := true
		for _, dep := range node.DependsOn {
			if _, ok := verified[dep]; !ok {
				satisfied = false
				break
			}
		}
		if satisfied {
			ready = append(ready, ranked{id: node.ID, position: node.Position})
		}
	}
	// Insertion sort keeps the function allocation-free for the small graphs
	// the plan contract allows, and it does not depend on map iteration order.
	for i := 1; i < len(ready); i++ {
		item := ready[i]
		j := i
		for j > 0 && (ready[j-1].position > item.position || (ready[j-1].position == item.position && ready[j-1].id > item.id)) {
			ready[j] = ready[j-1]
			j--
		}
		ready[j] = item
	}
	out := make([]string, len(ready))
	for i, item := range ready {
		out[i] = item.id
	}
	return out
}

// ClaimRefusal reports why node cannot be claimed given the verified results
// and the tasks already occupying the project. An empty string means claim it.
// The first failing constraint wins, and each constraint names itself.
func ClaimRefusal(node ScheduleNode, verified map[string]struct{}, active []ScheduleNode, limits ScheduleLimits) string {
	if node.ID == "" {
		return ClaimMissing
	}
	if node.State != TaskStateQueued {
		return ClaimState
	}
	for _, dep := range node.DependsOn {
		if _, ok := verified[dep]; !ok {
			return ClaimDependency
		}
	}
	limits = limits.Normalized()
	projectActive := 0
	harnessActive := 0
	for _, other := range active {
		if !ScheduleOccupies(other.State) {
			continue
		}
		if other.PlanID == node.PlanID && other.ID == node.ID {
			continue
		}
		if other.WorkspaceKey == node.WorkspaceKey {
			return ClaimWorkspace
		}
		projectActive++
		if other.Harness == node.Harness {
			harnessActive++
		}
	}
	if projectActive >= limits.PerProject {
		return ClaimProjectLimit
	}
	if harnessActive >= limits.PerHarness {
		return ClaimHarnessLimit
	}
	return ""
}

// AdvanceTask is the explicit recovery walk from one task state to another.
// It refuses paths that would pass through the retry edge (failed → queued):
// repairing a lagged completion must not look like a new attempt.
func AdvanceTask(from, to TaskState) ([]TaskState, bool) {
	if !from.Valid() || !to.Valid() {
		return nil, false
	}
	if from == to {
		return nil, true
	}
	switch to {
	case TaskStateClaimed:
		if from == TaskStateQueued {
			return []TaskState{TaskStateClaimed}, true
		}
	case TaskStateRunning:
		switch from {
		case TaskStateClaimed, TaskStateBlocked:
			return []TaskState{TaskStateRunning}, true
		case TaskStateQueued:
			return []TaskState{TaskStateClaimed, TaskStateRunning}, true
		}
	case TaskStateCollecting:
		switch from {
		case TaskStateRunning:
			return []TaskState{TaskStateCollecting}, true
		case TaskStateClaimed:
			return []TaskState{TaskStateRunning, TaskStateCollecting}, true
		case TaskStateBlocked:
			return []TaskState{TaskStateCollecting}, true
		}
	case TaskStateCompleted:
		switch from {
		case TaskStateCollecting:
			return []TaskState{TaskStateCompleted}, true
		case TaskStateRunning:
			return []TaskState{TaskStateCollecting, TaskStateCompleted}, true
		case TaskStateClaimed:
			return []TaskState{TaskStateRunning, TaskStateCollecting, TaskStateCompleted}, true
		case TaskStateBlocked:
			return []TaskState{TaskStateCompleted}, true
		}
	case TaskStateFailed:
		switch from {
		case TaskStateClaimed, TaskStateRunning, TaskStateCollecting, TaskStateBlocked:
			return []TaskState{TaskStateFailed}, true
		}
	case TaskStateBlocked:
		switch from {
		case TaskStateClaimed, TaskStateRunning, TaskStateCollecting:
			return []TaskState{TaskStateBlocked}, true
		case TaskStateQueued:
			return []TaskState{TaskStateClaimed, TaskStateBlocked}, true
		}
	}
	return nil, false
}

// AdvanceAttempt is the attempt-shaped counterpart of AdvanceTask.
func AdvanceAttempt(from, to TaskAttemptState) ([]TaskAttemptState, bool) {
	if !from.Valid() || !to.Valid() {
		return nil, false
	}
	if from == to {
		return nil, true
	}
	switch to {
	case TaskAttemptStateRunning:
		switch from {
		case TaskAttemptStateClaimed, TaskAttemptStateBlocked:
			return []TaskAttemptState{TaskAttemptStateRunning}, true
		}
	case TaskAttemptStateCollecting:
		switch from {
		case TaskAttemptStateRunning:
			return []TaskAttemptState{TaskAttemptStateCollecting}, true
		case TaskAttemptStateClaimed:
			return []TaskAttemptState{TaskAttemptStateRunning, TaskAttemptStateCollecting}, true
		case TaskAttemptStateBlocked:
			return []TaskAttemptState{TaskAttemptStateCollecting}, true
		}
	case TaskAttemptStateCompleted:
		switch from {
		case TaskAttemptStateCollecting:
			return []TaskAttemptState{TaskAttemptStateCompleted}, true
		case TaskAttemptStateBlocked:
			return []TaskAttemptState{TaskAttemptStateCompleted}, true
		}
	case TaskAttemptStateFailed:
		switch from {
		case TaskAttemptStateClaimed, TaskAttemptStateRunning, TaskAttemptStateCollecting, TaskAttemptStateBlocked:
			return []TaskAttemptState{TaskAttemptStateFailed}, true
		}
	case TaskAttemptStateBlocked:
		switch from {
		case TaskAttemptStateClaimed, TaskAttemptStateRunning, TaskAttemptStateCollecting:
			return []TaskAttemptState{TaskAttemptStateBlocked}, true
		}
	}
	return nil, false
}

// CheckAdvanceAttempt checks every edge of AdvanceAttempt against the attempt table.
func CheckAdvanceAttempt(from, to TaskAttemptState) ([]TaskAttemptState, bool) {
	steps, ok := AdvanceAttempt(from, to)
	if !ok {
		return nil, false
	}
	cur := from
	for _, step := range steps {
		if !CanTransitionTaskAttempt(cur, step) {
			return nil, false
		}
		cur = step
	}
	return steps, true
}

// CheckAdvanceTask checks every edge of AdvanceTask against the task table.
func CheckAdvanceTask(from, to TaskState) ([]TaskState, bool) {
	steps, ok := AdvanceTask(from, to)
	if !ok {
		return nil, false
	}
	cur := from
	for _, step := range steps {
		if !CanTransitionTask(cur, step) {
			return nil, false
		}
		cur = step
	}
	return steps, true
}
