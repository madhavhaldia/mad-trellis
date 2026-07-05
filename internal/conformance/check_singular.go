package conformance

import (
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// check_singular.go proves the safety-property conjunct (c): no agent can affect a
// SINGULAR resource without an explicit grant (GROUNDING L140; docs/0003 §10a;
// owned by singular-gate / Inv 8). DENY is the structural ground state.
//
// BLACK BOX over the public gate CLI (gate resolve / gate request) + the daemon's
// classifier. It carries the negative AND its positive control:
//
//	NEGATIVE — an UNDECLARED external resource is singular-by-default and DENIED:
//	  resolve => deny; request => granted=false, real_reachable=false, and the
//	  produced env carries a NON-routable sentinel, never a real endpoint (the
//	  real endpoint is unreachable from the env-spec alone — proxy/grant bypass).
//
//	POSITIVE CONTROL (granted-mode-only) — a GRANTED resource (declared in the
//	  repo manifest with a mock grant) resolves to its granted mode and request
//	  reports granted=true; so the deny verdict is non-vacuous (the gate is not
//	  denying literally everything). A mock grant is real_reachable=false (no real
//	  side effect) — the gate hands a sentinel, never a live endpoint — so even the
//	  GRANTED path never leaks a reachable real endpoint for a non-supervised mode.
//
// This check needs a manifest with a grant, so Run writes mad-trellis.json into the
// scratch governed repo and RESTARTS the daemon (the classifier loads the
// manifest at boot). To keep the gate hermetic, it uses a dedicated Scratch
// helper that re-reads the manifest.

func init() { RegisterCheck(singularGated{}) }

type singularGated struct{}

func (singularGated) ID() string           { return "singular-default-deny" }
func (singularGated) OwnerProject() string { return "singular-gate" }
func (singularGated) Clause() string {
	return "safety (c): no singular effect without an explicit grant; default-deny ground state (Inv 8)"
}

// grantedResource / deniedResource are the two probe resources. grantedResource is
// declared singular+mock in the manifest written below; deniedResource is left
// undeclared (singular-by-default, no grant => deny).
const (
	grantedResource = "mock-saas"
	deniedResource  = "prod-database"
)

func (c singularGated) Run(s *Scratch) Result {
	// Write a manifest declaring grantedResource as singular with a mock grant, and
	// restart the daemon so the classifier loads it. deniedResource stays undeclared.
	if err := s.writeManifestAndRestart(singularManifest()); err != nil {
		return fail(c, "install manifest + restart daemon: %v", err)
	}

	// --- NEGATIVE: an undeclared resource is denied.
	resDeny := s.CLI("gate", "resolve", deniedResource)
	if !resDeny.OK() {
		return fail(c, "gate resolve %q failed: exit %d %s", deniedResource, resDeny.ExitCode, resDeny.Out())
	}
	if !strings.Contains(resDeny.Out(), "deny") {
		return fail(c, "an undeclared singular resource must resolve to deny; got: %s", strings.TrimSpace(resDeny.Out()))
	}
	// Use the structured singular.request so we can enumerate EVERY env value (not
	// grep CLI text). We do NOT trust real_reachable — we scan the env bytes.
	deny, err := s.GateRequest(deniedResource)
	if err != nil {
		return fail(c, "gate request %q: %v", deniedResource, err)
	}
	if deny.Granted {
		return fail(c, "an ungranted singular request must be granted=false; got granted=%v", deny.Granted)
	}
	if deny.RealReachable {
		return fail(c, "an ungranted singular request must be real_reachable=false; got real_reachable=%v", deny.RealReachable)
	}
	// #4: the denied env-spec must carry the NON-routable sentinel AND leak NO routable
	// endpoint in any value's raw OR base64/hex/url-decoded form (the prior line-78
	// guard only caught a plaintext "://<resource>" — an encoded leak slipped past).
	if reason := assertNonRoutableEnv(deny.Env, "mad-trellis-denied://"); reason != "" {
		return fail(c, "BREACH (denied env-spec, NOT trusting real_reachable): %s", reason)
	}

	// --- POSITIVE CONTROL: the granted (mock) resource resolves to its mode and is
	// granted (so the deny above is non-vacuous), yet real_reachable stays false
	// (mock = no real side effect; the gate never hands a live endpoint here).
	resGrant := s.CLI("gate", "resolve", grantedResource)
	if !resGrant.OK() {
		return fail(c, "gate resolve %q failed: exit %d %s", grantedResource, resGrant.ExitCode, resGrant.Out())
	}
	if !strings.Contains(resGrant.Out(), "mock") {
		return fail(c, "POSITIVE CONTROL DEAD: the mock-granted resource did not resolve to mock; got: %s", strings.TrimSpace(resGrant.Out()))
	}
	reqGrant := s.CLI("gate", "request", grantedResource)
	if !reqGrant.OK() {
		return fail(c, "gate request %q failed: exit %d %s", grantedResource, reqGrant.ExitCode, reqGrant.Out())
	}
	gout := reqGrant.Out()
	if !strings.Contains(gout, "granted=true") {
		return fail(c, "POSITIVE CONTROL DEAD: the mock-granted resource was not granted; got: %s", strings.TrimSpace(gout))
	}
	if !strings.Contains(gout, "real_reachable=false") {
		return fail(c, "a mock grant must be real_reachable=false (no real side effect); got: %s", strings.TrimSpace(gout))
	}

	return pass(c, "undeclared %q denied (granted=false, real_reachable=false, sentinel env); granted %q resolves mock+granted (real_reachable=false)",
		deniedResource, grantedResource)
}

func (c singularGated) Control(s *Scratch) error {
	// INJECT a leaked endpoint into the probe's OWN evaluation path (the leak
	// detector) and assert it flips RED — the negative this check's deny-env scan is
	// meant to catch. We feed assertNonRoutableEnv a SYNTHETIC denied env-spec that
	// smuggles an ENCODED routable endpoint and assert the scan FAILS. If it passed an
	// encoded leak, the Run's "denied env carries only a non-routable sentinel" verdict
	// would be vacuous. (No production gate.go change — the detector is fed directly.)
	leaked := map[string]string{
		"MAD_SINGULAR_PROD_DATABASE": "mad-trellis-denied://prod-database", // the honest sentinel
		"MAD_SMUGGLED":               hexEndpoint("postgres://prod.internal.example:5432/db"),
	}
	if reason := assertNonRoutableEnv(leaked, "mad-trellis-denied://"); reason == "" {
		return fmt.Errorf("CONTROL VACUOUS: the deny-env leak scan PASSED a synthetic env smuggling a hex-encoded routable endpoint — it cannot catch an encoded leak, so the deny verdict proves nothing")
	}
	// And a clean sentinel-only denied env must NOT be flagged (not flagging all).
	clean := map[string]string{"MAD_SINGULAR_PROD_DATABASE": "mad-trellis-denied://prod-database"}
	if reason := assertNonRoutableEnv(clean, "mad-trellis-denied://"); reason != "" {
		return fmt.Errorf("CONTROL VACUOUS: the deny-env leak scan FLAGGED a clean sentinel-only env (%s) — it flags everything", reason)
	}

	// Belt and suspenders: the gate must GRANT the explicitly-granted resource — if it
	// denied EVERYTHING, the deny verdict for the undeclared resource would be
	// meaningless. Confirm the granted resource is not denied at runtime.
	if err := s.writeManifestAndRestart(singularManifest()); err != nil {
		return fmt.Errorf("control install manifest: %w", err)
	}
	grant, err := s.GateRequest(grantedResource)
	if err != nil {
		return fmt.Errorf("control gate request: %v", err)
	}
	if !grant.Granted {
		return fmt.Errorf("CONTROL VACUOUS: the gate denies even an explicitly granted resource (deny verdict is meaningless): %v", grant)
	}
	return nil
}

// hexEndpoint hex-encodes an endpoint string (a helper for the control's synthetic
// encoded-leak env).
func hexEndpoint(s string) string { return hex.EncodeToString([]byte(s)) }

// singularManifest is the scratch repo manifest declaring grantedResource as a
// singular resource with a mock grant, leaving deniedResource undeclared (so it is
// singular-by-default and default-denied).
func singularManifest() string {
	return `{
  "version": 1,
  "forkable": { "resources": [] },
  "convergent": { "paths": ["migrations/**", "**/*.lock"], "resources": [] },
  "singular": {
    "paths": [],
    "resources": ["` + grantedResource + `"],
    "grants": [
      { "resource": "` + grantedResource + `", "mode": "mock" }
    ]
  }
}
`
}

// writeManifestAndRestart writes mad-trellis.json into the scratch governed repo and
// restarts the daemon so the classifier reloads the manifest at boot. It is a
// Scratch method declared here (next to its only consumer) to keep harness.go
// focused on lifecycle primitives.
func (s *Scratch) writeManifestAndRestart(manifest string) error {
	if err := os.WriteFile(filepath.Join(s.RepoDir, "mad-trellis.json"), []byte(manifest), 0o644); err != nil {
		return err
	}
	// Stop the running daemon, then start a fresh one that reads the new manifest.
	if s.daemon != nil && s.daemon.Process != nil {
		_ = s.daemon.Process.Kill()
		_, _ = s.daemon.Process.Wait()
		s.daemon = nil
	}
	return s.startDaemon()
}
