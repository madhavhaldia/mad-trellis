package launcher

import (
	"os"

	"github.com/madhavhaldia/mad-substrate/internal/rpcclient"
)

// RunIntegratorPTY runs an integrator agent in the current worktree: PTY plumbing
// plus integrator-audience nudges, but no substrate boundary provisioning. It is
// intentionally narrower than Run: the integrator lives on the trunk side by
// design, while the nudge plane remains fail-soft.
func RunIntegratorPTY(socket, cwd string, extraEnv map[string]string, agent string, args []string) (int, error) {
	env := copyEnvMap(extraEnv)
	env["MAD_LAUNCHED"] = "1"

	ncfg := nudgeConfig{Audience: "integrator"}
	if !nudgesDisabledByEnv() {
		if conn, err := rpcclient.Dial(socket); err == nil {
			nudgeConn := serializeConn(conn)
			defer nudgeConn.Close()
			ncfg.Source = nudgeSourceFromConn(nudgeConn, "")
			ncfg.Audit = nudgeAudit(nudgeConn)
		}
	}
	return runPTYIOWithOptions(os.Stdin, os.Stdout, ExecTarget{Cwd: cwd}, env, agent, args, ptyRunOptions{Nudges: ncfg})
}

func copyEnvMap(in map[string]string) map[string]string {
	out := make(map[string]string, len(in)+1)
	for k, v := range in {
		out[k] = v
	}
	return out
}
