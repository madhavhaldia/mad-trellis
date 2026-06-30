package launcher

import (
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/madhavhaldia/mad-substrate/internal/rpcclient"
)

// TestCoopLiveContainerE2E is the FULL-STACK live proof of the cooperative-plane
// exec-stdio transport (#2): a REAL container-grain boundary, the REAL relay
// binary exec'd inside it, the REAL launcher pump, and an in-container probe that
// does a REAL session.attach + session.whoami + lease.acquire against a REAL
// daemon — over --network none, with no host socket path. It asserts the lease the
// probe takes inside the container is held by the LAUNCHER's session id (the
// adapter's token-authed attach rebinds the forwarded connection), which is the
// whole point of #2.
//
// It is GATED (skips unless the inputs are present) so the normal suite + CI stay
// fast and hermetic; it is meant to be run by hand against a live container-grain
// daemon, e.g.:
//
//	MAD_COOP_LIVE_SOCKET=/tmp/nm2e.sock \
//	MAD_COOP_LIVE_RELAY=/tmp/nm2-bin/mad-substrate-relay \
//	MAD_COOP_LIVE_PROBE=/tmp/nm2-bin/mad-substrate-coopprobe \
//	go test ./internal/launcher -run TestCoopLiveContainerE2E -v
//
// The daemon at the socket MUST already be on the container grain
// (MAD_GRAIN=container) so Provision yields a container boundary.
func TestCoopLiveContainerE2E(t *testing.T) {
	sock := os.Getenv("MAD_COOP_LIVE_SOCKET")
	relay := os.Getenv("MAD_COOP_LIVE_RELAY")
	probe := os.Getenv("MAD_COOP_LIVE_PROBE")
	if sock == "" || relay == "" || probe == "" {
		t.Skip("set MAD_COOP_LIVE_{SOCKET,RELAY,PROBE} to run the live coop e2e")
	}
	if _, err := exec.LookPath("container"); err != nil {
		t.Skip("no `container` runtime on PATH")
	}

	// (1) Open a session and provision a CONTAINER boundary on the held connection.
	sess, err := Open(func(s string) (Conn, error) { return rpcclient.Dial(s) }, sock)
	if err != nil {
		t.Fatalf("open session: %v", err)
	}
	defer sess.Close()
	spec, err := sess.Provision(0, nil)
	if err != nil {
		t.Fatalf("provision: %v", err)
	}
	if spec.Grain != containerGrainName || spec.ContainerID == "" {
		t.Fatalf("daemon is not on the container grain (grain=%q id=%q); start it with MAD_GRAIN=container", spec.Grain, spec.ContainerID)
	}
	defer sess.Teardown()
	scratch := spec.Env["MAD_SCRATCH"]
	if scratch == "" {
		t.Fatalf("no MAD_SCRATCH in the provisioned env")
	}

	// (2) Establish the shareable session identity (mint token + acquire the
	// session-liveness lease) so the probe's attach passes the liveness gate. A long
	// TTL keeps it live for the whole test without a keepalive goroutine.
	token, lk, err := sess.MintToken()
	if err != nil {
		t.Fatalf("mint token: %v", err)
	}
	if err := sess.AcquireSessionLease(lk, 120*time.Second); err != nil {
		t.Fatalf("acquire session lease: %v", err)
	}
	defer sess.ReleaseOwnLeases()

	// (3) Start the REAL cooperative plane: relay exec'd in the container + pump.
	stop, err := startCoop(coopConfig{
		containerBin:    containerBin,
		containerID:     spec.ContainerID,
		relayHostPath:   relay,
		scratchDir:      scratch,
		inContainerSock: coopSocketPath,
		daemonSocket:    sock,
		logf:            t.Logf,
	})
	if err != nil {
		t.Fatalf("startCoop: %v", err)
	}
	defer stop()

	// (4) Stage the probe into the same writable scratch (mounted at the same path
	// in-container) and exec it INSIDE the container, connecting to the relay socket.
	probeDst := filepath.Join(scratch, ".mad-substrate-coopprobe")
	if err := copyExecutable(probe, probeDst); err != nil {
		t.Fatalf("stage probe: %v", err)
	}
	out, err := exec.Command(containerBin, "exec", spec.ContainerID, probeDst, coopSocketPath, token, sess.ID()).CombinedOutput()
	t.Logf("in-container probe output:\n%s", out)
	if err != nil {
		t.Fatalf("in-container probe failed: %v", err)
	}
	got := string(out)
	if !strings.Contains(got, "COOP-PROBE-OK") {
		t.Fatalf("probe did not report success:\n%s", got)
	}
	if !strings.Contains(got, "holder="+sess.ID()) {
		t.Fatalf("lease taken inside the container is NOT held by the launcher session %q:\n%s", sess.ID(), got)
	}
	if !strings.Contains(got, "attached session="+sess.ID()) {
		t.Fatalf("probe did not attach to the launcher session %q:\n%s", sess.ID(), got)
	}
}

func copyExecutable(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o755)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		_ = out.Close()
		return err
	}
	return out.Close()
}
