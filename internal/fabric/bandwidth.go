package fabric

import (
	"context"
	"errors"
	"fmt"
	"net/netip"
	"strconv"
	"strings"
	"time"

	"openagentfabric.dev/oaf/internal/hostexec"
)

// Bandwidth test limits. The TCP port is ib_write_bw's out-of-band exchange
// port (its default is 18515); concurrent rails each take their own.
const (
	BandwidthPortMin = 18515
	BandwidthPortMax = 18614
	MaxBandwidthQPS  = 16
	MaxBandwidthTime = time.Minute
	bandwidthTool    = "ib_write_bw"
)

// connectRetryEvery paces the client while the server starts; tests shorten it.
var connectRetryEvery = time.Second

// BandwidthTest is one side of an ib_write_bw run on a rail.
type BandwidthTest struct {
	RDMADev  string
	GID      int
	Port     int // TCP port for the exchange
	Duration time.Duration
	QPS      int
	// Server is the peer's rail address for the client side; the zero value
	// runs the server side, which waits for one client.
	Server netip.Addr
	// ConnectWithin bounds how long the client retries while the server is
	// not listening yet.
	ConnectWithin time.Duration
}

// Validate checks the test against the agent's limits.
func (t BandwidthTest) Validate() error {
	switch {
	case t.RDMADev == "" || strings.ContainsAny(t.RDMADev, "/ ") || strings.HasPrefix(t.RDMADev, "-"):
		return fmt.Errorf("bad RDMA device %q", t.RDMADev)
	case t.GID < 0 || t.GID >= maxGIDIndex:
		return fmt.Errorf("GID index %d out of range", t.GID)
	case t.Port < BandwidthPortMin || t.Port > BandwidthPortMax:
		return fmt.Errorf("port %d outside %d–%d", t.Port, BandwidthPortMin, BandwidthPortMax)
	case t.Duration < time.Second || t.Duration > MaxBandwidthTime:
		return fmt.Errorf("duration %s outside 1s–%s", t.Duration, MaxBandwidthTime)
	case t.QPS < 1 || t.QPS > MaxBandwidthQPS:
		return fmt.Errorf("%d queue pairs outside 1–%d", t.QPS, MaxBandwidthQPS)
	case t.Server.IsValid() && !t.Server.Is4():
		return fmt.Errorf("server %s is not IPv4", t.Server)
	}
	return nil
}

// Args is the ib_write_bw command line.
func (t BandwidthTest) Args() []string {
	args := []string{
		"-d", t.RDMADev, "-x", strconv.Itoa(t.GID), "-p", strconv.Itoa(t.Port),
		"-q", strconv.Itoa(t.QPS), "-D", strconv.Itoa(int(t.Duration / time.Second)),
		"-F", "--report_gbits",
	}
	if t.Server.IsValid() {
		args = append(args, t.Server.String())
	}
	return args
}

// RunBandwidth runs one side and returns the average bandwidth it reports.
// The server side returns when its client finishes, or fails when ctx ends.
func RunBandwidth(ctx context.Context, r hostexec.Runner, t BandwidthTest) (float64, error) {
	if err := t.Validate(); err != nil {
		return 0, err
	}
	deadline := time.Now().Add(t.ConnectWithin)
	for {
		out, err := r.Run(ctx, bandwidthTool, t.Args()...)
		if err == nil {
			return ParseBandwidth(out)
		}
		if !t.Server.IsValid() || !notListening(out, err) || time.Now().After(deadline) {
			return 0, fmt.Errorf("%s: %w", bandwidthTool, err)
		}
		select {
		case <-ctx.Done():
			return 0, ctx.Err()
		case <-time.After(connectRetryEvery):
		}
	}
}

// notListening reports the client's failure when the server is not up yet.
func notListening(out []byte, err error) bool {
	text := string(out)
	var ee *hostexec.ExitError
	if errors.As(err, &ee) {
		text += ee.Stderr
	}
	return strings.Contains(text, "Couldn't connect to")
}

// ParseBandwidth reads the average Gb/s from ib_write_bw --report_gbits.
func ParseBandwidth(out []byte) (float64, error) {
	lines := strings.Split(string(out), "\n")
	for i, l := range lines {
		if !strings.Contains(l, "#bytes") {
			continue
		}
		if !strings.Contains(l, "BW average[Gb/sec]") {
			return 0, errors.New("ib_write_bw did not report Gb/s (needs --report_gbits)")
		}
		if i+1 < len(lines) {
			// #bytes #iterations BW-peak BW-average MsgRate
			if f := strings.Fields(lines[i+1]); len(f) >= 4 {
				return strconv.ParseFloat(f[3], 64)
			}
		}
	}
	return 0, errors.New("no bandwidth result in ib_write_bw output")
}
