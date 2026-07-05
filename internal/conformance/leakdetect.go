package conformance

import (
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"net/url"
	"strings"
)

// leakdetect.go is the ENCODED-ENDPOINT LEAK DETECTOR for the singular gate
// (fixes #4/#8). The prior proxy/deny probes TRUSTED the gate's self-reported
// real_reachable=false and grep'd only the PLAINTEXT env value — so an ENCODED
// (base64/hex/url) routable endpoint would slip through. This detector does NOT
// trust real_reachable: it enumerates EVERY env value the gate produced and scans
// each in RAW + base64/hex/url-DECODED forms for any routable scheme or a
// non-sentinel host:port, asserting ONLY the non-routable sentinel
// (mad-trellis-proxy:// / mad-trellis-deny:// / mad-trellis-mock:// / mad-trellis-denied://)
// appears.
//
// It is a PURE function over an env map (no daemon), so a Control can feed it a
// SYNTHETIC env containing an encoded endpoint and assert it flips RED — proving
// the detector is non-vacuous WITHOUT modifying production gate.go.

// routableSchemes are network schemes that, if present (raw or decoded) in an
// env value, mean the env-spec carries a REACHABLE real endpoint.
var routableSchemes = []string{
	"http://", "https://", "postgres://", "postgresql://", "mysql://",
	"redis://", "mongodb://", "amqp://", "grpc://", "tcp://", "ws://", "wss://",
	"ftp://", "ssh://", "ldap://", "smtp://",
}

// nonRoutableSentinelPrefixes are the ONLY routing values a deny/mock/proxy/denied
// env-spec may carry — non-routable by construction (a tool honoring the env fails
// closed). Anything else with a scheme/host:port is a leak.
var nonRoutableSentinelPrefixes = []string{
	"mad-trellis-proxy://", "mad-trellis-mock://", "mad-trellis-deny://", "mad-trellis-denied://",
}

// endpointLeak describes a detected routable endpoint leak (the env key, the form
// it was found in, and the offending decoded value) for a precise failure detail.
type endpointLeak struct {
	EnvKey string
	Form   string // "raw" | "base64" | "hex" | "url"
	Value  string
}

func (l endpointLeak) String() string {
	return fmt.Sprintf("%s leaks a routable endpoint via %s decode: %q", l.EnvKey, l.Form, l.Value)
}

// detectEndpointLeak scans an env map for a routable endpoint in any value's raw
// OR base64/hex/url-decoded form. allowedSentinel is the sentinel prefix this
// env-spec is permitted to carry (e.g. "mad-trellis-proxy://"); a value EQUAL to a
// permitted sentinel is fine. It returns (leak, true) on the FIRST leak found.
//
// CRITICAL: it does NOT consult any real_reachable flag — the env bytes are the
// authority. This is what catches an encoded endpoint a self-reported
// real_reachable=false would have hidden.
func detectEndpointLeak(env map[string]string, allowedSentinel string) (endpointLeak, bool) {
	for k, v := range env {
		// A value that IS exactly a permitted non-routable sentinel is fine; skip the
		// sentinel's own scheme so "mad-trellis-proxy://" is not flagged as routable.
		forms := decodeForms(v)
		for form, decoded := range forms {
			// Strip the permitted sentinel so its presence never trips the scan, but
			// any ADDITIONAL routable content after/around it still does.
			scan := decoded
			if allowedSentinel != "" {
				scan = strings.ReplaceAll(scan, allowedSentinel, "")
			}
			// Also strip the generic mad-trellis-* sentinels (they are non-routable).
			for _, sp := range nonRoutableSentinelPrefixes {
				scan = strings.ReplaceAll(scan, sp, "")
			}
			if endpoint := firstRoutable(scan); endpoint != "" {
				return endpointLeak{EnvKey: k, Form: form, Value: endpoint}, true
			}
		}
	}
	return endpointLeak{}, false
}

// firstRoutable returns the first routable indicator (a known scheme, or a bare
// host:port that is not an obvious sentinel) found in s, or "" if none.
func firstRoutable(s string) string {
	low := strings.ToLower(s)
	for _, sch := range routableSchemes {
		if i := strings.Index(low, sch); i >= 0 {
			// Return a short slice around the scheme for the failure detail.
			end := i + len(sch)
			for end < len(s) && !isSep(s[end]) {
				end++
			}
			return s[i:end]
		}
	}
	// A bare host:port (e.g. "db.internal:5432") with a DNS-ish host is routable.
	if hp := findHostPort(s); hp != "" {
		return hp
	}
	return ""
}

// findHostPort finds a bare host:port token (a dotted/host name followed by :port)
// that is NOT part of a mad-trellis sentinel (those were stripped already).
func findHostPort(s string) string {
	for _, tok := range strings.FieldsFunc(s, func(r rune) bool {
		return r == ' ' || r == '\n' || r == '\t' || r == '"' || r == ',' || r == '\''
	}) {
		// Need a colon with digits after it and a dot or a non-trivial host before it.
		ci := strings.LastIndexByte(tok, ':')
		if ci <= 0 || ci == len(tok)-1 {
			continue
		}
		host, port := tok[:ci], tok[ci+1:]
		if !allDigits(port) {
			continue
		}
		// Host must look like a real host: contain a dot (FQDN/ip) or be a known name.
		if strings.Contains(host, ".") && !strings.HasPrefix(host, "mad-trellis-") {
			return tok
		}
	}
	return ""
}

// decodeForms returns the candidate decodings of a value: raw, plus base64/hex/url
// decodings when they yield printable text. The key is the form name.
func decodeForms(v string) map[string]string {
	forms := map[string]string{"raw": v}
	if b, err := base64.StdEncoding.DecodeString(strings.TrimSpace(v)); err == nil && isPrintable(b) {
		forms["base64"] = string(b)
	}
	if b, err := hex.DecodeString(strings.TrimSpace(v)); err == nil && len(b) > 0 && isPrintable(b) {
		forms["hex"] = string(b)
	}
	if u, err := url.QueryUnescape(v); err == nil && u != v {
		forms["url"] = u
	}
	return forms
}

func isPrintable(b []byte) bool {
	if len(b) == 0 {
		return false
	}
	for _, c := range b {
		if c < 0x20 && c != '\n' && c != '\t' && c != '\r' {
			return false
		}
		if c > 0x7e {
			return false
		}
	}
	return true
}

func allDigits(s string) bool {
	if s == "" {
		return false
	}
	for _, r := range s {
		if r < '0' || r > '9' {
			return false
		}
	}
	return true
}

func isSep(b byte) bool {
	switch b {
	case ' ', '\n', '\t', '"', '\'', ',', ')', ']', '}':
		return true
	}
	return false
}

// assertNonRoutableEnv asserts an env map carries ONLY non-routable sentinels: the
// allowedSentinel prefix MUST appear in at least one value, and NO value (raw or
// decoded) may leak a routable endpoint. Returns a non-empty reason on violation.
func assertNonRoutableEnv(env map[string]string, allowedSentinel string) string {
	if len(env) == 0 {
		return "the gate produced an EMPTY env-spec — no sentinel to verify (vacuous)"
	}
	sawSentinel := false
	for _, v := range env {
		if strings.Contains(v, allowedSentinel) {
			sawSentinel = true
		}
	}
	if !sawSentinel {
		return fmt.Sprintf("the non-routable sentinel %q is absent from the env-spec %v", allowedSentinel, env)
	}
	if leak, found := detectEndpointLeak(env, allowedSentinel); found {
		return "ROUTABLE ENDPOINT LEAK: " + leak.String()
	}
	return ""
}
