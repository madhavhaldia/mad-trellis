package launcher

import "testing"

// no-goals / no-dispatch (Inv 13, [GATED]): the launcher's own surface must not
// sprout a task/goal/dispatch affordance. AuditNoGoals is the mechanical check;
// the positive control proves it is non-vacuous.

func TestAuditNoGoalsClearsBenignFlags(t *testing.T) {
	benign := []string{"socket", "ports", "--verbose", "dir"}
	if off := AuditNoGoals(benign); len(off) != 0 {
		t.Fatalf("benign flags must pass the no-goals audit; flagged %v", off)
	}
}

func TestAuditNoGoalsCatchesGoalAffordances(t *testing.T) {
	for _, bad := range []string{"goal", "--goal", "task", "spawn-task", "spawn_task", "spawnTask", "DISPATCH", "prompt"} {
		if off := AuditNoGoals([]string{"socket", bad}); len(off) == 0 {
			t.Errorf("a goal/dispatch affordance %q must be flagged", bad)
		}
	}
}

// POSITIVE CONTROL: injecting the forbidden artifact (a --goal flag onto an
// otherwise-clean flag set) must turn the audit RED. If this passed silently the
// negative obligation would be vacuous.
func TestAuditNoGoalsPositiveControl(t *testing.T) {
	clean := []string{"socket", "ports"}
	if len(AuditNoGoals(clean)) != 0 {
		t.Fatal("control setup is wrong: the clean set should pass")
	}
	withGoal := append(append([]string{}, clean...), "goal")
	if len(AuditNoGoals(withGoal)) == 0 {
		t.Fatal("REGRESSION: the no-goals audit did not catch an injected --goal flag")
	}
}
