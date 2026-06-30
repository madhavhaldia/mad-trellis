package substrate

// EnvSpec is the IMMUTABLE, grain-agnostic description of one agent's forkable
// boundary. The substrate PRODUCES it; the launcher (project 5) APPLIES it (sets
// cwd, merges env, execs). The substrate itself never execs, redirects a remote,
// or routes a singular resource — those belong to the launcher / integrator /
// gate. Immutability is structural: the fields are unexported and every accessor
// hands back a COPY, so a consumer cannot mutate the provisioned boundary.
type EnvSpec struct {
	session      string
	grain        string
	cwd          string
	branch       string
	hostWorktree string // host-side worktree path (== cwd at the worktree grain)
	containerID  string // the confined container id (empty for non-container grains)
	ports        []int
	env          map[string]string
	stateDirs    map[string]string  // role -> path (scratch/cache/state)
	stateRoot    string             // the agent's local-state root (for teardown)
	deferred     []DeferredResource // declared NON-forkable resources, routed elsewhere
}

// DeferredResource is a declared resource the substrate did NOT auto-isolate
// because the classifier judged it Convergent or Singular (Inv 2(b): what to
// isolate is the classifier's verdict, never the substrate's). It is recorded
// for the integrator (convergent) / gate (singular) to route — the substrate
// never forks it.
type DeferredResource struct {
	Name string `json:"name"`
	Kind string `json:"kind"` // "convergent" | "singular"
}

// Session is the daemon-bound identity this boundary belongs to.
func (s *EnvSpec) Session() string { return s.session }

// Grain is the isolation backend that produced this boundary ("worktree" in v1).
func (s *EnvSpec) Grain() string { return s.grain }

// Cwd is the agent's working directory (the worktree path at v1 grain).
func (s *EnvSpec) Cwd() string { return s.cwd }

// Branch is the worktree's branch (empty for non-git grains).
func (s *EnvSpec) Branch() string { return s.branch }

// HostWorktree is the host-side path of the agent's git worktree. At the
// container grain Cwd is the in-container mount point (/work) while this is the
// real host dir bind-mounted there; at the worktree grain they coincide.
func (s *EnvSpec) HostWorktree() string { return s.hostWorktree }

// ContainerID is the confined container the launcher execs into (empty for
// non-container grains).
func (s *EnvSpec) ContainerID() string { return s.containerID }

// Ports returns a copy of the disjoint port block allocated to this agent.
func (s *EnvSpec) Ports() []int {
	out := make([]int, len(s.ports))
	copy(out, s.ports)
	return out
}

// Env returns a copy of the routed runtime environment (per-agent ports,
// local-state paths, and any classifier-forkable resource paths).
func (s *EnvSpec) Env() map[string]string { return copyMap(s.env) }

// StateDirs returns a copy of the per-agent local-state dirs (role -> path).
func (s *EnvSpec) StateDirs() map[string]string { return copyMap(s.stateDirs) }

// Deferred returns a copy of the declared resources routed away from isolation.
func (s *EnvSpec) Deferred() []DeferredResource {
	out := make([]DeferredResource, len(s.deferred))
	copy(out, s.deferred)
	return out
}

// Wire is the JSON-serializable projection of an EnvSpec for the daemon RPC
// surface (substrate.provision returns this; the launcher/spawn decode it).
type Wire struct {
	Session string `json:"session"`
	Grain   string `json:"grain"`
	Cwd     string `json:"cwd"`
	Branch  string `json:"branch"`
	// HostWorktree and ContainerID are additive fields the container grain
	// populates so the launcher can `container exec` INTO the boundary; both are
	// "" (omitted) at the worktree grain, so the wire shape is backward-compatible.
	HostWorktree string             `json:"host_worktree,omitempty"`
	ContainerID  string             `json:"container_id,omitempty"`
	Ports        []int              `json:"ports"`
	Env          map[string]string  `json:"env"`
	StateDirs    map[string]string  `json:"state_dirs"`
	Deferred     []DeferredResource `json:"deferred"`
}

// Wire projects the immutable spec to its serializable form (copies all maps/
// slices, so the wire value shares no mutable state with the live boundary).
func (s *EnvSpec) Wire() Wire {
	return Wire{
		Session:      s.session,
		Grain:        s.grain,
		Cwd:          s.cwd,
		Branch:       s.branch,
		HostWorktree: s.hostWorktree,
		ContainerID:  s.containerID,
		Ports:        s.Ports(),
		Env:          s.Env(),
		StateDirs:    s.StateDirs(),
		Deferred:     s.Deferred(),
	}
}

func copyMap(m map[string]string) map[string]string {
	out := make(map[string]string, len(m))
	for k, v := range m {
		out[k] = v
	}
	return out
}
