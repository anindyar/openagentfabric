package topology

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"openagentfabric.dev/oaf/internal/config"
	"openagentfabric.dev/oaf/internal/fabric"
	"openagentfabric.dev/oaf/internal/join"
)

const singlePath = "../../examples/cluster.single.yaml"

func load(t *testing.T) ([]byte, *config.Cluster, *config.Catalog) {
	t.Helper()
	data, err := os.ReadFile(singlePath)
	if err != nil {
		t.Fatal(err)
	}
	c, err := config.ParseCluster(data)
	if err != nil {
		t.Fatal(err)
	}
	cat, err := config.LoadCatalog("../../examples/catalog.yaml")
	if err != nil {
		t.Fatal(err)
	}
	return data, c, cat
}

// gb10 is a joined box's hardware: two QSFP ports, each with two rails whose
// MACs end in the given byte.
func gb10(name string, macByte int, used ...int) join.PendingNode {
	h := join.Hardware{Hostname: name, GPU: "NVIDIA GB10", MemTotalGiB: 121.6, DiskFreeGiB: 3500}
	for port := 0; port < 2; port++ {
		p := fabric.Port{Index: port}
		for half := 0; half < 2; half++ {
			nd := fabric.Netdev{Name: []string{"enp1s0f", "enP2p1s0f"}[half] + string(rune('0'+port)) + "np" + string(rune('0'+port)),
				MAC: macOf(macByte, port, half), OperState: "down"}
			for _, u := range used {
				if u == port {
					nd.Addrs = []netip.Prefix{netip.MustParsePrefix("198.51.100.1/24")}
				}
			}
			p.Halves = append(p.Halves, nd)
		}
		h.Ports = append(h.Ports, p)
	}
	return join.PendingNode{Name: name, Address: "192.0.2." + string(rune('0'+macByte)), Hardware: h, EnrolledAt: time.Unix(1792800000, 0)}
}

func macOf(b, port, half int) string {
	return "aa:bb:cc:0" + string(rune('0'+b)) + ":0" + string(rune('0'+port)) + ":0" + string(rune('0'+half))
}

func byID(opts []Option) map[string]Option {
	m := map[string]Option{}
	for _, o := range opts {
		m[o.ID] = o
	}
	return m
}

// The appliance today: one box whose models use ~0.78 of its memory.
// With two joined boxes, option c (a pair next to it) is recommended with
// GLM on the pair; option a fits no pair model next to this box's models;
// HA is described, not planned.
func TestRecommendTwoBoxes(t *testing.T) {
	_, c, cat := load(t)
	in := Input{Cluster: c, Catalog: cat, Pending: []join.PendingNode{gb10("spark-2", 2), gb10("spark-3", 3)}}
	o := byID(Recommend(in))
	cc := o[OptionPairPlusSingle]
	if !cc.Possible || !cc.Recommended || cc.TotalMemoryGiB != 364.8 {
		t.Fatalf("option c: %+v", cc)
	}
	if len(cc.Models) == 0 || cc.Models[0].CatalogRef != "glm-5.3-flash-nvfp4" || cc.Models[0].Where != "pair spark-2+spark-3" {
		t.Fatalf("option c models: %+v", cc.Models)
	}
	if s := cc.Cabling[0]; *s.A.Port != 1 || *s.B.Port != 1 || s.Done {
		t.Fatalf("option c cabling: %+v", s)
	}
	if len(cc.Routing) != 2 || !strings.Contains(cc.Routing[0], "GLM-5.3-Flash") {
		t.Fatalf("option c routing: %v", cc.Routing)
	}
	a := o[OptionPair]
	if !a.Possible || a.Recommended || len(a.Models) != 0 || !strings.Contains(strings.Join(a.Notes, " "), "no pair model fits") {
		t.Fatalf("option a: %+v", a)
	}
	b := o[OptionHA]
	if b.Plannable || !b.Possible {
		t.Fatalf("option b: %+v", b)
	}
}

func TestRecommendOneBox(t *testing.T) {
	_, c, cat := load(t)
	// Room on this box: drop its big model, so a pair model fits next to it.
	c.Spec.Models = c.Spec.Models[1:]
	in := Input{Cluster: c, Catalog: cat, Pending: []join.PendingNode{gb10("spark-2", 2)}}
	o := byID(Recommend(in))
	if o[OptionPairPlusSingle].Possible || !strings.Contains(strings.Join(o[OptionPairPlusSingle].Notes, " "), "needs 2 joined boxes") {
		t.Fatalf("option c with one box: %+v", o[OptionPairPlusSingle])
	}
	a := o[OptionPair]
	// 0.82 is free next to the embedding model: GLM (0.86) does not fit,
	// gpt-oss-120b (0.55) does.
	if !a.Possible || !a.Recommended || a.Models[0].CatalogRef != "gpt-oss-120b" {
		t.Fatalf("option a: %+v", a)
	}
}

// Port 1 already carries an address on the current box: the pair cable goes
// on port 0. A cable both boxes see (rail neighbours) is used as it is.
func TestPortsAndObservedCabling(t *testing.T) {
	_, c, cat := load(t)
	c.Spec.Models = c.Spec.Models[1:]
	cur := gb10("spark-1", 1, 1)
	in := Input{Cluster: c, Catalog: cat, Pending: []join.PendingNode{gb10("spark-2", 2)}, Current: map[string]join.Hardware{"spark-1": cur.Hardware}}
	a := byID(Recommend(in))[OptionPair]
	if s := a.Cabling[0]; *s.A.Port != 0 || *s.B.Port != 1 {
		t.Fatalf("expected spark-1 port 0 ↔ spark-2 port 1: %+v", s)
	}

	// spark-2 port 0 rail 0 sees spark-3 port 1 rail 0, and the other way round.
	p2, p3 := gb10("spark-2", 2), gb10("spark-3", 3)
	p2.Hardware.Neighbors = map[string][]join.Neighbor{p2.Hardware.Ports[0].Halves[0].Name: {{MAC: macOf(3, 1, 0)}}}
	p3.Hardware.Neighbors = map[string][]join.Neighbor{p3.Hardware.Ports[1].Halves[0].Name: {{MAC: macOf(2, 0, 0)}}}
	cables := ObservedCables(map[string]join.Hardware{"spark-2": p2.Hardware, "spark-3": p3.Hardware})
	if len(cables) != 1 || cables[0].A.Node != "spark-2" || *cables[0].A.Port != 0 || *cables[0].B.Port != 1 {
		t.Fatalf("cables %+v", cables)
	}
	in = Input{Cluster: c, Catalog: cat, Pending: []join.PendingNode{p2, p3}, Cabling: cables}
	s := byID(Recommend(in))[OptionPairPlusSingle].Cabling[0]
	if !s.Done || *s.A.Port != 0 || *s.B.Port != 1 || !strings.Contains(s.Text, "cabled") {
		t.Fatalf("observed cable not used: %+v", s)
	}
}

// Plan c on the appliance's cluster.yaml: three nodes, the pair's cable with
// one /30 per rail, a group, GLM on it, a routing rule, comments kept, and
// the result validates.
func TestPlanPairPlusSingle(t *testing.T) {
	data, c, cat := load(t)
	in := Input{Cluster: c, Catalog: cat, Pending: []join.PendingNode{gb10("spark-2", 2), gb10("spark-3", 3)}}
	out, rep, err := Plan(data, in, Choice{Option: OptionPairPlusSingle, Name: "glm-5.3-flash"})
	if err != nil {
		t.Fatalf("plan: %v (%v)", err, rep.Errors())
	}
	if !strings.Contains(string(out), "Memory on one GB10") {
		t.Fatal("comments were lost")
	}
	next, err := config.ParseCluster(out)
	if err != nil {
		t.Fatal(err)
	}
	f := next.Spec.Fabric
	if len(next.Spec.Nodes) != 3 || f.Topology != config.TopologyPairPlusSingle || f.MTU != 9000 || f.RoCE.GIDIndex != "auto" {
		t.Fatalf("nodes/fabric: %+v", next.Spec)
	}
	if l := f.Links[0]; l.A.Node != "spark-2" || l.B.Node != "spark-3" || len(l.Subnets) != 2 || l.Subnets[0] != "10.200.0.0/30" || l.Subnets[1] != "10.200.0.4/30" {
		t.Fatalf("link: %+v", l)
	}
	if next.Spec.Groups[0].Name != "pair-1" {
		t.Fatalf("groups: %+v", next.Spec.Groups)
	}
	d := next.Spec.Models[len(next.Spec.Models)-1]
	if d.Name != "glm-5.3-flash" || d.CatalogRef != "glm-5.3-flash-nvfp4" || d.Placement.Group != "pair-1" || d.Profile != "pair-tp2" {
		t.Fatalf("model: %+v", d)
	}
	r := next.Spec.Routing.Rules[len(next.Spec.Routing.Rules)-1]
	if r.Name != "complex-glm-5-3-flash" || r.Target != "glm-5.3-flash" || r.Match.Signals[0].Type != "reasoning" {
		t.Fatalf("rule: %+v", r)
	}
	if next.Spec.Nodes[1].Roles[0] != config.RoleInference || next.Spec.Nodes[1].Address != gb10("spark-2", 2).Address {
		t.Fatalf("new node: %+v", next.Spec.Nodes[1])
	}
}

func TestPlanRefusals(t *testing.T) {
	data, c, cat := load(t)
	in := Input{Cluster: c, Catalog: cat, Pending: []join.PendingNode{gb10("spark-2", 2)}}
	if _, _, err := Plan(data, in, Choice{Option: OptionHA}); err == nil || !strings.Contains(err.Error(), "cannot be planned yet") {
		t.Fatalf("HA: %v", err)
	}
	if _, _, err := Plan(data, in, Choice{Option: OptionPairPlusSingle}); err == nil || !strings.Contains(err.Error(), "not possible") {
		t.Fatalf("c with one box: %v", err)
	}
	if _, _, err := Plan(data, in, Choice{Option: OptionPair}); err == nil || !strings.Contains(err.Error(), "no catalog model fits") {
		t.Fatalf("a with a full box: %v", err)
	}
	// Boxes and fabric only.
	out, _, err := Plan(data, in, Choice{Option: OptionPair, NoModel: true})
	if err != nil {
		t.Fatal(err)
	}
	next, _ := config.ParseCluster(out)
	if next.Spec.Fabric.Topology != config.TopologyPair || len(next.Spec.Groups) != 1 || len(next.Spec.Models) != 2 {
		t.Fatalf("a, no model: %+v", next.Spec)
	}
	for _, n := range next.Spec.Nodes {
		if n.Headless != nil {
			t.Fatalf("headless set on %s without being asked", n.Name)
		}
	}
	// Headless applies to the new boxes only.
	out, _, err = Plan(data, in, Choice{Option: OptionPair, NoModel: true, Headless: true})
	if err != nil {
		t.Fatal(err)
	}
	next, _ = config.ParseCluster(out)
	for _, n := range next.Spec.Nodes {
		isNew := n.Name != in.Cluster.Spec.Nodes[0].Name
		if isNew != (n.Headless != nil && *n.Headless) {
			t.Fatalf("headless on %s = %v (new box: %v)", n.Name, n.Headless, isNew)
		}
	}
}

func TestAllocSubnetsSkipsUsed(t *testing.T) {
	c := &config.Cluster{}
	c.Spec.Fabric.Links = []config.Link{{Subnets: []string{"10.200.0.0/31", "10.200.0.4/30"}}}
	got, err := allocSubnets(c, 2)
	if err != nil || got[0] != "10.200.0.8/30" || got[1] != "10.200.0.12/30" {
		t.Fatalf("subnets %v %v", got, err)
	}
}

func TestUnifiedDiff(t *testing.T) {
	a := "one\ntwo\nthree\nfour\nfive\nsix\nseven\neight\nnine\n"
	b := "one\ntwo\nthree\nfour\nFIVE\nsix\nseven\neight\nnine\nten\n"
	d := UnifiedDiff("x.yaml", []byte(a), []byte(b))
	for _, want := range []string{"--- a/x.yaml", "+++ b/x.yaml", "-five", "+FIVE", "+ten", "@@ -2,8 +2,9 @@"} {
		if !strings.Contains(d, want) {
			t.Fatalf("diff lacks %q:\n%s", want, d)
		}
	}
	if UnifiedDiff("x", []byte(a), []byte(a)) != "" {
		t.Fatal("no change must be an empty diff")
	}
}

// The wizard's routes: recommend before anything joined (placeholders), then
// plan with the boxes that did.
func TestRoutes(t *testing.T) {
	data, c, cat := load(t)
	dir := t.TempDir()
	pending := &join.PendingStore{Path: filepath.Join(dir, "pending.json")}
	var saved []byte
	d := Deps{
		Load:    func(context.Context) ([]byte, *config.Cluster, *config.Catalog, error) { return data, c, cat, nil },
		Pending: pending,
		SavePlan: func(_ context.Context, _ string, _, after []byte) (string, error) {
			saved = after
			return "plan-1", nil
		},
	}
	mux := http.NewServeMux()
	for p, h := range Routes(d) {
		mux.Handle(p, h)
	}
	srv := httptest.NewServer(mux)
	defer srv.Close()
	post := func(path, body string, out any) int {
		resp, err := http.Post(srv.URL+path, "application/json", bytes.NewBufferString(body))
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		_ = json.NewDecoder(resp.Body).Decode(out)
		return resp.StatusCode
	}

	var rec struct {
		Options []OptionView `json:"options"`
	}
	if code := post("/api/v1/topology/recommend", `{"new_nodes":2,"cabling":"cx7-pair"}`, &rec); code != 200 {
		t.Fatalf("recommend: %d", code)
	}
	ov := map[string]OptionView{}
	for _, o := range rec.Options {
		ov[o.ID] = o
	}
	c3 := ov["c"]
	if !c3.Possible || !c3.Recommended || c3.NodesNeeded != 2 || c3.NewNodes[0] != "new-1" || len(c3.Models) == 0 || c3.Models[0].ID != "glm-5.3-flash-nvfp4" {
		t.Fatalf("option c: %+v", c3)
	}
	kinds := map[string]int{}
	for _, cb := range c3.Cabling {
		kinds[cb.Kind]++
	}
	if kinds["cx7"] != 1 || kinds["lan"] != 2 {
		t.Fatalf("option c cabling: %+v", c3.Cabling)
	}
	if b := ov["b"]; !b.HA || !b.ComingNext || b.Possible {
		t.Fatalf("option b: %+v", b)
	}
	if code := post("/api/v1/topology/recommend", `{"new_nodes":3}`, &rec); code != 400 {
		t.Fatalf("three boxes: %d", code)
	}

	var bad struct {
		Error string `json:"error"`
	}
	if code := post("/api/v1/topology/plan", `{"option":"c","nodes":["spark-2","spark-3"]}`, &bad); code != 400 || !strings.Contains(bad.Error, "has not joined") {
		t.Fatalf("plan with unjoined boxes: %d %+v", code, bad)
	}
	var pr PlanResponse
	for _, n := range []join.PendingNode{gb10("spark-2", 2), gb10("spark-3", 3)} {
		if err := pending.Put(n); err != nil {
			t.Fatal(err)
		}
	}
	pr = PlanResponse{}
	if code := post("/api/v1/topology/plan", `{"option":"c","nodes":["spark-2","spark-3"]}`, &pr); code != 200 {
		t.Fatalf("plan: %d %+v", code, pr)
	}
	if pr.PlanID != "plan-1" || !strings.Contains(pr.Diff, "+  - name: glm-5.3-flash-nvfp4") && !strings.Contains(pr.Diff, "glm-5.3-flash-nvfp4") || len(saved) == 0 || pr.Summary == "" {
		t.Fatalf("plan response: %+v", pr)
	}
	if !strings.Contains(pr.Diff, "-    topology: single") || !strings.Contains(pr.Diff, "+    topology: pair-plus-single") {
		t.Fatalf("diff:\n%s", pr.Diff)
	}
}
