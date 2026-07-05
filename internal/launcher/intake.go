package launcher

import "strings"

// GoalDispatchFlagNames is the denylist of affordance names that would turn the
// launcher from a transparent session WRAPPER into a task DISPATCHER — which is
// exactly what mad-trellis must never become (Inv 13: "takes no goals and
// dispatches no tasks"; docs/0003 12-intake: keep the Layer-2 task-intake seam
// EMPTY). The launcher CLI is audited against this list at test time, with a
// positive control (adding such a flag turns AuditNoGoals non-empty → the
// no-dispatch test goes RED), so the negative obligation is non-vacuous.
//
// The unit of interaction is the SESSION (the user drives it live), never the
// GOAL. The agent's own flags are opaque pass-through and are NOT audited here —
// this guards mad-trellis's OWN surface from sprouting a dispatch affordance.
var GoalDispatchFlagNames = []string{
	"goal", "task", "prompt", "objective", "instruction", "assign", "dispatch", "spawn-task", "todo",
}

// AuditNoGoals returns the subset of flagNames that name a goal/dispatch
// affordance. A governed launcher command MUST return an empty slice. Matching
// is case-insensitive and tolerant of separators (goal, --goal, spawn_task,
// spawnTask all match their denylist entry) so a renamed-but-equivalent flag
// cannot slip past.
func AuditNoGoals(flagNames []string) []string {
	var offending []string
	for _, raw := range flagNames {
		norm := normalizeFlag(raw)
		for _, bad := range GoalDispatchFlagNames {
			if norm == normalizeFlag(bad) {
				offending = append(offending, raw)
				break
			}
		}
	}
	return offending
}

// normalizeFlag lowercases and strips leading dashes and internal separators so
// "--spawn-task", "spawn_task", and "spawnTask" compare equal.
func normalizeFlag(s string) string {
	s = strings.ToLower(strings.TrimLeft(s, "-"))
	s = strings.ReplaceAll(s, "-", "")
	s = strings.ReplaceAll(s, "_", "")
	return s
}
