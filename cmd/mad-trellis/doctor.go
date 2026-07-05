package main

import (
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"

	"github.com/spf13/cobra"

	"github.com/madhavhaldia/mad-trellis/internal/buildinfo"
	"github.com/madhavhaldia/mad-trellis/internal/coopembed"
	"github.com/madhavhaldia/mad-trellis/internal/protocol"
	"github.com/madhavhaldia/mad-trellis/internal/rpcclient"
	"github.com/madhavhaldia/mad-trellis/internal/runtimecfg"
)

// defaultContainerImageHint mirrors substrate.defaultContainerImage for doctor's
// image-cached check (cmd does not import the substrate's unexported default).
const defaultContainerImageHint = "alpine:latest"

// doctorCmd is the preflight diagnostic: it reports the resolved runtime dir +
// socket (and WHY each was chosen), enforces the conducted-git floor fail-closed,
// probes daemon reachability over the FROZEN diag.health method, and prints the
// binary's version pins. It is PURE-ADDITIVE (no new daemon RPC).
//
// FAIL POLICY (exit 1 when any FAILURE accrues):
//   - git missing or below the manifest floor  -> FAILURE (a conducted tool the
//     integrator's `git merge-tree --write-tree` gate hard-depends on, C-floor).
//   - a reachable daemon reporting a DIFFERENT contract_version -> FAILURE.
//   - daemon NOT reachable -> WARN only (you may simply not have started it).
//   - MAD_RUNTIME_DIR and MAD_HOME set and differing -> WARN (C23):
//     a TS adapter reading _HOME and the Go CLI reading _RUNTIME_DIR would land
//     on different sockets.
//
// The substantive checks (manifest load, version compare, git floor) live in
// internal/buildinfo where they are unit-tested; this file only dials, prints,
// accumulates failures, and exits.
func doctorCmd() *cobra.Command {
	var socket string
	cmd := &cobra.Command{
		Use:          "doctor",
		Short:        "Diagnose the local mad-trellis install (runtime paths, git floor, daemon reachability, version pins)",
		Args:         cobra.NoArgs,
		SilenceUsage: true,
		// Run (not RunE): doctor reports problems inline and controls its own exit
		// code; a non-zero exit must NOT print an extra cobra "error:" line.
		Run: func(cmd *cobra.Command, _ []string) {
			out := cmd.OutOrStdout()
			problems := 0

			m, manErr := buildinfo.Load()
			if manErr != nil {
				fmt.Fprintf(out, "FAIL: could not load embedded version manifest: %v\n", manErr)
				problems++
			}

			// --- runtime dir + socket resolution (and why) -------------------
			rtDir, rtSrc := runtimecfg.RuntimeDirSource()
			sockPath, sockSrc := runtimecfg.SocketSource(socket)
			// When per-repo auto-defaulting set the runtime this run, report THAT as the
			// origin (it set MAD_RUNTIME_DIR under the hood) so the operator sees
			// the true reason rather than a bare "MAD_RUNTIME_DIR".
			if perRepoRuntimeRoot != "" {
				rtSrc = runtimecfg.SourcePerRepo + " from " + perRepoRuntimeRoot
				if sockSrc == runtimecfg.SourceRuntimeDir {
					sockSrc = rtSrc
				}
			}
			fmt.Fprintf(out, "runtime dir: %s [%s]\n", rtDir, rtSrc)
			fmt.Fprintf(out, "socket:      %s [%s]\n", sockPath, sockSrc)
			if rd, hm, diverges := runtimecfg.Divergence(); diverges {
				fmt.Fprintf(out,
					"WARN: MAD_RUNTIME_DIR (%s) and MAD_HOME (%s) are both set and differ; "+
						"the Go CLI uses _RUNTIME_DIR while a TS adapter may use _HOME — they could resolve different sockets\n",
					rd, hm)
			}

			// --- conducted git floor (fail-closed) ---------------------------
			gitMin := "0.0.0"
			if manErr == nil {
				if g, ok := m.ConductedTools["git"]; ok {
					gitMin = g.Min
				}
			}
			have, ok, gerr := buildinfo.CheckGit(gitMin)
			switch {
			case gerr != nil:
				fmt.Fprintf(out, "FAIL: git: %v (required >= %s for the integrator's merge-tree --write-tree gate)\n", gerr, gitMin)
				problems++
			case !ok:
				fmt.Fprintf(out, "FAIL: git %s is below the required minimum %s (integrator merge-tree --write-tree gate)\n", have, gitMin)
				problems++
			default:
				fmt.Fprintf(out, "git %s (>= %s) OK\n", have, gitMin)
			}

			// --- daemon reachability (not-running is a WARN, not a failure) ---
			cl, dialErr := rpcclient.Dial(sockPath)
			if dialErr != nil {
				fmt.Fprintf(out, "daemon: not running (OK if you have not started it)\n")
			} else {
				var h daemonHealth
				callErr := cl.Call("diag.health", map[string]any{}, &h)
				cl.Close()
				if callErr != nil {
					fmt.Fprintf(out, "FAIL: daemon reachable at %s but diag.health failed: %v\n", sockPath, callErr)
					problems++
				} else if h.ContractVersion != protocol.ContractVersion {
					fmt.Fprintf(out,
						"FAIL: daemon contract v%d != this binary's contract v%d (mismatch; rebuild/restart the daemon)\n",
						h.ContractVersion, protocol.ContractVersion)
					problems++
				} else {
					fmt.Fprintf(out, "daemon: running (pid %d, contract v%d)\n", h.PID, h.ContractVersion)
				}
			}

			// --- container runtime (only when the container grain / relay is set) ---
			grain := strings.ToLower(strings.TrimSpace(os.Getenv("MAD_GRAIN")))
			relayPath := strings.TrimSpace(os.Getenv("MAD_CONTAINER_RELAY"))
			if grain == containerGrainName || relayPath != "" {
				image := strings.TrimSpace(os.Getenv("MAD_CONTAINER_IMAGE"))
				if image == "" {
					image = defaultContainerImageHint
				}
				problems += checkContainerRuntime(out, grain == containerGrainName, image, relayPath,
					coopembed.Available(),
					exec.LookPath,
					func(name string, args ...string) (string, error) {
						b, err := exec.Command(name, args...).CombinedOutput()
						return string(b), err
					},
					os.Stat,
				)
			}

			// --- binary identity + pins --------------------------------------
			fmt.Fprintf(out, "mad-trellis %s (commit %s, contract v%d)\n", version, commit, protocol.ContractVersion)
			if manErr == nil {
				fmt.Fprintf(out, "go pin:    %s\n", m.Go)
				fmt.Fprintf(out, "platforms: %v\n", m.Platforms)
			}

			// --- verdict -----------------------------------------------------
			if problems == 0 {
				fmt.Fprintf(out, "doctor: OK\n")
				return
			}
			fmt.Fprintf(out, "doctor: %d problem(s) found\n", problems)
			os.Exit(1)
		},
	}
	cmd.Flags().StringVar(&socket, "socket", "", socketFlagHelp)
	return cmd
}

// checkContainerRuntime reports container-runtime readiness when the container
// grain (or a configured relay) is in play, returning the number of FAILURES (each
// already printed). Dependencies are injected so the orchestration is unit-tested
// without a real runtime. POLICY: when the container grain is ACTIVELY selected
// (hardFail), a missing CLI / unreachable apiserver is a FAILURE (launch would
// BLOCK); a missing image is a WARN (it is pulled on first launch); a configured-
// but-missing relay is a WARN (the cooperative plane is fail-soft).
func checkContainerRuntime(
	out io.Writer,
	hardFail bool,
	image, relayPath string,
	relayEmbedded bool,
	lookPath func(string) (string, error),
	run func(name string, args ...string) (string, error),
	stat func(string) (os.FileInfo, error),
) int {
	problems := 0
	fail := func(format string, a ...any) {
		if hardFail {
			fmt.Fprintf(out, "FAIL: "+format+"\n", a...)
			problems++
		} else {
			fmt.Fprintf(out, "WARN: "+format+"\n", a...)
		}
	}

	// (1) the `container` CLI on PATH — nothing else is checkable without it.
	if _, err := lookPath("container"); err != nil {
		fail("container runtime CLI not on PATH (the container grain requires it): %v", err)
		return problems
	}
	// (2) apiserver reachable.
	if o, err := run("container", "list", "-a", "-q"); err != nil {
		fail("container apiserver not reachable (start it with `container system start`): %v: %s", err, strings.TrimSpace(o))
	} else {
		fmt.Fprintf(out, "container runtime: CLI present, apiserver reachable\n")
	}
	// (3) image cached — a miss is a WARN (pulled on first launch).
	if lsOut, err := run("container", "image", "ls"); err != nil {
		fmt.Fprintf(out, "WARN: could not list container images: %v\n", err)
	} else if !buildinfo.ImageCached(lsOut, image) {
		fmt.Fprintf(out, "WARN: container image %q not cached (pulled on first launch)\n", image)
	} else {
		fmt.Fprintf(out, "container image %q cached\n", image)
	}
	// (4) cooperative-plane relay, only if configured (fail-soft → WARN).
	if relayPath != "" {
		if fi, err := stat(relayPath); err != nil {
			fmt.Fprintf(out, "WARN: MAD_CONTAINER_RELAY %q not found (cooperative plane will be off): %v\n", relayPath, err)
		} else if fi.IsDir() || fi.Mode().Perm()&0o111 == 0 {
			fmt.Fprintf(out, "WARN: MAD_CONTAINER_RELAY %q is not an executable file (cooperative plane will be off)\n", relayPath)
		} else {
			fmt.Fprintf(out, "cooperative relay: %s OK\n", relayPath)
		}
	}
	// (5) embedded cooperative plane — the make-built default. Independent of any
	// host relay override above (fail-soft → WARN, never a FAILURE). When the relay
	// is embedded the container grain carries its own in-container cooperative plane;
	// a binary built with a plain `go build` (no -tags coopembed) embeds nothing, so
	// the container grain runs confined WITHOUT the plane unless an explicit relay is
	// pointed at via MAD_CONTAINER_RELAY.
	if relayEmbedded {
		fmt.Fprintf(out, "cooperative relay: embedded (container grain has the in-container cooperative plane)\n")
	} else if relayPath == "" {
		fmt.Fprintf(out, "WARN: this binary was built without -tags coopembed (not via `make build`/`make install`); "+
			"the container grain runs confined WITHOUT the cooperative plane — set MAD_CONTAINER_RELAY to override, or build via make\n")
	}
	return problems
}
