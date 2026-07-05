package conformance

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"

	"github.com/madhavhaldia/mad-substrate/internal/rpcclient"
)

// check_integration.go proves the Wing 3 integration plane is a CONVERGENCE-SCOPED
// review/verdict queue, NOT a general agent-to-agent messaging mesh (the thing
// mad-substrate must never have). The surface is integration.request / pending /
// claim / verdict / status / events: a BUILDER requests integration of its
// committed boundary branch, an INTEGRATOR claims + approves/rejects with feedback,
// and integration.events emits daemon-authored wake-ups. The danger of any new
// cross-session surface is that it smuggles in a free channel
// (broadcast/peer/relay/mesh) or prompt payload under cover of "integration". This
// check asserts the opposite, black-box through the PUBLIC daemon contract only:
//
//   1. The general agent-to-agent MESH is still ABSENT. A forbidden list of
//      mesh/broadcast/peer/relay/channel methods — including the integration.*-named
//      decoys (integration.broadcast/send/message/publish/emit/notify) — is NOT in
//      the registry: each is method-not-found (-32601). Adding integration.* must
//      not have created a messaging or event-authoring affordance.
//   2. The integration surface IS present and forms a CLOSED convergence loop that
//      exposes only branch-review METADATA. Driven end-to-end across two distinct
//      governed sessions: a builder integration.request{branch,title}; an integrator
//      integration.pending (each entry carries ONLY {id,branch,title,state,
//      created_at_ms} — no file contents, no working-tree path, no arbitrary payload
//      field), integration.claim{id}, integration.verdict{id,decision:"reject",
//      feedback}; then the builder's integration.status{branch} shows
//      changes_requested + the feedback. The ONLY cross-session information is a
//      submitted-branch review verdict, bounded to a claimed record — never a free
//      channel.
//   3. Identity is connection-bound (Inv 4). The mutating methods take NO
//      holder/claimer param: the requester is the request connection's session and
//      the claimer is the claim connection's session. Driving request on one
//      connection and claim on a SECOND proves the loop spans two distinct governed
//      identities through the QUEUE — it is not a direct session-to-session channel.
//   4. Events are DAEMON-AUTHORED WAKE-UPS, not state or prompt content. An event
//      reply is pinned to ONLY {id,kind,branch,created_at_ms}; a marker title is
//      visible in integration.pending (positive control) but absent from
//      integration.events. The durable request row remains the truth.
//   5. Branch-audience events are authorized: a third unrelated session polling
//      a branch receives no branch events, while the record holder receives the
//      verdict event. This proves the audience check is live, not merely that no
//      events were produced.
//
// CONTROL (non-vacuity, mirrors check_nodispatch.go): the registry-probe must be
// demonstrably able to tell PRESENT from ABSENT, else "no mesh method" is vacuously
// green. It asserts a real integration method (integration.request) RESOLVES (the
// positive) while a mesh method (session.send) returns method-not-found (the
// negative), and that a REGISTERED method's param error (integration.verdict with a
// bad decision → CodeInvalidParams) is NOT misclassified as method-absent — so a
// partially-wired mesh method could never pass as "absent".

func init() { RegisterCheck(integrationConvergenceScoped{}) }

type integrationConvergenceScoped struct{}

func (integrationConvergenceScoped) ID() string { return "integration-convergence-scoped" }
func (integrationConvergenceScoped) OwnerProject() string {
	return "convergence-plane (integration, Wing 3)"
}
func (integrationConvergenceScoped) Clause() string {
	return "integration plane is a convergence-scoped review queue, not a cross-agent mesh (Inv 1/4/13): integration.* exposes only branch-review metadata and payload-free daemon-authored events; no broadcast/peer/relay/channel/mesh/event-authoring affordance exists"
}

// meshMethods are general agent-to-agent messaging affordances that MUST NOT be in
// the public registry — their presence would be a free cross-agent channel (mesh,
// not star). The integration.broadcast/send/message decoys assert that adding the
// integration.* queue did not smuggle in any messaging surface under that prefix;
// integration.publish/emit/notify assert agents cannot author events. Each must
// return method-not-found (-32601). Kept self-contained to this file so the diff
// stays isolated (no shared list mutated).
var meshMethods = []string{
	"session.send", "session.recv", "session.broadcast",
	"broadcast.publish", "broadcast.subscribe",
	"relay.send", "relay.recv",
	"peer.connect", "peer.send", "peer.list",
	"mesh.join", "mesh.send", "channel.open",
	"integration.broadcast", "integration.send", "integration.message",
	"integration.publish", "integration.emit", "integration.notify",
}

// pendingMetadataFields is the EXACT key set an integration.pending entry may
// expose — review metadata only. Anything beyond this (file contents, a working-tree
// path, an arbitrary payload/body/data field) would turn the queue into a channel.
var pendingMetadataFields = []string{"id", "branch", "title", "state", "created_at_ms"}

// eventMetadataFields is the EXACT key set an integration.events entry may expose.
// It is deliberately smaller than integration.pending: events are wake-ups only,
// never state, title/body text, verdict feedback, or agent-authored payload.
var eventMetadataFields = []string{"id", "kind", "branch", "created_at_ms"}

func (c integrationConvergenceScoped) Run(s *Scratch) Result {
	// --- (1) The mesh is ABSENT. Every messaging-shaped method is method-not-found.
	mc, err := s.Dial()
	if err != nil {
		return fail(c, "mesh-probe dial: %v", err)
	}
	for _, m := range meshMethods {
		var out map[string]any
		err := mc.Call(m, map[string]any{}, &out)
		if err == nil {
			mc.Close()
			return fail(c, "MESH AFFORDANCE: the registry served %q (a cross-agent channel exists — mesh, not a convergence queue)", m)
		}
		if !isMethodNotFound(err) {
			mc.Close()
			return fail(c, "mesh method %q returned a non-not-found error (it may be partially wired): %v", m, err)
		}
	}
	mc.Close()

	// --- (2)+(3) The integration surface forms a CLOSED loop across TWO sessions.
	// The builder and integrator are DISTINCT connections → distinct connection-bound
	// identities. request is taken from the builder connection, claim from the
	// integrator connection (neither method accepts a holder/claimer param).
	builder, err := s.Dial()
	if err != nil {
		return fail(c, "builder dial: %v", err)
	}
	defer builder.Close()
	integrator, err := s.Dial()
	if err != nil {
		return fail(c, "integrator dial: %v", err)
	}
	defer integrator.Close()

	builderID, err := whoAmIOn(builder)
	if err != nil {
		return fail(c, "builder whoami: %v", err)
	}
	integratorID, err := whoAmIOn(integrator)
	if err != nil {
		return fail(c, "integrator whoami: %v", err)
	}
	if builderID == integratorID {
		return fail(c, "builder and integrator connections minted the SAME session %q (cannot prove the loop spans two governed identities)", builderID)
	}

	const branch = "nm/conform-x"
	const title = "conformance review fixture"
	const feedback = "tighten the error path before integration"

	// builder: request integration of its committed branch.
	var reqOut struct {
		ID    string `json:"id"`
		State string `json:"state"`
	}
	if err := builder.Call("integration.request", map[string]any{"branch": branch, "title": title}, &reqOut); err != nil {
		return fail(c, "integration.request: %v", err)
	}
	if reqOut.State != "requested" {
		return fail(c, "integration.request landed in state %q, want \"requested\"", reqOut.State)
	}
	if reqOut.ID != branch {
		return fail(c, "integration.request id %q != branch %q (the record is keyed by the submitted branch, not a free handle)", reqOut.ID, branch)
	}

	// integrator: list pending. Assert the branch appears AND each entry exposes ONLY
	// the review metadata — no file contents, no working-tree path, no payload field.
	var pend struct {
		Pending []map[string]json.RawMessage `json:"pending"`
	}
	if err := integrator.Call("integration.pending", map[string]any{}, &pend); err != nil {
		return fail(c, "integration.pending: %v", err)
	}
	entry, found := findPending(pend.Pending, branch)
	if !found {
		return fail(c, "integration.pending did not list the requested branch %q (the queue lost the request)", branch)
	}
	if extra := unexpectedFields(entry, pendingMetadataFields); len(extra) > 0 {
		return fail(c, "integration.pending entry exposed NON-metadata field(s) %v — the queue is leaking more than branch-review metadata (a channel, not a convergence queue); allowed=%v", extra, pendingMetadataFields)
	}
	if missing := missingFields(entry, pendingMetadataFields); len(missing) > 0 {
		return fail(c, "integration.pending entry missing required metadata field(s) %v (entry=%v)", missing, sortedKeys(entry))
	}

	// integrator: claim the requested record (CAS requested -> claimed; claimer is
	// the integrator connection's session, taken from the connection, not params).
	var claimOut struct {
		OK     bool   `json:"ok"`
		Branch string `json:"branch"`
		Title  string `json:"title"`
	}
	if err := integrator.Call("integration.claim", map[string]any{"id": branch}, &claimOut); err != nil {
		return fail(c, "integration.claim: %v", err)
	}
	if !claimOut.OK {
		return fail(c, "integration.claim of the pending record returned ok=false (the integrator could not claim its own queue's request)")
	}
	if claimOut.Branch != branch {
		return fail(c, "integration.claim returned branch %q, want %q", claimOut.Branch, branch)
	}

	// integrator: reject with feedback (CAS claimed -> changes_requested).
	var verdOut struct {
		OK    bool   `json:"ok"`
		State string `json:"state"`
	}
	if err := integrator.Call("integration.verdict", map[string]any{"id": branch, "decision": "reject", "feedback": feedback}, &verdOut); err != nil {
		return fail(c, "integration.verdict(reject): %v", err)
	}
	if !verdOut.OK || verdOut.State != "changes_requested" {
		return fail(c, "integration.verdict(reject) -> ok=%v state=%q, want ok=true state=\"changes_requested\"", verdOut.OK, verdOut.State)
	}

	// builder: poll status. The ONLY cross-session information that crossed is the
	// review verdict on the submitted branch + its feedback — bounded to the claimed
	// record, never a free message.
	var statOut struct {
		Found    bool   `json:"found"`
		ID       string `json:"id"`
		State    string `json:"state"`
		Feedback string `json:"feedback"`
	}
	if err := builder.Call("integration.status", map[string]any{"branch": branch}, &statOut); err != nil {
		return fail(c, "integration.status: %v", err)
	}
	if !statOut.Found {
		return fail(c, "integration.status(%q) found=false after a verdict was recorded", branch)
	}
	if statOut.State != "changes_requested" {
		return fail(c, "integration.status state %q, want \"changes_requested\"", statOut.State)
	}
	if statOut.Feedback != feedback {
		return fail(c, "integration.status feedback %q != the integrator's verdict feedback %q (the verdict did not propagate intact)", statOut.Feedback, feedback)
	}

	payloadDetail, err := c.checkEventPayloadNoLeak(s)
	if err != nil {
		return fail(c, "%v", err)
	}
	audienceDetail, err := c.checkEventBranchAudienceIsolation(s)
	if err != nil {
		return fail(c, "%v", err)
	}

	return pass(c, "mesh absent (%v all method-not-found); closed convergence loop across two sessions (builder %s -> integrator %s): request->pending(metadata-only %v)->claim->verdict(reject)->status=changes_requested+feedback; the only cross-session datum is a claimed-record review verdict; %s; %s",
		meshMethods, short(builderID), short(integratorID), pendingMetadataFields, payloadDetail, audienceDetail)
}

func (c integrationConvergenceScoped) Control(s *Scratch) error {
	cl, err := s.Dial()
	if err != nil {
		return fmt.Errorf("control dial: %w", err)
	}
	defer cl.Close()

	// (1) POSITIVE: a real integration method must RESOLVE — otherwise the registry
	// probe cannot confirm presence and "the integration loop is reachable" is hollow.
	var req struct {
		State string `json:"state"`
	}
	if err := cl.Call("integration.request", map[string]any{"branch": "nm/control-probe", "title": "control"}, &req); err != nil {
		return fmt.Errorf("CONTROL VACUOUS: a known-registered method (integration.request) failed (%v) — the method-probe cannot confirm a method is present, so 'mesh method absent' is meaningless", err)
	}
	if req.State != "requested" {
		return fmt.Errorf("CONTROL: integration.request resolved but did not land in \"requested\" (got %q) — the positive baseline is unreliable", req.State)
	}

	// (2) NEGATIVE: a mesh method (session.send) must be method-not-found — the
	// probe's absent-case baseline. If it were served, the mesh-absent verdict is invalid.
	var absent map[string]any
	merr := cl.Call("session.send", map[string]any{}, &absent)
	if merr == nil {
		return fmt.Errorf("CONTROL: a mesh affordance (session.send) was unexpectedly SERVED — the detector's absent-case baseline is invalid")
	}
	if !isMethodNotFound(merr) {
		return fmt.Errorf("CONTROL VACUOUS: a fabricated mesh method did not return method-not-found (%v) — the detector cannot tell absent from present", merr)
	}

	// (3) PRECISION: a REGISTERED integration method's PARAM error (invalid decision
	// -> CodeInvalidParams) must NOT be misclassified as method-not-found — otherwise
	// the over-broad match would let a partially-wired mesh method pass as "absent".
	var bad map[string]any
	perr := cl.Call("integration.verdict", map[string]any{"id": "nm/control-probe", "decision": "bogus"}, &bad)
	if perr == nil {
		return fmt.Errorf("CONTROL: integration.verdict with a bogus decision should error (invalid params) but succeeded — cannot exercise the misclassification guard")
	}
	if isMethodNotFound(perr) {
		return fmt.Errorf("CONTROL VACUOUS: a REGISTERED method's param error (%v) was misclassified as METHOD-not-found — the over-broad 'not found' match would hide a partially-wired mesh method", perr)
	}
	return nil
}

func (c integrationConvergenceScoped) checkEventPayloadNoLeak(s *Scratch) (string, error) {
	builder, err := s.Dial()
	if err != nil {
		return "", fmt.Errorf("event payload builder dial: %w", err)
	}
	defer builder.Close()
	integrator, err := s.Dial()
	if err != nil {
		return "", fmt.Errorf("event payload integrator dial: %w", err)
	}
	defer integrator.Close()
	eventConsumer, err := s.Dial()
	if err != nil {
		return "", fmt.Errorf("event payload consumer dial: %w", err)
	}
	defer eventConsumer.Close()

	const branch = "nm/event-payload-control"
	const marker = "EVENT_PAYLOAD_MARKER_title_must_not_enter_events"
	var req struct {
		State string `json:"state"`
	}
	if err := builder.Call("integration.request", map[string]any{"branch": branch, "title": marker}, &req); err != nil {
		return "", fmt.Errorf("event payload integration.request: %w", err)
	}
	if req.State != "requested" {
		return "", fmt.Errorf("event payload request landed in state %q, want requested", req.State)
	}

	eventsReply, events, err := callEvents(eventConsumer, map[string]any{"max": 50})
	if err != nil {
		return "", fmt.Errorf("integration.events payload poll: %w", err)
	}
	if err := validateEventFields(events); err != nil {
		return "", err
	}
	if !hasEvent(events, "integration.requested", branch) {
		return "", fmt.Errorf("CONTROL VACUOUS: integration.events did not return the daemon-authored requested event for %q, so the no-payload assertion saw no target event (events=%v)", branch, events)
	}
	eventsBytes, err := json.Marshal(eventsReply)
	if err != nil {
		return "", fmt.Errorf("marshal integration.events reply: %w", err)
	}
	if strings.Contains(string(eventsBytes), marker) {
		return "", fmt.Errorf("EVENT PAYLOAD LEAK: marker title %q appeared in integration.events reply %s — events must carry only %v", marker, eventsBytes, eventMetadataFields)
	}

	var pend struct {
		Pending []map[string]json.RawMessage `json:"pending"`
	}
	if err := integrator.Call("integration.pending", map[string]any{}, &pend); err != nil {
		return "", fmt.Errorf("integration.pending payload control: %w", err)
	}
	pendingBytes, err := json.Marshal(pend)
	if err != nil {
		return "", fmt.Errorf("marshal integration.pending reply: %w", err)
	}
	if !strings.Contains(string(pendingBytes), marker) {
		return "", fmt.Errorf("CONTROL VACUOUS: marker title %q did not appear in integration.pending (%s), so the no-payload check cannot prove it would see a leaked marker", marker, pendingBytes)
	}
	return fmt.Sprintf("events are payload-free wake-ups (fields pinned to %v; title marker visible in pending but absent from events)", eventMetadataFields), nil
}

func (c integrationConvergenceScoped) checkEventBranchAudienceIsolation(s *Scratch) (string, error) {
	holder, err := s.Dial()
	if err != nil {
		return "", fmt.Errorf("event audience holder dial: %w", err)
	}
	defer holder.Close()
	reviewer, err := s.Dial()
	if err != nil {
		return "", fmt.Errorf("event audience reviewer dial: %w", err)
	}
	defer reviewer.Close()
	stranger, err := s.Dial()
	if err != nil {
		return "", fmt.Errorf("event audience stranger dial: %w", err)
	}
	defer stranger.Close()

	holderID, err := whoAmIOn(holder)
	if err != nil {
		return "", fmt.Errorf("event audience holder whoami: %w", err)
	}
	reviewerID, err := whoAmIOn(reviewer)
	if err != nil {
		return "", fmt.Errorf("event audience reviewer whoami: %w", err)
	}
	strangerID, err := whoAmIOn(stranger)
	if err != nil {
		return "", fmt.Errorf("event audience stranger whoami: %w", err)
	}
	if holderID == reviewerID || holderID == strangerID || reviewerID == strangerID {
		return "", fmt.Errorf("event audience identities are not distinct (holder=%q reviewer=%q stranger=%q)", holderID, reviewerID, strangerID)
	}

	branch := "nm/" + holderID
	var req struct {
		State string `json:"state"`
	}
	if err := holder.Call("integration.request", map[string]any{"branch": branch, "title": "branch audience isolation"}, &req); err != nil {
		return "", fmt.Errorf("event audience integration.request: %w", err)
	}
	if req.State != "requested" {
		return "", fmt.Errorf("event audience request landed in state %q, want requested", req.State)
	}

	var claim struct {
		OK bool `json:"ok"`
	}
	if err := reviewer.Call("integration.claim", map[string]any{"id": branch}, &claim); err != nil {
		return "", fmt.Errorf("event audience integration.claim: %w", err)
	}
	if !claim.OK {
		return "", fmt.Errorf("event audience integration.claim returned ok=false for %q", branch)
	}

	var verdict struct {
		OK    bool   `json:"ok"`
		State string `json:"state"`
	}
	if err := reviewer.Call("integration.verdict", map[string]any{"id": branch, "decision": "reject", "feedback": "audience isolation verdict"}, &verdict); err != nil {
		return "", fmt.Errorf("event audience integration.verdict: %w", err)
	}
	if !verdict.OK || verdict.State != "changes_requested" {
		return "", fmt.Errorf("event audience verdict -> ok=%v state=%q, want ok=true state=changes_requested", verdict.OK, verdict.State)
	}

	_, strangerEvents, err := callEvents(stranger, map[string]any{"branch": branch, "max": 50})
	if err != nil {
		return "", fmt.Errorf("integration.events stranger branch poll: %w", err)
	}
	if len(strangerEvents) != 0 {
		return "", fmt.Errorf("BRANCH AUDIENCE LEAK: stranger session %s received branch events for %q: %v", short(strangerID), branch, strangerEvents)
	}

	_, holderEvents, err := callEvents(holder, map[string]any{"branch": branch, "max": 50})
	if err != nil {
		return "", fmt.Errorf("integration.events holder branch poll: %w", err)
	}
	if err := validateEventFields(holderEvents); err != nil {
		return "", err
	}
	if !hasEvent(holderEvents, "integration.verdict", branch) {
		return "", fmt.Errorf("CONTROL VACUOUS: holder session %s did not receive the verdict event for %q (events=%v), so the branch-audience isolation assertion would not flip red if authorization were dropped", short(holderID), branch, holderEvents)
	}
	return fmt.Sprintf("branch-audience events isolated (stranger %s saw 0 for %s; holder %s saw verdict)", short(strangerID), branch, short(holderID)), nil
}

func callEvents(c *rpcclient.Client, params map[string]any) (map[string]json.RawMessage, []map[string]json.RawMessage, error) {
	var raw map[string]json.RawMessage
	if err := c.Call("integration.events", params, &raw); err != nil {
		return nil, nil, err
	}
	var events []map[string]json.RawMessage
	if err := json.Unmarshal(raw["events"], &events); err != nil {
		return raw, nil, err
	}
	return raw, events, nil
}

func validateEventFields(events []map[string]json.RawMessage) error {
	for i, ev := range events {
		if extra := unexpectedFields(ev, eventMetadataFields); len(extra) > 0 {
			return fmt.Errorf("integration.events entry %d exposed NON-wake-up field(s) %v — events must carry only %v", i, extra, eventMetadataFields)
		}
		if missing := missingFields(ev, eventMetadataFields); len(missing) > 0 {
			return fmt.Errorf("integration.events entry %d missing required wake-up field(s) %v (entry=%v)", i, missing, sortedKeys(ev))
		}
	}
	return nil
}

func hasEvent(events []map[string]json.RawMessage, kind, branch string) bool {
	for _, ev := range events {
		if rawString(ev, "kind") == kind && rawString(ev, "branch") == branch {
			return true
		}
	}
	return false
}

func rawString(entry map[string]json.RawMessage, key string) string {
	var out string
	_ = json.Unmarshal(entry[key], &out)
	return out
}

// findPending returns the pending entry whose "branch" field equals branch.
func findPending(entries []map[string]json.RawMessage, branch string) (map[string]json.RawMessage, bool) {
	for _, e := range entries {
		raw, ok := e["branch"]
		if !ok {
			continue
		}
		var b string
		if err := json.Unmarshal(raw, &b); err != nil {
			continue
		}
		if b == branch {
			return e, true
		}
	}
	return nil, false
}

// unexpectedFields returns the keys present in entry that are NOT in allowed — any
// field beyond the sanctioned review metadata (a channel leak).
func unexpectedFields(entry map[string]json.RawMessage, allowed []string) []string {
	ok := map[string]bool{}
	for _, a := range allowed {
		ok[a] = true
	}
	var extra []string
	for k := range entry {
		if !ok[k] {
			extra = append(extra, k)
		}
	}
	sort.Strings(extra)
	return extra
}

// missingFields returns the allowed keys absent from entry.
func missingFields(entry map[string]json.RawMessage, allowed []string) []string {
	var missing []string
	for _, a := range allowed {
		if _, ok := entry[a]; !ok {
			missing = append(missing, a)
		}
	}
	sort.Strings(missing)
	return missing
}

// sortedKeys returns entry's keys sorted (for a deterministic diagnostic).
func sortedKeys(entry map[string]json.RawMessage) []string {
	keys := make([]string, 0, len(entry))
	for k := range entry {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

// short abbreviates a session id for a compact detail line.
func short(id string) string {
	if len(id) <= 10 {
		return id
	}
	return id[:10]
}
