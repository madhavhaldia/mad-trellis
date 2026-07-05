// Command mad-trellis-coopprobe is a tiny diagnostic / e2e client for the
// cooperative-plane exec-stdio transport (#2). Run INSIDE a container, it connects
// to the relay's unix socket (the agent's MAD_SOCKET), optionally attaches a
// capability token to bind to the launcher's session, then verifies it can reach
// the daemon (session.whoami) and participate in coordination (lease.acquire) —
// proving an in-container cooperative adapter reaches the daemon with NO host
// socket path under --network none. It uses the real internal/rpcclient, so it
// also exercises the exact client an adapter would. cgo-free; stdlib + rpcclient.
//
// usage: mad-trellis-coopprobe <socket> [token] [expect-session]
package main

import (
	"encoding/base64"
	"fmt"
	"os"

	"github.com/madhavhaldia/mad-trellis/internal/rpcclient"
)

func main() {
	if len(os.Args) < 2 {
		fail("usage: mad-trellis-coopprobe <socket> [token] [expect-session]")
	}
	sock := os.Args[1]
	var token, expect string
	if len(os.Args) > 2 {
		token = os.Args[2]
	}
	if len(os.Args) > 3 {
		expect = os.Args[3]
	}

	cli, err := rpcclient.Dial(sock)
	if err != nil {
		fail("dial %s: %v", sock, err)
	}
	defer cli.Close()

	if token != "" {
		var att struct {
			Session string `json:"session"`
		}
		if err := cli.Call("session.attach", map[string]any{"token": token}, &att); err != nil {
			fail("session.attach: %v", err)
		}
		if expect != "" && att.Session != expect {
			fail("attach bound to %q, expected %q", att.Session, expect)
		}
		fmt.Printf("attached session=%s\n", att.Session)
	}

	var who struct {
		Session string `json:"session"`
	}
	if err := cli.Call("session.whoami", nil, &who); err != nil {
		fail("session.whoami: %v", err)
	}

	key := base64.StdEncoding.EncodeToString([]byte("mad-trellis:coop:probe"))
	var acq struct {
		Granted bool   `json:"granted"`
		Holder  string `json:"holder"`
	}
	if err := cli.Call("lease.acquire", map[string]any{"key": key, "ttl_ms": 60000}, &acq); err != nil {
		fail("lease.acquire: %v", err)
	}
	if !acq.Granted {
		fail("lease.acquire not granted (holder=%s)", acq.Holder)
	}

	fmt.Printf("COOP-PROBE-OK session=%s holder=%s granted=%v\n", who.Session, acq.Holder, acq.Granted)
}

func fail(format string, a ...any) {
	fmt.Fprintf(os.Stderr, "coopprobe: "+format+"\n", a...)
	os.Exit(1)
}
