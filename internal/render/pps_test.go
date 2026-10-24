package render

import (
	"strings"
	"testing"

	"go.yaml.in/yaml/v3"

	"openagentfabric.dev/oaf/internal/config"
	"openagentfabric.dev/oaf/internal/fabric"
)

// pair-plus-single renders: the large model runs on the pair (rank 0 and a
// headless rank over the pair's cable, RoCE), the small one on the single
// box, and nothing of the pair model lands on the single box.
func TestRenderPairPlusSingle(t *testing.T) {
	in := pairInput(t)
	c, err := config.LoadCluster("../../examples/cluster.pair-plus-single.yaml")
	if err != nil {
		t.Fatal(err)
	}
	in.Cluster = c
	in.Fabric = map[string][]fabric.Port{
		"spark-1": {},
		"spark-2": {rails(1, "10.200.0.1/30", "10.200.0.5/30", 3)},
		"spark-3": {rails(1, "10.200.0.2/30", "10.200.0.6/30", 3)},
	}
	byNode := map[string][]string{}
	for _, p := range mustRender(t, in) {
		byNode[p.Node] = append(byNode[p.Node], p.Name)
	}
	has := func(node, project string) bool {
		for _, n := range byNode[node] {
			if n == project {
				return true
			}
		}
		return false
	}
	glm := modelProjectName("glm-5.3-flash")
	if !has("spark-2", glm) || !has("spark-3", glm) || has("spark-1", glm) || !has("spark-1", modelProjectName("fast")) {
		t.Fatalf("projects by node: %v", byNode)
	}
	var cf composeFile
	if err := yaml.Unmarshal(fileOf(t, in, "spark-3", glm, "compose.yaml"), &cf); err != nil {
		t.Fatal(err)
	}
	s := cf.Services["vllm"]
	cmd := strings.Join(s.Command, " ")
	for _, want := range []string{"--headless", "--node-rank 1", "--master-addr 10.200.0.1", "--language-model-only", "--tensor-parallel-size 2"} {
		if !strings.Contains(cmd, want) {
			t.Fatalf("rank 1 command lacks %q: %s", want, cmd)
		}
	}
	if s.Environment["NCCL_IB_DISABLE"] != "0" || s.Environment["NCCL_IB_GID_INDEX"] != "3" || s.Environment["NCCL_CUMEM_ENABLE"] != "0" || s.Environment["VLLM_HOST_IP"] != "10.200.0.2" {
		t.Fatalf("rank 1 env: %v", s.Environment)
	}
}
