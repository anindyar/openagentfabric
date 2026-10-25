package render

import "fmt"

// rankGuard runs a follower rank of a model that spans boxes (vllm serve
// --headless) and watches its TCP connection to the leader's rendezvous
// store. When the leader restarts, a follower's vLLM does not exit: it keeps
// its dead store connection ("Broken pipe") and never joins the leader's new
// attempt, which then times out waiting for it ("1/2 clients joined") and
// restarts again, forever. Once the follower has been connected, losing the
// connection for 45 seconds stops it, so docker restarts it into the
// leader's next attempt.
//
// It is python3 -c <script> <vllm command...>: it runs the command, forwards
// SIGTERM/SIGINT and exits with the command's status.
const rankGuardScript = `import os, signal, socket, struct, subprocess, sys, time
host, port = sys.argv[1], int(sys.argv[2])
cmd = sys.argv[3:]
want4 = "%%08X:%%04X" %% (struct.unpack("<I", socket.inet_aton(host))[0], port)
def connected():
    for f in ("/proc/net/tcp", "/proc/net/tcp6"):
        try:
            for line in open(f).readlines()[1:]:
                p = line.split()
                if p[3] == "01" and p[2].endswith(want4):  # tcp6 holds IPv4 as ::ffff:a.b.c.d
                    return True
        except OSError:
            pass
    return False
child = subprocess.Popen(cmd)
for s in (signal.SIGTERM, signal.SIGINT):
    signal.signal(s, lambda n, f: child.send_signal(n))
seen, lost = False, None
while child.poll() is None:
    time.sleep(5)
    if connected():
        seen, lost = True, None
    elif seen:
        lost = lost or time.time()
        if time.time() - lost > 45:
            print("rank guard: lost the leader at %%s:%%d; stopping so docker restarts this rank" %% (host, port), flush=True)
            child.terminate()
            try:
                child.wait(30)
            except subprocess.TimeoutExpired:
                child.kill()
            sys.exit(1)
sys.exit(child.returncode)
`

// guardFollower wraps a follower rank's command in rankGuardScript. With a
// command prefix (an image whose entrypoint is a wrapper, like NVIDIA's) the
// guard goes in the command and the entrypoint stays; otherwise the image's
// entrypoint is vllm serve and the guard replaces it.
func guardFollower(s *service, prefix []string, master string, port int) {
	guard := []string{"python3", "-c", fmt.Sprintf(rankGuardScript), master, fmt.Sprint(port)}
	if len(prefix) > 0 {
		s.Command = append(guard, s.Command...)
		return
	}
	s.Entrypoint = guard
	s.Command = append([]string{"vllm", "serve"}, s.Command...)
}
