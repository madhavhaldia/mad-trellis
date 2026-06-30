package conformance

import (
	"fmt"
	"path/filepath"
	"strings"
)

// check_confinement.go is the T3 CONFINEMENT TIER: it proves the CONTAINER GRAIN
// (chafe C5/C13) structurally confines an UNCOOPERATIVE agent, UPGRADING
// escape-resistance from the worktree grain's grain-RELATIVE honest scope ("mad-substrate
// never HANDS OUT an escaping path") to STRUCTURAL confinement ("an agent that
// IGNORES the boundary and runs arbitrary shell still cannot escape"). It is the
// Inv-10 grain dial made real (GROUNDING; docs/0003 §10a; Inv 4 "sandboxed harder").
//
// WHY THE WORKTREE GRAIN COULD NOT ASSERT THIS. check_escape.go's worktree-FS probe
// scopes itself honestly: at the v1 grain a plain directory cannot confine a process
// that runs `cd /` and reads/writes elsewhere — so it asserts only DISJOINTNESS, NO
// ESCAPING PATH HANDED OUT, and the grain-independent convergent floor. These
// container-grain checks assert what that grain structurally COULD NOT: a host
// sentinel OUTSIDE the mount is unreadable inside, writes outside /work fail, the
// canonical trunk is unreachable, and the governance plane (the daemon socket) is
// unreachable.
//
// BLACK BOX + UNCOOPERATIVE. Each check provisions a container boundary over the
// PUBLIC substrate.provision RPC (the daemon on the container grain), then acts as an
// uncooperative agent by driving raw `container exec` INTO the boundary (the
// launcher's container-exec path) — no adapter, no cooperative protocol. The runtime
// must confine it STRUCTURALLY. Assertions are over OBSERVABLE in-container state
// (command exit + output), never a self-reported flag (chafe C24).
//
// CONTROLS ARE NON-VACUOUS, via OBSERVABLE STATE. Every negative carries a control
// that genuinely flips RED: the control provisions a deliberately NON-confined
// container that DOES mount the host root / the trunk repo / the socket dir, and
// asserts the SAME breach predicate the Run uses FIRES — proving "not reachable
// under confinement" is a real structural property, not a dead probe.
//
// SKIP-WITH-REASON. When the `container` runtime is unavailable (CI / runtime-less
// host) each check returns a SKIPPED Result naming the reason — the gate stays
// runnable and the worktree-grain probes are UNCHANGED and still run. Every container
// a check creates is torn down (substrate.teardown + a belt-and-suspenders rm -f), so
// a failing assertion never leaks a container.

func init() {
	RegisterCheck(containerFSConfinement{})
	RegisterCheck(containerTrunkConfinement{})
	RegisterCheck(containerNetworkConfinement{})
}

// confinementOwner is the owning project for the container-grain structural tier.
const confinementOwner = "isolation-substrate/container-grain"

// hostSentinelName is the basename of the secret written on the HOST outside any
// mount; its absence inside the container is the FS-confinement assertion.
const hostSentinelName = "HOST_SENTINEL"

// ----------------------------------------------------------------------------
// container-fs-confinement — an uncooperative in-container agent cannot read or
// write the host FS outside its /work mount.
// ----------------------------------------------------------------------------

type containerFSConfinement struct{}

func (containerFSConfinement) ID() string           { return "container-fs-confinement" }
func (containerFSConfinement) OwnerProject() string { return confinementOwner }
func (containerFSConfinement) Clause() string {
	return "0003 §10a / Inv 4 (container grain, CONFINED mode opt-in): an uncooperative agent INSIDE the container cannot read a host sentinel outside its /work mount nor write outside /work — read confinement is MOUNT-STRUCTURAL (holds in both modes); the 'write outside /work FAILS' clause requires the read-only rootfs, so this tier opts into the confined mode (MAD_CONTAINER_CONFINED=1). Full-FS confinement the worktree grain (C5/C13) could not provide"
}

func (c containerFSConfinement) Run(s *Scratch) Result {
	if ok, reason := ContainerRuntimeAvailable(); !ok {
		return skip(c, "container-fs-confinement: %s", reason)
	}
	// The 'write outside /work FAILS' assertion below depends on the --read-only rootfs,
	// which is now the OPT-IN confined mode (a cooperative-default writable rootfs would
	// let an in-container write to its OWN ephemeral rootfs succeed — host isolation is
	// mount-structural either way, but this clause is specifically about read-only). So
	// boot the daemon on the CONFINED container grain.
	if err := s.UseConfinedContainerGrain(); err != nil {
		return fail(c, "boot daemon on the confined container grain: %v", err)
	}

	// A host sentinel written OUTSIDE any mount: the scratch state dir is a real host
	// dir the container grain never mounts. Its unreadability inside is the assertion.
	secret := "top-secret-" + short12(s.Root)
	hostSentinel := filepath.Join(s.StateDir, hostSentinelName)
	if err := writeHostFile(hostSentinel, secret); err != nil {
		return fail(c, "write host sentinel: %v", err)
	}

	cb, err := s.ProvisionContainer()
	if err != nil {
		return fail(c, "provision container boundary: %v", err)
	}
	defer cb.Teardown()

	// Sanity: the bind-mount IS the agent's /work (the seed file is visible) — so a
	// later "host path absent" is genuine confinement, not a broken container.
	if out, err := cb.ExecInContainer("ls", "/work"); err != nil {
		return fail(c, "the container's /work must be execable (a benign command failed: %v: %s)", err, out)
	}

	// (1) READ confinement: the host sentinel path is NOT readable inside.
	if hostPathReadableInside(cb, hostSentinel) {
		return fail(c, "BREACH: the host sentinel %q (outside the /work mount) was READABLE inside the container — the host FS is not confined", hostSentinel)
	}

	// (2) WRITE confinement: a write to a host-rooted path outside /work FAILS
	// (read-only rootfs; the host FS is not even present to write to).
	if pathWritableInside(cb, "/escape-attempt.txt") {
		return fail(c, "BREACH: an uncooperative agent WROTE outside /work (/escape-attempt.txt) — the rootfs is not confined")
	}

	// (3) the agent's OWN /work IS writable (so the negatives above are not just a
	// wholly broken FS) — observable: write then read back.
	if !pathWritableInside(cb, "/work/agent-work.txt") {
		return fail(c, "the agent's own /work mount must be writable (it was not) — the boundary is broken, making the confinement negatives vacuous")
	}

	return pass(c, "container grain STRUCTURAL: host sentinel %q outside /work is UNREADABLE inside; a write outside /work FAILS (read-only rootfs); the agent's own /work is writable. Upgrades C5/C13 from grain-relative to structural",
		filepath.Base(hostSentinel))
}

func (c containerFSConfinement) Control(s *Scratch) error {
	if ok, _ := ContainerRuntimeAvailable(); !ok {
		return nil // nothing to assert non-vacuously on a runtime-less host
	}
	// Match the Run's CONFINED grain so the confined boundary asserted below is the same
	// mode the Run relies on.
	if err := s.UseConfinedContainerGrain(); err != nil {
		return fmt.Errorf("control boot confined container grain: %w", err)
	}
	secret := "top-secret-ctl-" + short12(s.Root)
	hostSentinel := filepath.Join(s.StateDir, hostSentinelName)
	if err := writeHostFile(hostSentinel, secret); err != nil {
		return fmt.Errorf("control write host sentinel: %w", err)
	}

	// CONTROL — prove the READ-confinement predicate is LOAD-BEARING by INJECTING the
	// breach via OBSERVABLE state: a deliberately NON-confined container that DOES
	// mount the sentinel's host dir at its OWN absolute path. If the host FS WERE
	// reachable, the sentinel WOULD be readable at that exact path — assert the SAME
	// predicate hostPathReadableInside FIRES on it.
	leaky, err := s.runContainerWithHostMount(s.StateDir)
	if err != nil {
		return fmt.Errorf("control: start unconfined host-mount container: %w", err)
	}
	defer leaky.rm()
	if !hostPathReadableInside(leaky.boundary(), hostSentinel) {
		return fmt.Errorf("CONTROL VACUOUS: the read-confinement predicate did NOT fire even when the host ROOT was mounted (the sentinel %q should have been readable) — it cannot detect a host-FS breach, so the Run's confinement green proves nothing", hostSentinel)
	}
	// And the confined boundary must NOT expose it (the positive structural assertion
	// the Run depends on), so the predicate distinguishes confined from breached.
	cb, err := s.ProvisionContainer()
	if err != nil {
		return fmt.Errorf("control provision confined boundary: %w", err)
	}
	defer cb.Teardown()
	if hostPathReadableInside(cb, hostSentinel) {
		return fmt.Errorf("CONTROL: the CONFINED boundary exposed the host sentinel — confinement is not actually holding")
	}
	return nil
}

// ----------------------------------------------------------------------------
// container-trunk-confinement — the in-container agent cannot reach or advance the
// canonical/mediated trunk repo (no path from inside to the host trunk repo).
// ----------------------------------------------------------------------------

type containerTrunkConfinement struct{}

func (containerTrunkConfinement) ID() string           { return "container-trunk-confinement" }
func (containerTrunkConfinement) OwnerProject() string { return confinementOwner }
func (containerTrunkConfinement) Clause() string {
	return "0003 §10a / Inv 7 (container grain): the in-container agent has NO host path to the canonical/mediated trunk repo — it cannot read or advance trunk by reaching its files. The no-path guarantee is MOUNT-STRUCTURAL (the trunk repo is simply not mounted) and holds in both modes; this tier exercises it under the confined mode (opt-in) where the read-only rootfs also makes the write strictly fail. The worktree grain confined this only by hook policy, not structurally"
}

func (c containerTrunkConfinement) Run(s *Scratch) Result {
	if ok, reason := ContainerRuntimeAvailable(); !ok {
		return skip(c, "container-trunk-confinement: %s", reason)
	}
	// The no-host-path-to-trunk guarantee is mount-structural (the trunk repo is never
	// mounted), but boot the CONFINED grain so the 'cannot WRITE the host trunk ref path'
	// clause is backed by the read-only rootfs too (opt-in confined mode).
	if err := s.UseConfinedContainerGrain(); err != nil {
		return fail(c, "boot daemon on the confined container grain: %v", err)
	}

	// Establish a REAL trunk on the host so "unreachable" is non-vacuous: there is an
	// actual canonical trunk repo + ref to be unreachable. EstablishTrunkBase uses the
	// worktree-grain agent path (host git against the mediated bare repo) — independent
	// of the in-container agent, and it proves the trunk repo genuinely exists.
	agent, err := s.NewAgent("trunkbase")
	if err != nil {
		return fail(c, "new host agent: %v", err)
	}
	if _, err := s.EstablishTrunkBase(agent, map[string]string{"trunk.txt": "canonical\n"}); err != nil {
		return fail(c, "establish a real host trunk (so unreachability is non-vacuous): %v", err)
	}
	if !s.RefExists(s.BareDir, "refs/heads/trunk") {
		return fail(c, "the canonical trunk ref must exist on the host before asserting it is unreachable inside")
	}

	cb, err := s.ProvisionContainer()
	if err != nil {
		return fail(c, "provision container boundary: %v", err)
	}
	defer cb.Teardown()

	// The in-container agent cannot REACH the canonical trunk repo: its host path is
	// not mounted, so neither the repo dir nor its trunk ref file is present inside.
	if hostPathReadableInside(cb, s.BareDir) {
		return fail(c, "BREACH: the canonical/mediated trunk repo %q is READABLE inside the container — the agent has a structural path to trunk", s.BareDir)
	}
	if hostPathReadableInside(cb, filepath.Join(s.BareDir, "refs", "heads", "trunk")) {
		return fail(c, "BREACH: the trunk ref file under %q is READABLE inside the container", s.BareDir)
	}
	// Nor can it ADVANCE trunk by writing the host ref path (no path, read-only rootfs).
	if pathWritableInside(cb, filepath.Join(s.BareDir, "refs", "heads", "trunk")) {
		return fail(c, "BREACH: the agent WROTE the host trunk ref path from inside the container — it can advance trunk structurally")
	}

	return pass(c, "container grain STRUCTURAL: the canonical trunk repo %q (a REAL host trunk exists) has NO path inside the container — neither the repo nor its trunk ref is readable/writable; the agent cannot reach or advance trunk by its files",
		filepath.Base(s.BareDir))
}

func (c containerTrunkConfinement) Control(s *Scratch) error {
	if ok, _ := ContainerRuntimeAvailable(); !ok {
		return nil
	}
	if err := s.UseConfinedContainerGrain(); err != nil {
		return fmt.Errorf("control boot confined container grain: %w", err)
	}
	agent, err := s.NewAgent("trunkbase-ctl")
	if err != nil {
		return fmt.Errorf("control new host agent: %w", err)
	}
	if _, err := s.EstablishTrunkBase(agent, map[string]string{"trunk.txt": "canonical\n"}); err != nil {
		return fmt.Errorf("control establish host trunk: %w", err)
	}

	// CONTROL via OBSERVABLE state: a NON-confined container that DOES mount the trunk
	// repo at its OWN absolute path. If a path to trunk existed, the trunk repo WOULD
	// be readable inside at that exact path — assert the SAME reachability predicate
	// FIRES on the injected reachable path.
	leaky, err := s.runContainerWithHostMount(s.BareDir)
	if err != nil {
		return fmt.Errorf("control: start container with the trunk repo mounted: %w", err)
	}
	defer leaky.rm()
	if !hostPathReadableInside(leaky.boundary(), s.BareDir) {
		return fmt.Errorf("CONTROL VACUOUS: the trunk-reachability predicate did NOT fire even when the trunk repo was mounted at its own path %q — it cannot detect a reachable trunk, so the Run's unreachable verdict proves nothing", s.BareDir)
	}
	// The confined boundary must NOT expose it.
	cb, err := s.ProvisionContainer()
	if err != nil {
		return fmt.Errorf("control provision confined boundary: %w", err)
	}
	defer cb.Teardown()
	if hostPathReadableInside(cb, s.BareDir) {
		return fmt.Errorf("CONTROL: the CONFINED boundary exposed the trunk repo path — confinement is not holding")
	}
	return nil
}

// ----------------------------------------------------------------------------
// container-network-confinement — the in-container agent cannot reach the
// governance plane: the daemon socket is on a host path NOT mounted into the
// container, so it is structurally unreachable from inside.
// ----------------------------------------------------------------------------

type containerNetworkConfinement struct{}

func (containerNetworkConfinement) ID() string           { return "container-network-confinement" }
func (containerNetworkConfinement) OwnerProject() string { return confinementOwner }
func (containerNetworkConfinement) Clause() string {
	return "0003 §10a / Inv 4-5 (container grain, STRUCTURAL): the in-container agent cannot reach the GOVERNANCE PLANE nor the wider network — the daemon's unix socket lives on a host path NOT mounted into the container (structurally unreachable from inside), AND the grain runs --network none so there is no netns interface/route: OUTBOUND EGRESS to a routable IP FAILS (no data-exfiltration channel). The arbiter is reachable only through the launcher's mediated exec, never directly"
}

func (c containerNetworkConfinement) Run(s *Scratch) Result {
	if ok, reason := ContainerRuntimeAvailable(); !ok {
		return skip(c, "container-network-confinement: %s", reason)
	}
	// EXPLICITLY opt into the CONFINED mode (MAD_CONTAINER_CONFINED=1, which implies
	// network none): egress is now COOPERATIVE-by-default, so this tier must REQUEST
	// confinement to prove it holds when chosen. Without this the boundary would get the
	// runtime's default network and the egress assertion below would (correctly) see OPEN
	// egress.
	if err := s.UseConfinedContainerGrain(); err != nil {
		return fail(c, "boot daemon on the confined container grain (confined ⇒ network none): %v", err)
	}

	cb, err := s.ProvisionContainer()
	if err != nil {
		return fail(c, "provision container boundary: %v", err)
	}
	defer cb.Teardown()

	// STRUCTURAL, host-INDEPENDENT guarantee: the daemon socket (the SOLE governance
	// channel) is a host path the container grain never mounts. Its directory and the
	// socket file are absent inside — the agent has no path to dial the arbiter.
	sockDir := filepath.Dir(s.Socket)
	if hostPathReadableInside(cb, sockDir) {
		return fail(c, "BREACH: the daemon socket directory %q is READABLE inside the container — the governance plane is reachable", sockDir)
	}
	if hostPathReadableInside(cb, s.Socket) {
		return fail(c, "BREACH: the daemon socket %q is present inside the container — the agent can reach the arbiter directly", s.Socket)
	}

	// OUTBOUND EGRESS confinement (STRUCTURAL, now that the grain runs --network none):
	// the agent has NO netns interface/route, so a connect to a routable IP FAILS.
	// This closes the data-exfiltration channel a default NAT network would open AND is
	// what makes the governance socket unreachable even if it were network-exposed.
	if outboundEgressOpenInside(cb) {
		return fail(c, "BREACH: an uncooperative in-container agent reached a routable IP (%s) — outbound egress is OPEN; the container is not network-confined (data-exfiltration channel)", egressRoutableIP)
	}

	return pass(c, "container grain STRUCTURAL: the daemon socket %q and its directory are NOT present inside the container (no path to the governance plane) AND outbound egress to a routable IP (%s) FAILS (--network none — no netns interface/route, no data-exfiltration channel)",
		filepath.Base(s.Socket), egressRoutableIP)
}

func (c containerNetworkConfinement) Control(s *Scratch) error {
	if ok, _ := ContainerRuntimeAvailable(); !ok {
		return nil
	}
	// Same CONFINED opt-in as Run: the confined boundary `cb` provisioned below must
	// have NO egress for the egress probe to distinguish confined from breached.
	if err := s.UseConfinedContainerGrain(); err != nil {
		return fmt.Errorf("control boot confined container grain (confined ⇒ network none): %w", err)
	}
	sockDir := filepath.Dir(s.Socket)

	// CONTROL via OBSERVABLE state: a NON-confined container that DOES mount the socket
	// directory at its OWN absolute path. If the governance plane were reachable, the
	// socket dir WOULD be present at that exact path — assert the SAME reachability
	// predicate FIRES on the injected reachable path.
	leaky, err := s.runContainerWithHostMount(sockDir)
	if err != nil {
		return fmt.Errorf("control: start container with the socket dir mounted: %w", err)
	}
	defer leaky.rm()
	if !hostPathReadableInside(leaky.boundary(), sockDir) {
		return fmt.Errorf("CONTROL VACUOUS: the governance-plane reachability predicate did NOT fire even when the socket directory was mounted at its own path %q — it cannot detect a reachable governance plane, so the Run's unreachable verdict proves nothing", sockDir)
	}
	// The confined boundary must NOT expose it.
	cb, err := s.ProvisionContainer()
	if err != nil {
		return fmt.Errorf("control provision confined boundary: %w", err)
	}
	defer cb.Teardown()
	if hostPathReadableInside(cb, sockDir) {
		return fmt.Errorf("CONTROL: the CONFINED boundary exposed the socket directory — confinement is not holding")
	}

	// CONTROL (C24) for the EGRESS assertion via OBSERVABLE state: a deliberately
	// NON-confined container on the DEFAULT network (no --network none). If the
	// runtime's network were always dead, the Run's "egress FAILS" green would prove
	// nothing — so assert the SAME egress probe SUCCEEDS here, proving the namespace
	// removal is what blocks egress (not a runtime that simply has no network).
	leakyNet, err := s.runContainerDefaultNetwork()
	if err != nil {
		return fmt.Errorf("control: start default-network container: %w", err)
	}
	defer leakyNet.rm()
	if !outboundEgressOpenInside(leakyNet.boundary()) {
		return fmt.Errorf("CONTROL VACUOUS: the egress probe did NOT succeed even on a DEFAULT-network container (egress to %s should have worked) — the probe cannot detect open egress, so the Run's egress-blocked green proves nothing", egressRoutableIP)
	}
	// And the confined boundary must NOT have egress (the positive structural assertion
	// the Run depends on), so the probe distinguishes confined from breached.
	if outboundEgressOpenInside(cb) {
		return fmt.Errorf("CONTROL: the CONFINED boundary had OPEN egress to %s — --network none is not actually holding", egressRoutableIP)
	}
	return nil
}

// ----------------------------------------------------------------------------
// in-container probe predicates (OBSERVABLE state only — no self-reported flag).
// ----------------------------------------------------------------------------

// hostPathReadableInside reports whether the given HOST path is readable from inside
// the container boundary — observable via the exit code of an in-container stat/cat.
// A non-zero exit (the path is absent) means it is NOT reachable.
func hostPathReadableInside(cb *ContainerBoundary, hostPath string) bool {
	// `test -e` is the cheapest existence check available in the alpine image.
	_, err := cb.ExecInContainer("test", "-e", hostPath)
	return err == nil
}

// pathWritableInside reports whether the given path is writable from inside the
// container — observable: write a byte then confirm it landed. A failed write
// (read-only rootfs / no such path) returns false.
func pathWritableInside(cb *ContainerBoundary, path string) bool {
	out, err := cb.ExecInContainer("sh", "-c", "echo nm-probe > "+path+" 2>/dev/null && test -s "+path+" && echo WROTE")
	if err != nil {
		return false
	}
	return strings.Contains(out, "WROTE")
}

// writeHostFile writes content to a host path with 0600 perms (the sentinel secret).
func writeHostFile(path, content string) error {
	return osWriteFile(path, content)
}

// egressRoutableIP is a well-known routable address used purely as an outbound
// reachability target. The probe asserts the in-container agent CANNOT open a
// connection to it (no egress under --network none) — it never depends on the
// remote actually serving content, only on whether the TCP/ICMP path exists.
const egressRoutableIP = "1.1.1.1"

// outboundEgressOpenInside reports whether the in-container agent can reach a
// ROUTABLE address — observable via the exit code of an in-container connect probe.
// It tries wget (HTTP), then a raw /dev/tcp dial, then ping; ANY success means
// egress is open. A short timeout keeps a blocked probe from hanging. Under
// --network none there is no eth0/route, so every attempt fails → returns false.
func outboundEgressOpenInside(cb *ContainerBoundary) bool {
	probe := "wget -T3 -q -O- http://" + egressRoutableIP + " >/dev/null 2>&1 && echo NM-EGRESS-OPEN; " +
		"(exec 3<>/dev/tcp/" + egressRoutableIP + "/80) 2>/dev/null && echo NM-EGRESS-OPEN; " +
		"ping -c1 -W3 " + egressRoutableIP + " >/dev/null 2>&1 && echo NM-EGRESS-OPEN; true"
	out, err := cb.ExecInContainer("sh", "-c", probe)
	if err != nil {
		return false
	}
	return strings.Contains(out, "NM-EGRESS-OPEN")
}
