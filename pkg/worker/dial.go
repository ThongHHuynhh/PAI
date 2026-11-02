package worker

import (
	"errors"
	"log"
	"net"
	"net/netip"
	"os"
	"time"

	"pai/pkg/mesh"
)

// Redial waits grow from a blink, so a harness restart costs no visible time,
// to five seconds: a harness is restarted often while it is developed, and a
// worker that takes a minute to come back reads as a failure. A dial a second
// against a dead harness costs nothing. A dial waits longer than the longest
// wait: a first connect over a relayed path can take seconds.
// A keepalive probe every 5s idle meets a restarted harness's reset within
// seconds; a harness that is gone fails 4 probes, so its connection ends in
// about 25s on every OS, not after the OS's own count.
// A mesh can wedge so no packet comes back from the harness while it answers
// as coordinator, as one started before the network did (2026-11-01): only a
// new node mends it. With the coordinator answering, 2 minutes without a
// connection is that; with it silent too, the harness may just be down, and a
// restart every 10 minutes is cheap enough to cover a map poll that wedged.
const (
	redialMin    = 200 * time.Millisecond
	redialMax    = 5 * time.Second
	dialTimeout  = 10 * time.Second
	keepAlive    = 5 * time.Second
	keepAliveCnt = 4
	stuckPolling = 2 * time.Minute
	stuckSilent  = 10 * time.Minute
)

// Default is the interpreters of each OS: the shell it ships with, the
// default, then js, which every worker has.
var Default = map[string][]string{"windows": {"pwsh", "js"}, "linux": {"bash", "js"}, "darwin": {"bash", "js"}}

// Dial makes this machine a worker of the harness on m: dial the harness's
// node at the address the last map gives it, serve until the connection
// ends, dial again, until the harness removes this worker. WireGuard proved
// both ends, so the hello carries no secret. A connection that ends within
// the longest wait, as a refused hello does, keeps the backoff; only one that
// held resets it. A worker that cannot connect for long restarts, which
// makes its mesh node anew. The js folder is readied beside it.
func Dial(m *mesh.Mesh, l Local) {
	go startJS(l)
	go dial(m, l)
}

func dial(m *mesh.Mesh, l Local) {
	wait, connected := redialMin, time.Now()
	d := net.Dialer{Timeout: dialTimeout, KeepAliveConfig: net.KeepAliveConfig{Enable: true, Idle: keepAlive, Interval: keepAlive, Count: keepAliveCnt}}
	for {
		err := errors.New("no harness in the mesh map yet")
		if a, ok := m.Peer(Harness); ok {
			var conn net.Conn
			at := netip.AddrPortFrom(a, Port).String()
			if conn, err = d.Dial("tcp", at); err == nil {
				log.Printf("connected to the harness at %s as %s", at, l.Name)
				start, done := time.Now(), make(chan struct{})
				go hangUpOnMove(m, conn, done)
				err = Serve(conn, l)
				close(done)
				_ = conn.Close()
				connected = time.Now()
				if time.Since(start) > redialMax {
					wait = redialMin
				}
			}
			// The connection is lost, maybe with the route or rule to the
			// harness: systemd-networkd removes rules it did not make. With
			// them gone a dial fails as unreachable, or times out by a
			// default route.
			if err := m.Reroute(); err != nil {
				log.Printf("mesh: reroute: %v", err)
			}
		}
		if off := time.Since(connected); stuck(off, m.Polling()) {
			exe, err := os.Executable()
			if err != nil {
				log.Fatalf("no connection to the harness in %s, and no restart: %v", off.Round(time.Second), err)
			}
			log.Printf("no connection to the harness in %s: restarting for a new mesh node", off.Round(time.Second))
			restart(exe)
		}
		log.Printf("%v, redial in %s", err, wait)
		select {
		case <-m.Removed():
			log.Print("removed from the mesh: no redial")
			return
		case <-time.After(wait):
		}
		wait = min(2*wait, redialMax)
	}
}

// stuck is whether a worker off the harness for off, with the map poll up or
// not, should make its mesh node anew.
func stuck(off time.Duration, polling bool) bool {
	return off > stuckSilent || polling && off > stuckPolling
}

// hangUpOnMove closes conn once this node's address is no longer the one
// conn is from, until done. A harness that lost this node registers it again
// at a new address, and Linux and macOS keep a connection from the old one
// open for minutes: its keepalives go nowhere and no reset comes back.
func hangUpOnMove(m *mesh.Mesh, conn net.Conn, done <-chan struct{}) {
	from := conn.LocalAddr().(*net.TCPAddr).AddrPort().Addr()
	for {
		select {
		case <-done:
			return
		case <-time.After(keepAlive):
		}
		if a := m.Self(); a.IsValid() && a != from {
			log.Printf("this node moved from %s to %s: hanging up", from, a)
			_ = conn.Close()
			return
		}
	}
}
