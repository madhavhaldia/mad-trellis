package launcher

import (
	"context"
	"io"
	"os"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

const (
	defaultNudgePollInterval = 2 * time.Second
	defaultNudgeQuietPeriod  = 2 * time.Second
	defaultNudgeRetryAfter   = 100 * time.Millisecond
	nudgeEventMax            = 200
)

type nudgeEvent struct {
	Kind   string
	Branch string
}

type nudgeSource func(context.Context) ([]nudgeEvent, error)

type nudgeConfig struct {
	Audience string
	Branch   string
	Source   nudgeSource
	Audit    func(context.Context, string, []nudgeEvent)

	PollInterval time.Duration
	QuietPeriod  time.Duration
	RetryAfter   time.Duration
	// SubmitDelay isolates the submit keypress from the injected body (see
	// inject.go). 0 -> defaultNudgeSubmitDelay.
	SubmitDelay time.Duration
}

type ptyRunOptions struct {
	Nudges nudgeConfig
}

type inputActivity struct {
	mu   sync.Mutex
	last time.Time
}

func (a *inputActivity) mark(now time.Time) {
	a.mu.Lock()
	a.last = now
	a.mu.Unlock()
}

func (a *inputActivity) quietFor(d time.Duration, now time.Time) bool {
	a.mu.Lock()
	last := a.last
	a.mu.Unlock()
	return last.IsZero() || now.Sub(last) >= d
}

type activityReader struct {
	r        io.Reader
	activity *inputActivity
}

func (r activityReader) Read(p []byte) (int, error) {
	n, err := r.r.Read(p)
	if n > 0 && r.activity != nil {
		r.activity.mark(time.Now())
	}
	return n, err
}

type lockedWriter struct {
	mu *sync.Mutex
	w  io.Writer
}

func (w lockedWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.w.Write(p)
}

type serializedConn struct {
	mu sync.Mutex
	c  Conn
}

func (c *serializedConn) Call(method string, params any, out any) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.c.Call(method, params, out)
}

func (c *serializedConn) Close() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.c.Close()
}

func serializeConn(c Conn) Conn {
	if c == nil {
		return nil
	}
	return &serializedConn{c: c}
}

func startNudgeLoop(ctx context.Context, inj lineInjector, activity *inputActivity, cfg nudgeConfig) {
	if nudgesDisabledByEnv() || cfg.Source == nil {
		return
	}
	poll := cfg.PollInterval
	if poll <= 0 {
		poll = defaultNudgePollInterval
	}

	state := &nudgeDeliveryState{
		inj:        inj,
		activity:   activity,
		cfg:        cfg,
		quiet:      durationOr(cfg.QuietPeriod, defaultNudgeQuietPeriod),
		retryAfter: durationOr(cfg.RetryAfter, defaultNudgeRetryAfter),
	}

	pollOnce := func() {
		events, err := cfg.Source(ctx)
		if err != nil || len(events) == 0 {
			return
		}
		state.enqueue(ctx, events)
	}

	ticker := time.NewTicker(poll)
	go func() {
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				pollOnce()
			}
		}
	}()
}

type nudgeDeliveryState struct {
	mu         sync.Mutex
	pending    []nudgeEvent
	delivering bool

	inj        lineInjector
	activity   *inputActivity
	cfg        nudgeConfig
	quiet      time.Duration
	retryAfter time.Duration
}

func (s *nudgeDeliveryState) enqueue(ctx context.Context, events []nudgeEvent) {
	s.mu.Lock()
	s.pending = append(s.pending, events...)
	if s.delivering {
		s.mu.Unlock()
		return
	}
	s.delivering = true
	s.mu.Unlock()
	go s.deliver(ctx)
}

func (s *nudgeDeliveryState) deliver(ctx context.Context) {
	for {
		if !s.waitQuiet(ctx) {
			s.mu.Lock()
			s.delivering = false
			s.mu.Unlock()
			return
		}
		s.mu.Lock()
		if len(s.pending) == 0 {
			s.delivering = false
			s.mu.Unlock()
			return
		}
		batch := append([]nudgeEvent(nil), s.pending...)
		s.pending = nil
		s.mu.Unlock()
		text, ok := buildNudgeText(s.cfg.Audience, s.cfg.Branch, batch)
		if !ok {
			continue
		}
		if err := s.inj.Inject(text); err == nil && s.cfg.Audit != nil {
			s.cfg.Audit(ctx, s.cfg.Audience, batch)
		}
	}
}

func (s *nudgeDeliveryState) waitQuiet(ctx context.Context) bool {
	if s.activity == nil {
		return true
	}
	for {
		if s.activity.quietFor(s.quiet, time.Now()) {
			return true
		}
		timer := time.NewTimer(s.retryAfter)
		select {
		case <-ctx.Done():
			if !timer.Stop() {
				select {
				case <-timer.C:
				default:
				}
			}
			return false
		case <-timer.C:
		}
	}
}

// buildNudgeText assembles the fixed-template body for one delivery batch. It is
// PURE TEXT: no carriage returns / submit bytes — how a body is inserted and
// submitted is the injector's job (inject.go), never the template's. Multi-kind
// batches join with \n, which the injector delivers inside one paste so the
// whole batch rides a single submit.
func buildNudgeText(audience, branch string, events []nudgeEvent) (string, bool) {
	if audience == "integrator" {
		line := "[mad-trellis] " + strconv.Itoa(len(events)) + " integration request(s) awaiting review — run mad_integration_pending and process them."
		return line, true
	}

	if branch == "" && len(events) > 0 {
		branch = events[0].Branch
	}
	kinds := uniqueNudgeKinds(events)
	lines := make([]string, 0, len(kinds))
	for _, kind := range kinds {
		switch kind {
		case "integration.verdict":
			lines = append(lines, "[mad-trellis] your integration request on "+branch+" has a verdict — run mad_integration_status.")
		case "integration.claimed":
			lines = append(lines, "[mad-trellis] your integration request on "+branch+" was claimed for review.")
		}
	}
	if len(lines) == 0 {
		return "", false
	}
	return strings.Join(lines, "\n"), true
}

func uniqueNudgeKinds(events []nudgeEvent) []string {
	seen := map[string]bool{}
	out := make([]string, 0, len(events))
	for _, ev := range events {
		if ev.Kind == "" || seen[ev.Kind] {
			continue
		}
		seen[ev.Kind] = true
		out = append(out, ev.Kind)
	}
	return out
}

func sortedNudgeKinds(events []nudgeEvent) []string {
	kinds := uniqueNudgeKinds(events)
	sort.Strings(kinds)
	return kinds
}

func durationOr(v, fallback time.Duration) time.Duration {
	if v > 0 {
		return v
	}
	return fallback
}

func nudgesDisabledByEnv() bool {
	switch strings.ToLower(strings.TrimSpace(os.Getenv("MAD_NUDGES"))) {
	case "off", "0", "false", "no":
		return true
	default:
		return false
	}
}

func nudgeSourceFromConn(conn Conn, branch string) nudgeSource {
	return func(context.Context) ([]nudgeEvent, error) {
		params := map[string]any{"max": nudgeEventMax}
		if strings.TrimSpace(branch) != "" {
			params["branch"] = branch
		}
		var out struct {
			Events []struct {
				Kind   string `json:"kind"`
				Branch string `json:"branch"`
			} `json:"events"`
		}
		if err := conn.Call("integration.events", params, &out); err != nil {
			return nil, err
		}
		events := make([]nudgeEvent, 0, len(out.Events))
		for _, ev := range out.Events {
			events = append(events, nudgeEvent{Kind: ev.Kind, Branch: ev.Branch})
		}
		return events, nil
	}
}

func nudgeAudit(conn Conn) func(context.Context, string, []nudgeEvent) {
	return func(_ context.Context, audience string, events []nudgeEvent) {
		var out struct {
			OK bool `json:"ok"`
		}
		_ = conn.Call("audit.append", map[string]any{
			"decision_project": "nudge",
			"decision_kind":    "nudge.delivered",
			"payload": map[string]any{
				"audience": audience,
				"kinds":    sortedNudgeKinds(events),
			},
		}, &out)
	}
}

func attachNudgeConn(dial Dialer, socket, token string) (Conn, error) {
	conn, err := dial(socket)
	if err != nil {
		return nil, err
	}
	var out struct {
		Session string `json:"session"`
	}
	if err := conn.Call("session.attach", map[string]any{"token": token}, &out); err != nil {
		_ = conn.Close()
		return nil, err
	}
	return serializeConn(conn), nil
}
