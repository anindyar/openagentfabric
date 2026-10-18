package fabric

import (
	"context"
	"net/netip"
	"os"
	"strings"
	"testing"
	"time"

	"openagentfabric.dev/oaf/internal/hostexec"
)

func TestParseBandwidth(t *testing.T) {
	out, err := os.ReadFile("testdata/ib_write_bw_client.txt") // DGX Spark ↔ DGX Spark, one rail
	if err != nil {
		t.Fatal(err)
	}
	got, err := ParseBandwidth(out)
	if err != nil || got != 111.61 {
		t.Fatalf("got %v, %v; want 111.61", got, err)
	}
	if _, err := ParseBandwidth([]byte(" #bytes #iterations BW peak[MB/sec] BW average[MB/sec]\n 65536 1 0 1000 0\n")); err == nil || !strings.Contains(err.Error(), "report_gbits") {
		t.Fatalf("MB/s output must be refused: %v", err)
	}
	if _, err := ParseBandwidth([]byte("Couldn't connect\n")); err == nil {
		t.Fatal("output without a result must fail")
	}
}

func validTest() BandwidthTest {
	return BandwidthTest{RDMADev: "rocep1s0f1", GID: 3, Port: BandwidthPortMin, Duration: 5 * time.Second, QPS: 4}
}

func TestBandwidthArgsAndLimits(t *testing.T) {
	c := validTest()
	c.Server = netip.MustParseAddr("192.168.31.1")
	if got := strings.Join(c.Args(), " "); got != "-d rocep1s0f1 -x 3 -p 18515 -q 4 -D 5 -F --report_gbits 192.168.31.1" {
		t.Fatalf("client args: %s", got)
	}
	if got := strings.Join(validTest().Args(), " "); strings.HasSuffix(got, "192.168.31.1") {
		t.Fatalf("server args must not name a peer: %s", got)
	}
	for name, mut := range map[string]func(*BandwidthTest){
		"option as device": func(b *BandwidthTest) { b.RDMADev = "--help" },
		"path as device":   func(b *BandwidthTest) { b.RDMADev = "../x" },
		"low port":         func(b *BandwidthTest) { b.Port = 22 },
		"long run":         func(b *BandwidthTest) { b.Duration = time.Hour },
		"no QPS":           func(b *BandwidthTest) { b.QPS = 0 },
		"bad GID":          func(b *BandwidthTest) { b.GID = -1 },
		"IPv6 server":      func(b *BandwidthTest) { b.Server = netip.MustParseAddr("fe80::1") },
	} {
		b := validTest()
		mut(&b)
		if b.Validate() == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}

// flaky fails like an ib_write_bw client whose server is not up yet.
type flaky struct {
	fails int
	calls int
	out   []byte
}

func (f *flaky) Run(_ context.Context, name string, _ ...string) ([]byte, error) {
	f.calls++
	if f.calls <= f.fails {
		return []byte("Couldn't connect to 192.168.31.1:18515\n"), &hostexec.ExitError{Cmd: name, Code: 1}
	}
	return f.out, nil
}

func TestRunBandwidthRetriesUntilServerListens(t *testing.T) {
	connectRetryEvery = time.Millisecond
	out, _ := os.ReadFile("testdata/ib_write_bw_client.txt")
	c := validTest()
	c.Server = netip.MustParseAddr("192.168.31.1")
	c.ConnectWithin = time.Minute
	r := &flaky{fails: 2, out: out}
	if got, err := RunBandwidth(context.Background(), r, c); err != nil || got != 111.61 || r.calls != 3 {
		t.Fatalf("got %v, %v after %d calls", got, err, r.calls)
	}

	c.ConnectWithin = 0
	if _, err := RunBandwidth(context.Background(), &flaky{fails: 5, out: out}, c); err == nil {
		t.Fatal("the client must give up after ConnectWithin")
	}
	srv := &flaky{fails: 1, out: out}
	if _, err := RunBandwidth(context.Background(), srv, validTest()); err == nil || srv.calls != 1 {
		t.Fatalf("the server side must not retry: %v, %d calls", err, srv.calls)
	}
}

func port(index int, addrs ...[]string) Port {
	p := Port{Index: index}
	for h, a := range addrs {
		nd := Netdev{Name: []string{"enp1s0f1np1", "enP2p1s0f1np1"}[h], OperState: "up", MTU: 9000, RDMADev: []string{"rocep1s0f1", "roceP2p1s0f1"}[h]}
		for i, s := range a {
			pfx := netip.MustParsePrefix(s)
			nd.Addrs = append(nd.Addrs, pfx)
			v4 := pfx.Addr().As4()
			nd.GIDs = append(nd.GIDs, GID{Index: 3 + 2*i, Type: "RoCE v2", GID: "0000:0000:0000:0000:0000:ffff:" + hex2(v4[0], v4[1]) + ":" + hex2(v4[2], v4[3])})
		}
		p.Halves = append(p.Halves, nd)
	}
	return p
}

func hex2(a, b byte) string {
	const d = "0123456789abcdef"
	return string([]byte{d[a>>4], d[a&15], d[b>>4], d[b&15]})
}

func TestResolveEnd(t *testing.T) {
	ports := []Port{port(1, []string{"192.168.31.2/30"}, []string{"10.0.0.2/24", "192.168.41.2/30"})}
	e, err := ResolveEnd("n", ports, 1, []string{"", "192.168.41.0/30"}, "")
	if err != nil {
		t.Fatal(err)
	}
	if e.Rails[0].Addr.String() != "192.168.31.2/30" || e.Rails[0].GID != 3 {
		t.Errorf("rail 0: %+v", e.Rails[0])
	}
	// The second address of the netdev carries the second RoCE v2 GID.
	if e.Rails[1].Addr.String() != "192.168.41.2/30" || e.Rails[1].GID != 5 {
		t.Errorf("rail 1 must follow the subnet and its GID: %+v", e.Rails[1])
	}
	if e, _ := ResolveEnd("n", ports, 1, nil, "7"); e.Rails[1].GID != 7 {
		t.Errorf("a fixed gidIndex is used as is: %+v", e.Rails[1])
	}

	down := []Port{port(1, []string{"192.168.31.2/30"}, []string{"192.168.41.2/30"})}
	down[0].Halves[1].OperState = "down"
	for name, tc := range map[string]struct {
		ports   []Port
		port    int
		subnets []string
		want    string
	}{
		"missing port":   {ports, 0, nil, "no QSFP port 0"},
		"no carrier":     {down, 1, nil, "no carrier"},
		"subnet missing": {ports, 1, []string{"172.16.0.0/30"}, "no IPv4 address in 172.16.0.0/30"},
		"bad subnet":     {ports, 1, []string{"x"}, "fabric subnet"},
	} {
		if _, err := ResolveEnd("n", tc.ports, tc.port, tc.subnets, ""); err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: %v, want %q", name, err, tc.want)
		}
	}
}
