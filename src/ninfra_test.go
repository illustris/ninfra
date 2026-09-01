package main

import (
	"regexp"
	"strings"
	"testing"
)

func mkConfigs(hosts map[string]DeploymentInfo) Configurations {
	configs := make(Configurations, len(hosts))
	for host, d := range hosts {
		configs[host] = HostConfig{Deployment: d}
	}
	return configs
}

// batchHosts flattens a plan into "wave/domain/batch: host1,host2" lines for
// easy shape comparison.
func planString(waves [][][]hostBatch) string {
	var lines []string
	for wi, wave := range waves {
		for di, domain := range wave {
			for bi, batch := range domain {
				lines = append(lines, strings.Join([]string{
					"w" + itoa(wi), "d" + itoa(di), "b" + itoa(bi),
					strings.Join(batch.hosts, ","),
				}, "/"))
			}
		}
	}
	return strings.Join(lines, "\n")
}

func itoa(i int) string {
	if i == 0 {
		return "0"
	}
	var digits []byte
	for i > 0 {
		digits = append([]byte{byte('0' + i%10)}, digits...)
		i /= 10
	}
	return string(digits)
}

func TestPlanApplyWavesDegenerate(t *testing.T) {
	// No group/domain anywhere: one wave, one domain, one batch, all hosts
	// sorted - today's behavior through the new code path.
	configs := mkConfigs(map[string]DeploymentInfo{
		"c": {TargetHost: "c"},
		"a": {TargetHost: "a"},
		"b": {TargetHost: "b"},
	})
	waves := planApplyWaves(configs, 1)
	if len(waves) != 1 || len(waves[0]) != 1 || len(waves[0][0]) != 1 {
		t.Fatalf("expected 1 wave/1 domain/1 batch, got %s", planString(waves))
	}
	got := strings.Join(waves[0][0][0].hosts, ",")
	if got != "a,b,c" {
		t.Fatalf("expected hosts a,b,c, got %s", got)
	}
}

func TestPlanApplyWavesGroupChunking(t *testing.T) {
	// One group of 5 hosts with k=2: batches 2,2,1; ungrouped hosts first.
	configs := mkConfigs(map[string]DeploymentInfo{
		"g1": {TargetHost: "g1", Group: "web", MaxConcurrentNodes: 2},
		"g2": {TargetHost: "g2", Group: "web", MaxConcurrentNodes: 2},
		"g3": {TargetHost: "g3", Group: "web", MaxConcurrentNodes: 2},
		"g4": {TargetHost: "g4", Group: "web", MaxConcurrentNodes: 2},
		"g5": {TargetHost: "g5", Group: "web", MaxConcurrentNodes: 2},
		"u1": {TargetHost: "u1"},
		"u2": {TargetHost: "u2"},
	})
	waves := planApplyWaves(configs, 1)
	if len(waves) != 1 || len(waves[0]) != 1 {
		t.Fatalf("expected 1 wave/1 domain, got %s", planString(waves))
	}
	batches := waves[0][0]
	if len(batches) != 4 {
		t.Fatalf("expected 4 batches (ungrouped + 2,2,1), got %d: %s", len(batches), planString(waves))
	}
	if strings.Join(batches[0].hosts, ",") != "u1,u2" {
		t.Fatalf("expected ungrouped batch u1,u2 first, got %s", planString(waves))
	}
	sizes := []string{}
	for _, b := range batches[1:] {
		sizes = append(sizes, itoa(len(b.hosts)))
	}
	if strings.Join(sizes, ",") != "2,2,1" {
		t.Fatalf("expected group batches 2,2,1, got %s", planString(waves))
	}
	for _, b := range batches {
		if b.group != "" && b.group != "web" {
			t.Fatalf("unexpected group %q", b.group)
		}
	}
}

func TestPlanApplyWavesFailureDomains(t *testing.T) {
	// 3 domains with j=2: waves [[d1,d2],[d3]]; hosts without a domain land
	// in the "" domain, which sorts first.
	configs := mkConfigs(map[string]DeploymentInfo{
		"h1": {TargetHost: "h1", FailureDomain: "dc1"},
		"h2": {TargetHost: "h2", FailureDomain: "dc2"},
		"h3": {TargetHost: "h3", FailureDomain: "dc3"},
		"h4": {TargetHost: "h4"},
	})
	waves := planApplyWaves(configs, 2)
	if len(waves) != 2 {
		t.Fatalf("expected 2 waves, got %d: %s", len(waves), planString(waves))
	}
	if len(waves[0]) != 2 || len(waves[1]) != 2 {
		t.Fatalf("expected wave sizes 2 and 2, got %s", planString(waves))
	}
	// First wave: "" domain (h4) and dc1 (h1).
	if waves[0][0][0].domain != "" || strings.Join(waves[0][0][0].hosts, ",") != "h4" {
		t.Fatalf("expected first domain \"\" with h4, got %s", planString(waves))
	}
	if waves[0][1][0].domain != "dc1" || strings.Join(waves[0][1][0].hosts, ",") != "h1" {
		t.Fatalf("expected second domain dc1 with h1, got %s", planString(waves))
	}
	if waves[1][0][0].domain != "dc2" || strings.Join(waves[1][0][0].hosts, ",") != "h2" {
		t.Fatalf("expected second wave to start with dc2, got %s", planString(waves))
	}
	if waves[1][1][0].domain != "dc3" || strings.Join(waves[1][1][0].hosts, ",") != "h3" {
		t.Fatalf("expected second wave to contain dc3, got %s", planString(waves))
	}
}

func TestPlanApplyWavesGroupPerDomain(t *testing.T) {
	// Same group name in two domains: k is enforced per (domain, group) pair.
	configs := mkConfigs(map[string]DeploymentInfo{
		"a1": {TargetHost: "a1", Group: "web", FailureDomain: "dc1", MaxConcurrentNodes: 1},
		"a2": {TargetHost: "a2", Group: "web", FailureDomain: "dc2", MaxConcurrentNodes: 1},
	})
	waves := planApplyWaves(configs, 2)
	if len(waves) != 1 || len(waves[0]) != 2 {
		t.Fatalf("expected 1 wave with 2 domains, got %s", planString(waves))
	}
	for di, domain := range waves[0] {
		if len(domain) != 1 || len(domain[0].hosts) != 1 {
			t.Fatalf("expected 1 batch of 1 per domain, got %s", planString(waves))
		}
		if domain[0].hosts[0] != []string{"a1", "a2"}[di] {
			t.Fatalf("unexpected host in domain %d: %s", di, planString(waves))
		}
	}
}

func TestValidateDeploymentCheckTypes(t *testing.T) {
	tests := []struct {
		name    string
		check   HealthCheck
		wantErr string
	}{
		{"tcp ok", HealthCheck{Type: "tcp", Port: 9100}, ""},
		{"tcp port 0", HealthCheck{Type: "tcp", Port: 0}, "invalid port"},
		{"tcp port too big", HealthCheck{Type: "tcp", Port: 65536}, "invalid port"},
		{"http ok", HealthCheck{Type: "http", URL: "http://h:9100/metrics"}, ""},
		{"http no url", HealthCheck{Type: "http"}, "url must not be empty"},
		{"http bad scheme", HealthCheck{Type: "http", URL: "ftp://h/m"}, "scheme must be http or https"},
		{"http unparseable", HealthCheck{Type: "http", URL: "http://[::1"}, "invalid url"},
		{"exec ok", HealthCheck{Type: "exec", Command: "true"}, ""},
		{"exec no command", HealthCheck{Type: "exec"}, "command must not be empty"},
		{"output ok", HealthCheck{Type: "output", Command: "x", Match: "^ready"}, ""},
		{"output bad regex", HealthCheck{Type: "output", Command: "x", Match: "["}, "invalid match regex"},
		{"unknown type", HealthCheck{Type: "gossip"}, "unknown type"},
	}
	for _, tt := range tests {
		configs := mkConfigs(map[string]DeploymentInfo{
			"h": {TargetHost: "h", HealthChecks: map[string]HealthCheck{"mycheck": tt.check}},
		})
		err := validateDeployment(configs)
		if tt.wantErr == "" {
			if err != nil {
				t.Errorf("%s: unexpected error: %v", tt.name, err)
			}
			continue
		}
		if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
			t.Errorf("%s: expected error containing %q, got %v", tt.name, tt.wantErr, err)
		}
		if err != nil && !strings.Contains(err.Error(), "host h") || !strings.Contains(err.Error(), "mycheck") {
			t.Errorf("%s: error must name host and check: %v", tt.name, err)
		}
	}
}

func TestValidateDeploymentKnobConflicts(t *testing.T) {
	// Conflicting k within a group.
	configs := mkConfigs(map[string]DeploymentInfo{
		"a": {TargetHost: "a", Group: "web", MaxConcurrentNodes: 2},
		"b": {TargetHost: "b", Group: "web", MaxConcurrentNodes: 3},
	})
	err := validateDeployment(configs)
	if err == nil || !strings.Contains(err.Error(), "conflicting maxConcurrentNodes within group web") {
		t.Fatalf("expected group conflict error, got %v", err)
	}
	if !strings.Contains(err.Error(), "hosts: a") || !strings.Contains(err.Error(), "b") {
		t.Fatalf("expected error to name hosts, got %v", err)
	}

	// Unset (0) alongside a set value is NOT a conflict.
	configs = mkConfigs(map[string]DeploymentInfo{
		"a": {TargetHost: "a", Group: "web", MaxConcurrentNodes: 2},
		"b": {TargetHost: "b", Group: "web"},
	})
	if err := validateDeployment(configs); err != nil {
		t.Fatalf("unset k must not conflict: %v", err)
	}
	if k := effectiveMaxConcurrentNodes(configs, "web"); k != 2 {
		t.Fatalf("expected effective k=2, got %d", k)
	}

	// Conflicting j across hosts.
	configs = mkConfigs(map[string]DeploymentInfo{
		"a": {TargetHost: "a", MaxConcurrentDomains: 2},
		"b": {TargetHost: "b", MaxConcurrentDomains: 3},
	})
	err = validateDeployment(configs)
	if err == nil || !strings.Contains(err.Error(), "conflicting maxConcurrentDomains") {
		t.Fatalf("expected domain conflict error, got %v", err)
	}

	// Unset j alongside a set value is fine and effective.
	configs = mkConfigs(map[string]DeploymentInfo{
		"a": {TargetHost: "a", MaxConcurrentDomains: 2},
		"b": {TargetHost: "b"},
	})
	if err := validateDeployment(configs); err != nil {
		t.Fatalf("unset j must not conflict: %v", err)
	}
	if j := effectiveMaxConcurrentDomains(configs); j != 2 {
		t.Fatalf("expected effective j=2, got %d", j)
	}
}

func TestSelectedChecks(t *testing.T) {
	cfg := HostConfig{Deployment: DeploymentInfo{HealthChecks: map[string]HealthCheck{
		"zeta":  {Type: "tcp", Port: 1},
		"alpha": {Type: "tcp", Port: 2},
		"mike":  {Type: "tcp", Port: 3},
	}}}

	// Unfiltered: sorted by name.
	all := selectedChecks(cfg, nil, nil)
	if len(all) != 3 || all[0].name != "alpha" || all[1].name != "mike" || all[2].name != "zeta" {
		t.Fatalf("expected sorted alpha,mike,zeta, got %v", all)
	}

	// Include filter narrows by name (anchored semantics: MatchString on
	// pre-anchored pattern).
	include := regexp.MustCompile("^(?:alpha|zeta)$")
	got := selectedChecks(cfg, include, nil)
	if len(got) != 2 || got[0].name != "alpha" || got[1].name != "zeta" {
		t.Fatalf("expected alpha,zeta, got %v", got)
	}

	// Ignore filter subtracts.
	ignore := regexp.MustCompile("^(?:mike)$")
	got = selectedChecks(cfg, nil, ignore)
	if len(got) != 2 || got[0].name != "alpha" || got[1].name != "zeta" {
		t.Fatalf("expected mike subtracted, got %v", got)
	}

	// Both together.
	got = selectedChecks(cfg, include, regexp.MustCompile("^(?:zeta)$"))
	if len(got) != 1 || got[0].name != "alpha" {
		t.Fatalf("expected only alpha, got %v", got)
	}
}

func TestDescribeCheckTruncation(t *testing.T) {
	long := strings.Repeat("x", 100)
	s := describeCheck("c", HealthCheck{Type: "http", URL: long})
	if len(s) > 60 || !strings.HasSuffix(s, "...") {
		t.Fatalf("expected truncation to 60 chars, got %d: %s", len(s), s)
	}
	if got := describeCheck("p", HealthCheck{Type: "tcp", Port: 9100}); got != "p (tcp:9100)" {
		t.Fatalf("unexpected tcp rendering: %s", got)
	}
}
