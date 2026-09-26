package domain

import "testing"

func TestDecideRetry(t *testing.T) {
	base := RetryInput{
		Outcome: TaskResultFailed, AttemptState: TaskAttemptStateFailed, HasResult: true,
		AttemptCount: 1, MaxAttempts: 2, CurrentHarness: "codex", FallbackHarness: "opencode",
		Reason: RetryReasonFailed,
	}
	tests := []struct {
		name    string
		mutate  func(*RetryInput)
		want    RetryAction
		harness string
	}{
		{name: "first failure retries the same harness", want: RetryActionRetry, harness: "codex"},
		{
			name:   "provider exhaustion uses the fallback",
			mutate: func(in *RetryInput) { in.Reason = RetryReasonProviderExhausted },
			want:   RetryActionFallback, harness: "opencode",
		},
		{
			name: "exhaustion on the fallback harness escalates at the cap",
			mutate: func(in *RetryInput) {
				in.Reason = RetryReasonProviderExhausted
				in.CurrentHarness = "opencode"
				in.AttemptCount = 2
			},
			want: RetryActionEscalate,
		},
		{
			name:   "attempt cap escalates",
			mutate: func(in *RetryInput) { in.AttemptCount = 2 },
			want:   RetryActionEscalate,
		},
		{
			name: "inconclusive holds",
			mutate: func(in *RetryInput) {
				in.Outcome = TaskResultInconclusive
				in.AttemptState = TaskAttemptStateBlocked
			},
			want: RetryActionHold,
		},
		{
			name:   "blocked attempt holds even when a failed outcome was supplied",
			mutate: func(in *RetryInput) { in.AttemptState = TaskAttemptStateBlocked },
			want:   RetryActionHold,
		},
		{
			name: "unresolved dispatch lease holds",
			mutate: func(in *RetryInput) {
				in.AttemptState = TaskAttemptStateClaimed
				in.RuntimeRef = TaskAttemptDispatchLease
				in.Outcome = ""
				in.HasResult = false
			},
			want: RetryActionHold,
		},
		{
			name: "confirmed launch failure may still carry the dispatch lease",
			mutate: func(in *RetryInput) {
				in.RuntimeRef = TaskAttemptDispatchLease
				in.HasResult = false
				in.Outcome = ""
			},
			want: RetryActionRetry, harness: "codex",
		},
		{
			name: "verified work is not retried",
			mutate: func(in *RetryInput) {
				in.Outcome = TaskResultVerified
				in.AttemptState = TaskAttemptStateCompleted
			},
			want: RetryActionNone,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			in := base
			if tt.mutate != nil {
				tt.mutate(&in)
			}
			got := DecideRetry(in)
			if got.Action != tt.want || got.Harness != tt.harness {
				t.Fatalf("DecideRetry() = %+v, want action %s harness %q", got, tt.want, tt.harness)
			}
		})
	}
}
