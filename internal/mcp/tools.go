package mcp

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"sort"
	"strings"

	"github.com/madhavhaldia/mad-substrate/internal/conductor"
	"github.com/madhavhaldia/mad-substrate/internal/coopclient"
	"github.com/madhavhaldia/mad-substrate/internal/manifest"
)

// integratorGateEnv is the OPT-IN gate-on-approve knob (R12): a shell command run
// in the trunk worktree before the merge. Empty/unset ⇒ merge-only (Gate "").
const integratorGateEnv = "MAD_INTEGRATOR_GATE"

// defaultProtocolVersion is the MCP protocol version we advertise when the
// client does not request one. We otherwise echo the client's requested version
// (MCP version negotiation: the server reflects what the client asked for).
const defaultProtocolVersion = "2024-11-05"

// standingGuidance is the initialize "instructions" string: the model-facing
// explanation of the governed boundary and when to reach for these tools. It is
// the single most important piece of agent steering this server emits.
const standingGuidance = "You are running inside a mad-substrate governed boundary: an isolated git worktree with its own ports and state. Edits to your working tree are private and safe. Before editing a SHARED/convergent resource (one that must merge to the trunk), coordinate with these tools: mad_classify tells you whether a path needs coordination; mad_claim takes it so other agents see it as held. Forkable paths need no claim — edit freely. mad_status and mad_locks show current contention. You do NOT merge your own branch: just commit your work on your boundary branch — convergence to the trunk is handled outside the session (by `mad-substrate integrate` / the lead). The substrate guarantees safety regardless; these tools just help you avoid wasted work from conflicting edits."

// failSoftNote is the advisory returned (as isError) when the daemon is
// unreachable even after a re-Dial. Inv 13: a governed session must be no more
// fragile than a bare one — so we tell the agent it is SAFE to proceed.
const failSoftNote = "mad-substrate daemon unreachable — proceeding is safe (your worktree is isolated); coordination is just unavailable right now."

// toolDescriptor is one entry of tools/list. InputSchema is a raw JSON object so
// each tool can pin its exact schema (additionalProperties:false etc.) without
// fighting Go's map ordering.
type toolDescriptor struct {
	Name        string          `json:"name"`
	Description string          `json:"description"`
	InputSchema json.RawMessage `json:"inputSchema"`
}

// emptySchema is the inputSchema for the no-argument tools (locks, status).
var emptySchema = json.RawMessage(`{"type":"object","properties":{},"additionalProperties":false}`)

// pathSchema is the inputSchema for the path-argument tools.
var pathSchema = json.RawMessage(`{"type":"object","properties":{"path":{"type":"string","description":"repo-relative path"}},"required":["path"],"additionalProperties":false}`)

// titleSchema is the inputSchema for mad_request_integration (title is optional).
var titleSchema = json.RawMessage(`{"type":"object","properties":{"title":{"type":"string","description":"optional human-readable title for the integration request"}},"additionalProperties":false}`)

// idSchema is the inputSchema for the integrator tools that take an integration id.
var idSchema = json.RawMessage(`{"type":"object","properties":{"id":{"type":"string","description":"integration request id (equal to the builder's branch name)"}},"required":["id"],"additionalProperties":false}`)

// rejectSchema is the inputSchema for mad_integration_reject (id + feedback required).
var rejectSchema = json.RawMessage(`{"type":"object","properties":{"id":{"type":"string","description":"integration request id"},"feedback":{"type":"string","description":"why it was rejected / what the builder should change"}},"required":["id","feedback"],"additionalProperties":false}`)

// builderTools are the cooperative tools plus the builder-side integration
// plane: a builder requests integration of its OWN branch and polls status.
var builderTools = []toolDescriptor{
	{
		Name:        "mad_locks",
		Description: "List every lease (lock) currently held across all agents governed by the mad-substrate daemon — who holds which shared/convergent resource. Use it to see contention before editing.",
		InputSchema: emptySchema,
	},
	{
		Name:        "mad_classify",
		Description: "Classify a repo-relative path: forkable (isolated copy, edit freely), convergent (shared — must merge to trunk), or singular (a real external side effect). Tells you whether a path needs coordination.",
		InputSchema: pathSchema,
	},
	{
		Name:        "mad_claim",
		Description: "Claim a shared (convergent/singular) resource for exclusive work, so other agents see it as taken. Forkable paths need no claim. Returns granted, or who currently holds it. Release with mad_release.",
		InputSchema: pathSchema,
	},
	{
		Name:        "mad_release",
		Description: "Release a resource you claimed with mad_claim.",
		InputSchema: pathSchema,
	},
	{
		Name:        "mad_status",
		Description: "A situational-awareness summary: the resources you currently hold, all lease holders, and in-flight integrations.",
		InputSchema: emptySchema,
	},
	{
		Name:        "mad_request_integration",
		Description: "Request integration of YOUR current branch into the trunk. Call this once your work is committed; an integrator agent will review and either merge it or send feedback. Returns the request id and state. Poll progress with mad_integration_status.",
		InputSchema: titleSchema,
	},
	{
		Name:        "mad_integration_status",
		Description: "Check the state of YOUR branch's integration request (e.g. pending, claimed, merged, changes_requested) and read any reviewer feedback so you can iterate.",
		InputSchema: emptySchema,
	},
}

// integratorTools are the trunk-side reviewer tools plus read-only awareness
// (locks/status). The integrator lists pending requests, claims one, then
// approves (the gated merge) or rejects with feedback.
var integratorTools = []toolDescriptor{
	{
		Name:        "mad_integration_pending",
		Description: "List every integration request awaiting a verdict: id, branch, title, and state.",
		InputSchema: emptySchema,
	},
	{
		Name:        "mad_integration_claim",
		Description: "Claim a pending integration request for review (so other integrators don't double-handle it). Returns its branch and title.",
		InputSchema: idSchema,
	},
	{
		Name:        "mad_integration_approve",
		Description: "Approve a claimed request: run the deterministic gated merge of its branch into your current (trunk) branch, then record the verdict. On a merge conflict or skip it reports the reason and records NOTHING, so you can resolve manually or reject.",
		InputSchema: idSchema,
	},
	{
		Name:        "mad_integration_reject",
		Description: "Reject a request with required feedback explaining what the builder must change. The builder sees this via mad_integration_status.",
		InputSchema: rejectSchema,
	},
	{
		Name:        "mad_locks",
		Description: "List every lease (lock) currently held across all agents governed by the mad-substrate daemon — situational awareness while reviewing.",
		InputSchema: emptySchema,
	},
	{
		Name:        "mad_status",
		Description: "A situational-awareness summary: all lease holders and in-flight integrations.",
		InputSchema: emptySchema,
	},
}

// toolDescriptors returns the tool set for the given role, in a stable order so
// tools/list output (and its tests) never flaps.
func toolDescriptors(role string) []toolDescriptor {
	if role == roleIntegrator {
		return integratorTools
	}
	return builderTools
}

// toolsListResult is the tools/list result envelope.
type toolsListResult struct {
	Tools []toolDescriptor `json:"tools"`
}

func (s *server) toolsList() toolsListResult {
	return toolsListResult{Tools: toolDescriptors(s.role)}
}

// initializeResult is the initialize result envelope.
type initializeResult struct {
	ProtocolVersion string         `json:"protocolVersion"`
	Capabilities    map[string]any `json:"capabilities"`
	ServerInfo      serverInfo     `json:"serverInfo"`
	Instructions    string         `json:"instructions"`
}

type serverInfo struct {
	Name    string `json:"name"`
	Version string `json:"version"`
}

// handleInitialize answers the MCP handshake. It echoes the client's requested
// protocolVersion when present (version negotiation) and advertises the tools
// capability plus the standing guidance.
func (s *server) handleInitialize(req *rpcRequest) rpcResponse {
	pv := defaultProtocolVersion
	var p struct {
		ProtocolVersion string `json:"protocolVersion"`
	}
	if len(req.Params) > 0 {
		_ = json.Unmarshal(req.Params, &p) // fail-soft: malformed params -> default version
	}
	if strings.TrimSpace(p.ProtocolVersion) != "" {
		pv = p.ProtocolVersion
	}
	return resultResponse(req.ID, initializeResult{
		ProtocolVersion: pv,
		Capabilities:    map[string]any{"tools": map[string]any{}},
		ServerInfo:      serverInfo{Name: "mad-substrate", Version: s.version},
		Instructions:    standingGuidance,
	})
}

// toolContent is one MCP content element; we only ever emit a single text block.
type toolContent struct {
	Type string `json:"type"`
	Text string `json:"text"`
}

// toolResult is the tools/call result shape. isError true marks a tool-level
// failure (NOT a JSON-RPC error) — the agent still gets readable text.
type toolResult struct {
	Content []toolContent `json:"content"`
	IsError bool          `json:"isError"`
}

func textResult(text string, isError bool) toolResult {
	return toolResult{Content: []toolContent{{Type: "text", Text: text}}, IsError: isError}
}

// toolsCallParams is the params of a tools/call request.
type toolsCallParams struct {
	Name      string `json:"name"`
	Arguments struct {
		Path     string `json:"path"`
		Title    string `json:"title"`
		ID       string `json:"id"`
		Feedback string `json:"feedback"`
	} `json:"arguments"`
}

// handleToolsCall dispatches a tools/call. The result is ALWAYS an MCP content
// shape (even on failure: isError true) — never a JSON-RPC error — so the agent
// always receives readable text. The whole dispatch is panic-guarded so a
// misbehaving handler can never crash the session (Inv 13).
func (s *server) handleToolsCall(req *rpcRequest) (resp rpcResponse) {
	defer func() {
		if r := recover(); r != nil {
			// Never panic out of a tool: degrade to a fail-soft note.
			s.logf("mcp: tool panic recovered: %v", r)
			resp = resultResponse(req.ID, textResult(failSoftNote, true))
		}
	}()

	var p toolsCallParams
	if len(req.Params) > 0 {
		if err := json.Unmarshal(req.Params, &p); err != nil {
			return resultResponse(req.ID, textResult("invalid tool arguments", true))
		}
	}

	result := s.callTool(p)
	return resultResponse(req.ID, result)
}

// callTool routes to the named tool handler. The dispatch is role-gated: a tool
// not in the active role's set is reported as unknown (so an integrator cannot
// call a builder-coordination tool and vice-versa). An unknown tool name is a
// tool-level error (isError), not a JSON-RPC error.
func (s *server) callTool(p toolsCallParams) toolResult {
	if !s.toolAllowed(p.Name) {
		return textResult(fmt.Sprintf("unknown tool %q", p.Name), true)
	}
	switch p.Name {
	case "mad_locks":
		return s.toolLocks()
	case "mad_classify":
		return s.toolClassify(p.Arguments.Path)
	case "mad_claim":
		return s.toolClaim(p.Arguments.Path)
	case "mad_release":
		return s.toolRelease(p.Arguments.Path)
	case "mad_status":
		return s.toolStatus()
	case "mad_request_integration":
		return s.toolRequestIntegration(p.Arguments.Title)
	case "mad_integration_status":
		return s.toolIntegrationStatus()
	case "mad_integration_pending":
		return s.toolIntegrationPending()
	case "mad_integration_claim":
		return s.toolIntegrationClaim(p.Arguments.ID)
	case "mad_integration_approve":
		return s.toolIntegrationApprove(p.Arguments.ID)
	case "mad_integration_reject":
		return s.toolIntegrationReject(p.Arguments.ID, p.Arguments.Feedback)
	default:
		return textResult(fmt.Sprintf("unknown tool %q", p.Name), true)
	}
}

// toolAllowed reports whether name is exposed in the active role's tool set.
func (s *server) toolAllowed(name string) bool {
	for _, d := range toolDescriptors(s.role) {
		if d.Name == name {
			return true
		}
	}
	return false
}

// withBackend runs fn against a live backend, transparently re-Dialing ONCE on a
// transport error (Inv 13). If no daemon can be reached, it returns the
// fail-soft note as an isError result and fn is never (or no longer) retried.
// fn returns (result, err); a transport err triggers the single re-Dial+retry.
func (s *server) withBackend(fn func(be backend) (toolResult, error)) toolResult {
	be := s.backendOrRedial()
	if be == nil {
		return textResult(failSoftNote, true)
	}
	res, err := fn(be)
	if err == nil {
		return res
	}
	if !coopclient.IsTransport(err) {
		// A real daemon VERDICT (authz/conflict/etc.) is not a transport blip;
		// surface it as a tool error rather than masking it with the note.
		return textResult(fmt.Sprintf("mad-substrate: %s", err.Error()), true)
	}
	// One re-Dial on a transport failure, then retry exactly once.
	be = s.redial()
	if be == nil {
		return textResult(failSoftNote, true)
	}
	res, err = fn(be)
	if err != nil {
		if coopclient.IsTransport(err) {
			return textResult(failSoftNote, true)
		}
		return textResult(fmt.Sprintf("mad-substrate: %s", err.Error()), true)
	}
	return res
}

// toolLocks lists every held lease across all governed agents.
func (s *server) toolLocks() toolResult {
	return s.withBackend(func(be backend) (toolResult, error) {
		holders, err := be.ListHolders()
		if err != nil {
			return toolResult{}, err
		}
		if len(holders) == 0 {
			return textResult("No leases are currently held.", false), nil
		}
		me := be.Holder()
		var b strings.Builder
		for i, h := range holders {
			if i > 0 {
				b.WriteByte('\n')
			}
			you := ""
			if h.Holder == me && me != "" {
				you = " (you)"
			}
			fmt.Fprintf(&b, "• %s%s holds %s (fence %d)", h.Holder, you, describeLeaseKey(h.Key), h.Fence)
		}
		return textResult(b.String(), false), nil
	})
}

// describeLeaseKey renders a held lease key for humans. The daemon emits keys
// base64-encoded (lease.list), so we decode then classify: a per-path convergent
// key shows the file held, a per-name external convergent key shows the resource
// held, the trunk-merge key shows "the trunk", and anything else (incl. an
// undecodable key) falls back to the raw opaque key. Fail-soft:
// any decode/match miss degrades to the opaque rendering, never an error.
func describeLeaseKey(key string) string {
	raw, err := base64.StdEncoding.DecodeString(key)
	if err != nil {
		return key // not base64 (or already plain) — opaque fallback
	}
	if path, ok := manifest.PathFromConvergentKey(raw); ok {
		return fmt.Sprintf("convergent path %q", path)
	}
	if name, ok := manifest.ExternalFromConvergentKey(raw); ok {
		return fmt.Sprintf("convergent external %q", name)
	}
	if string(raw) == string(manifest.TrunkKey) {
		return "the trunk"
	}
	return key
}

// toolClassify reports whether a path is forkable, convergent, or singular and
// whether it needs coordination.
func (s *server) toolClassify(path string) toolResult {
	path = strings.TrimSpace(path)
	if path == "" {
		return textResult("path is required", true)
	}
	return s.withBackend(func(be backend) (toolResult, error) {
		kind, leaseKey, err := be.Route("path", path)
		if err != nil {
			return toolResult{}, err
		}
		coord := "no coordination needed"
		if leaseKey != "" {
			coord = "needs coordination (claim before editing)"
		}
		return textResult(fmt.Sprintf("%q -> %s — %s.", path, kind, coord), false), nil
	})
}

// toolClaim takes a shared resource for exclusive work, tracking it for
// auto-renew. A forkable path needs no claim; a contended one reports the holder.
func (s *server) toolClaim(path string) toolResult {
	path = strings.TrimSpace(path)
	if path == "" {
		return textResult("path is required", true)
	}
	return s.withBackend(func(be backend) (toolResult, error) {
		// Route("path", path) now yields a PER-PATH convergent key (the daemon's
		// domain-aware routing), so claiming distinct convergent files acquires
		// distinct leases — two agents no longer falsely serialize on one lock.
		kind, leaseKey, err := be.Route("path", path)
		if err != nil {
			return toolResult{}, err
		}
		if leaseKey == "" {
			return textResult(fmt.Sprintf("%q is forkable — no claim needed; edit freely in your isolated worktree.", path), false), nil
		}
		granted, holder, _, err := be.Acquire(leaseKey, s.cfg.LeaseTTL)
		if err != nil {
			return toolResult{}, err
		}
		if !granted {
			return textResult(fmt.Sprintf("Could not claim %q — currently held by another session (%s).", path, holder), true), nil
		}
		// Track for auto-renew + shutdown release. The key, not the path, is
		// what the daemon arbitrates on (Inv 9), but we key by path so a later
		// release-by-path finds it.
		s.track(path, leaseKey)
		return textResult(fmt.Sprintf("Claimed %q (%s). Release it with mad_release when done.", path, kind), false), nil
	})
}

// toolRelease drops a resource claimed earlier, stopping its auto-renew.
func (s *server) toolRelease(path string) toolResult {
	path = strings.TrimSpace(path)
	if path == "" {
		return textResult("path is required", true)
	}
	return s.withBackend(func(be backend) (toolResult, error) {
		kind, leaseKey, err := be.Route("path", path)
		_ = kind
		if err != nil {
			return toolResult{}, err
		}
		if leaseKey == "" {
			return textResult(fmt.Sprintf("%q is forkable — nothing to release.", path), false), nil
		}
		// Stop auto-renew BEFORE releasing so the ticker can't re-touch a key we
		// are dropping.
		s.untrack(path)
		ok, err := be.Release(leaseKey)
		if err != nil {
			return toolResult{}, err
		}
		if ok {
			return textResult(fmt.Sprintf("Released %q.", path), false), nil
		}
		return textResult(fmt.Sprintf("%q was not held by you; nothing released.", path), false), nil
	})
}

// toolStatus is the situational-awareness summary: identity, what I hold, all
// holders, and in-flight integrations.
func (s *server) toolStatus() toolResult {
	return s.withBackend(func(be backend) (toolResult, error) {
		holders, err := be.ListHolders()
		if err != nil {
			return toolResult{}, err
		}
		ints, err := be.Integrations()
		if err != nil {
			return toolResult{}, err
		}
		me := be.Holder()

		// Mine = keys held by my own daemon-minted identity (Inv 4).
		var mine []string
		for _, h := range holders {
			if h.Holder == me && me != "" {
				mine = append(mine, h.Key)
			}
		}
		sort.Strings(mine)
		hold := "nothing"
		if len(mine) > 0 {
			hold = strings.Join(mine, ", ")
		}

		// In-flight = integrations still moving through the gate.
		var inFlight []string
		for _, ig := range ints {
			if ig.State == "received" || ig.State == "validating" {
				inFlight = append(inFlight, fmt.Sprintf("%s (%s: %s)", ig.ID, ig.Branch, ig.State))
			}
		}
		flight := "none"
		if len(inFlight) > 0 {
			flight = strings.Join(inFlight, ", ")
		}

		identity := me
		if s.cfg.Session != "" {
			identity = fmt.Sprintf("%s (session %s)", me, s.cfg.Session)
		}

		text := fmt.Sprintf(
			"Your identity: %s\nYou hold: %s\nAll lease holders: %d\nIn-flight integrations: %s",
			identity, hold, len(holders), flight,
		)
		return textResult(text, false), nil
	})
}

// ----- builder integration tools -----

// toolRequestIntegration (builder) resolves the builder's OWN branch from the
// working dir and registers it for integration. A detached HEAD or a non-nm/*
// branch is a tool error (you can only request integration of a boundary branch).
func (s *server) toolRequestIntegration(title string) toolResult {
	branch, err := s.ownBranch()
	if err != nil {
		return textResult(err.Error(), true)
	}
	title = strings.TrimSpace(title)
	return s.withBackend(func(be backend) (toolResult, error) {
		id, state, err := be.RequestIntegration(branch, title)
		if err != nil {
			return toolResult{}, err
		}
		return textResult(fmt.Sprintf("Requested integration of %s (id %s) — state: %s. Poll with mad_integration_status.", branch, id, state), false), nil
	})
}

// toolIntegrationStatus (builder) reports the state + feedback of the builder's
// own branch's integration request so the agent can iterate on changes_requested.
func (s *server) toolIntegrationStatus() toolResult {
	branch, err := s.ownBranch()
	if err != nil {
		return textResult(err.Error(), true)
	}
	return s.withBackend(func(be backend) (toolResult, error) {
		found, _, state, feedback, merge, err := be.IntegrationStatus(branch)
		if err != nil {
			return toolResult{}, err
		}
		if !found {
			return textResult(fmt.Sprintf("No integration request found for %s. Use mad_request_integration once your work is committed.", branch), false), nil
		}
		line := fmt.Sprintf("%s: %s", branch, state)
		if strings.TrimSpace(feedback) != "" {
			line += fmt.Sprintf(" — feedback: %s", feedback)
		}
		if strings.TrimSpace(merge) != "" {
			line += fmt.Sprintf(" (merged as %s)", merge)
		}
		return textResult(line, false), nil
	})
}

// ----- integrator integration tools -----

// toolIntegrationPending (integrator) lists requests awaiting a verdict.
func (s *server) toolIntegrationPending() toolResult {
	return s.withBackend(func(be backend) (toolResult, error) {
		pending, err := be.IntegrationPending()
		if err != nil {
			return toolResult{}, err
		}
		if len(pending) == 0 {
			return textResult("No integration requests are pending.", false), nil
		}
		var b strings.Builder
		for i, p := range pending {
			if i > 0 {
				b.WriteByte('\n')
			}
			title := p.Title
			if strings.TrimSpace(title) == "" {
				title = "(no title)"
			}
			fmt.Fprintf(&b, "• %s — %s [%s] %q", p.ID, p.Branch, p.State, title)
		}
		return textResult(b.String(), false), nil
	})
}

// toolIntegrationClaim (integrator) claims a pending request for review.
func (s *server) toolIntegrationClaim(id string) toolResult {
	id = strings.TrimSpace(id)
	if id == "" {
		return textResult("id is required", true)
	}
	return s.withBackend(func(be backend) (toolResult, error) {
		ok, branch, title, err := be.IntegrationClaim(id)
		if err != nil {
			return toolResult{}, err
		}
		if !ok {
			return textResult(fmt.Sprintf("Could not claim %s — already claimed or not pending.", id), true), nil
		}
		if strings.TrimSpace(title) == "" {
			title = "(no title)"
		}
		return textResult(fmt.Sprintf("Claimed %s — branch %s %q. Review then mad_integration_approve or mad_integration_reject.", id, branch, title), false), nil
	})
}

// toolIntegrationApprove (integrator) performs THE GATED MERGE. The integrator
// runs in the trunk (feature) worktree, so the branch is merged into the
// worktree's current HEAD via conductor.Converge over a FRESH daemon connection.
// Only a clean StatusConverged records the approve verdict (with the merge OID);
// any other outcome reports the reason and records nothing, leaving the
// integrator free to resolve manually or reject.
func (s *server) toolIntegrationApprove(id string) toolResult {
	branch := strings.TrimSpace(id)
	if branch == "" {
		return textResult("id is required", true)
	}
	if !safeRef(branch) {
		return textResult(fmt.Sprintf("refusing to merge unsafe branch ref %q", branch), true)
	}

	getwd := s.getwd
	if getwd == nil {
		getwd = os.Getwd
	}
	repoDir, err := getwd()
	if err != nil {
		return textResult("cannot resolve working directory: "+err.Error(), true)
	}
	target, err := gitCurrentBranch(repoDir)
	if err != nil {
		return textResult("cannot resolve target branch: "+err.Error(), true)
	}
	if target == "HEAD" {
		return textResult("integrator worktree is in detached HEAD; checkout the trunk/feature branch first", true)
	}

	dialRPC := s.dialRPC
	if dialRPC == nil {
		dialRPC = defaultDialRPC
	}
	conn, err := dialRPC()
	if err != nil || conn == nil {
		// Fail-soft: no daemon to serialize the merge — do nothing rather than
		// merge without the trunk lease.
		return textResult(failSoftNote, true)
	}
	defer conn.Close()

	converge := s.converge
	if converge == nil {
		converge = conductor.Converge
	}
	// OPT-IN gate-on-approve (R12): when MAD_INTEGRATOR_GATE is set, the
	// merge runs that shell command in the trunk worktree (BoundaryDir == repoDir)
	// BEFORE merging; a non-zero exit ⇒ conductor.StatusGateFailed ⇒ the
	// not-converged branch below reports the failure and records NO verdict.
	// Empty/unset ⇒ Gate "" ⇒ EXACTLY today's merge-only behavior.
	gate := strings.TrimSpace(os.Getenv(integratorGateEnv))
	res := converge(conn, conductor.Spec{
		Branch:       branch,
		TargetBranch: target,
		RepoDir:      repoDir,
		BoundaryDir:  repoDir,
		Gate:         gate,
		Logf:         s.logf,
	})
	if res.Status != conductor.StatusConverged {
		reason := res.Reason
		if reason == "" {
			reason = string(res.Status)
		}
		return textResult(fmt.Sprintf("Did NOT approve %s: %s (%s). No verdict recorded — resolve manually or reject.", branch, res.Status, reason), true)
	}

	// Clean merge: record the approve verdict carrying the merge OID.
	return s.withBackend(func(be backend) (toolResult, error) {
		if _, _, err := be.IntegrationVerdict(id, "approve", "", res.Merge); err != nil {
			return toolResult{}, err
		}
		return textResult(fmt.Sprintf("merged %s into %s (%s)", branch, target, res.Merge), false), nil
	})
}

// toolIntegrationReject (integrator) records a reject verdict with required
// feedback the builder reads via mad_integration_status.
func (s *server) toolIntegrationReject(id, feedback string) toolResult {
	id = strings.TrimSpace(id)
	if id == "" {
		return textResult("id is required", true)
	}
	feedback = strings.TrimSpace(feedback)
	if feedback == "" {
		return textResult("feedback is required to reject (tell the builder what to change)", true)
	}
	return s.withBackend(func(be backend) (toolResult, error) {
		ok, state, err := be.IntegrationVerdict(id, "reject", feedback, "")
		if err != nil {
			return toolResult{}, err
		}
		if !ok {
			return textResult(fmt.Sprintf("Could not reject %s — not claimed by you or already decided.", id), true), nil
		}
		return textResult(fmt.Sprintf("Rejected %s (state: %s). The builder will see your feedback.", id, state), false), nil
	})
}

// ownBranch resolves the builder's current branch from the MCP server's working
// directory (which runs in the builder boundary). A detached HEAD or a non-nm/*
// branch is rejected with a helpful message.
func (s *server) ownBranch() (string, error) {
	getwd := s.getwd
	if getwd == nil {
		getwd = os.Getwd
	}
	dir, err := getwd()
	if err != nil {
		return "", fmt.Errorf("cannot resolve working directory: %v", err)
	}
	branch, err := gitCurrentBranch(dir)
	if err != nil {
		return "", fmt.Errorf("cannot resolve current branch (is this a git worktree?): %v", err)
	}
	if branch == "HEAD" {
		return "", fmt.Errorf("HEAD is detached — checkout your boundary branch (nm/...) before requesting integration")
	}
	if !strings.HasPrefix(branch, "nm/") {
		return "", fmt.Errorf("current branch %q is not a mad-substrate boundary branch (nm/...); only boundary branches can be integrated", branch)
	}
	return branch, nil
}

// gitCurrentBranch returns the abbreviated symbolic HEAD of dir ("HEAD" when
// detached).
func gitCurrentBranch(dir string) (string, error) {
	out, err := exec.Command("git", "-C", dir, "rev-parse", "--abbrev-ref", "HEAD").Output()
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(out)), nil
}

// safeRef reports whether a branch/ref name is safe to pass to git as a
// POSITIONAL argument: non-empty, not a flag, and only [A-Za-z0-9._/-]. Mirrors
// the conductor's guard so the approve flow can never arg-inject git.
func safeRef(s string) bool {
	if s == "" || strings.HasPrefix(s, "-") {
		return false
	}
	for _, r := range s {
		ok := (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') ||
			r == '.' || r == '_' || r == '/' || r == '-'
		if !ok {
			return false
		}
	}
	return true
}
