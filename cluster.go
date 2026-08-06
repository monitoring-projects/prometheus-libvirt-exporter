package main

import (
	"encoding/json"
	"io"
	"log/slog"
	"math"
	"net/http"
	"net/http/httptest"
	"os"
	"sort"
	"strconv"
	"sync"
	"time"

	"github.com/digitalocean/go-libvirt"
	exporter "github.com/inovex/prometheus-libvirt-exporter/pkg/exporter"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	dto "github.com/prometheus/client_model/go"
	"github.com/prometheus/common/expfmt"
	yaml "gopkg.in/yaml.v3"
)

const defaultTargetsFile = "/etc/libvirt-exporter/libvirt-targets.yml"

type targetGroup struct {
	Targets []string          `yaml:"targets"`
	Labels  map[string]string `yaml:"labels,omitempty"`
}

type JSONFloat float64

func (f JSONFloat) MarshalJSON() ([]byte, error) {
	v := float64(f)
	if math.IsNaN(v) || math.IsInf(v, 0) {
		return []byte("null"), nil
	}
	return []byte(strconv.FormatFloat(v, 'f', -1, 64)), nil
}

const (
	healthHealthy  = "healthy"
	healthUnknown  = "unknown"
	healthWarning  = "warning"
	healthCritical = "critical"
)

var healthRank = map[string]int{
	healthHealthy:  0,
	healthUnknown:  1,
	healthWarning:  2,
	healthCritical: 3,
}

func worseHealth(a, b string) string {
	if healthRank[b] > healthRank[a] {
		return b
	}
	return a
}

type domainInfo struct {
	Name           string     `json:"name"`
	UUID           string     `json:"uuid,omitempty"`
	State          string     `json:"state"`
	StateCode      *JSONFloat `json:"state_code,omitempty"`
	VCPUs          *JSONFloat `json:"vcpus,omitempty"`
	MemoryBytes    *JSONFloat `json:"memory_bytes,omitempty"`
	MaxMemoryBytes *JSONFloat `json:"max_memory_bytes,omitempty"`
	CPUTimeSeconds *JSONFloat `json:"cpu_time_seconds,omitempty"`
}

type storagePoolInfo struct {
	Name            string     `json:"name"`
	State           *JSONFloat `json:"state,omitempty"`
	CapacityBytes   *JSONFloat `json:"capacity_bytes,omitempty"`
	AllocationBytes *JSONFloat `json:"allocation_bytes,omitempty"`
	AvailableBytes  *JSONFloat `json:"available_bytes,omitempty"`
}

type nodeHealth struct {
	Status string   `json:"status"`
	Issues []string `json:"issues,omitempty"`
}

type NodeState struct {
	Host                  string             `json:"host"`
	ConnectionURI         string             `json:"connection_uri"`
	Labels                map[string]string  `json:"labels,omitempty"`
	Up                    bool               `json:"up"`
	Error                 string             `json:"error,omitempty"`
	ScrapeDurationSeconds JSONFloat          `json:"scrape_duration_seconds"`
	Health                nodeHealth         `json:"health"`
	Domains               []*domainInfo      `json:"domains,omitempty"`
	StoragePools          []*storagePoolInfo `json:"storage_pools,omitempty"`
	DomainCount           int                `json:"domain_count"`
	DomainsRunning        int                `json:"domains_running"`
	DomainsPaused         int                `json:"domains_paused"`
	DomainsShutoff        int                `json:"domains_shutoff"`
}

type clusterSummary struct {
	TotalNodes     int            `json:"total_nodes"`
	NodesUp        int            `json:"nodes_up"`
	NodesDown      int            `json:"nodes_down"`
	OverallHealth  string         `json:"overall_health"`
	HealthCounts   map[string]int `json:"health_counts"`
	TotalDomains   int            `json:"total_domains"`
	DomainsRunning int            `json:"domains_running"`
	DomainsPaused  int            `json:"domains_paused"`
	DomainsShutoff int            `json:"domains_shutoff"`
}

type ClusterState struct {
	GeneratedAt           string         `json:"generated_at"`
	ScrapeDurationSeconds JSONFloat      `json:"scrape_duration_seconds"`
	TargetsFile           string         `json:"targets_file"`
	Summary               clusterSummary `json:"summary"`
	Nodes                 []*NodeState   `json:"nodes"`
}

func clusterHandler(logger *slog.Logger, timeout time.Duration) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()

		targetsFile := defaultTargetsFile
		if v := r.URL.Query().Get("targets_file"); v != "" {
			targetsFile = v
		}

		data, err := os.ReadFile(targetsFile)
		if err != nil {
			logger.Error("Failed to read targets file for /cluster", "error", err, "file", targetsFile)
			http.Error(w, "Failed to read targets file", http.StatusInternalServerError)
			return
		}

		var groups []targetGroup
		if err := yaml.Unmarshal(data, &groups); err != nil {
			logger.Error("Failed to parse targets file for /cluster", "error", err, "file", targetsFile)
			http.Error(w, "Failed to parse targets file", http.StatusInternalServerError)
			return
		}

		type job struct {
			connectionURI string
			labels        map[string]string
		}
		var jobs []job
		for _, g := range groups {
			for _, t := range g.Targets {
				jobs = append(jobs, job{connectionURI: t, labels: g.Labels})
			}
		}

		nodes := make([]*NodeState, len(jobs))

		const maxConcurrency = 16
		sem := make(chan struct{}, maxConcurrency)
		var wg sync.WaitGroup
		for i, j := range jobs {
			wg.Add(1)
			sem <- struct{}{}
			go func(i int, j job) {
				defer wg.Done()
				defer func() { <-sem }()
				nodes[i] = scrapeNode(j.connectionURI, j.labels, logger, timeout)
			}(i, j)
		}
		wg.Wait()

		cluster := buildClusterState(nodes, targetsFile, time.Since(start).Seconds())

		w.Header().Set("Content-Type", "application/json")
		enc := json.NewEncoder(w)
		enc.SetIndent("", "  ")
		if err := enc.Encode(map[string]*ClusterState{"cluster": cluster}); err != nil {
			logger.Error("Failed to encode /cluster response", "error", err)
		}
	}
}

func scrapeNode(connectionURI string, labels map[string]string, logger *slog.Logger, timeout time.Duration) *NodeState {
	node := &NodeState{
		Host:          connectionURI,
		ConnectionURI: connectionURI,
		Labels:        labels,
	}

	start := time.Now()

	req := httptest.NewRequest("GET", "/metrics", nil)
	rec := httptest.NewRecorder()

	registry := prometheus.NewRegistry()

	libvirtExporter, err := exporter.NewLibvirtExporter(connectionURI, libvirt.ConnectURI(connectionURI), logger, timeout, 4)
	if err != nil {
		node.Error = "failed to create exporter: " + err.Error()
		node.ScrapeDurationSeconds = JSONFloat(time.Since(start).Seconds())
		computeNodeHealth(node)
		return node
	}

	registry.MustRegister(libvirtExporter)
	h := promhttp.HandlerFor(registry, promhttp.HandlerOpts{})
	h.ServeHTTP(rec, req)

	node.ScrapeDurationSeconds = JSONFloat(time.Since(start).Seconds())

	if rec.Code != http.StatusOK {
		node.Error = rec.Body.String()
		computeNodeHealth(node)
		return node
	}

	format := expfmt.ResponseFormat(rec.Header())
	if format == expfmt.NewFormat(expfmt.TypeUnknown) {
		format = expfmt.NewFormat(expfmt.TypeTextPlain)
	}

	var families []*dto.MetricFamily
	dec := expfmt.NewDecoder(rec.Body, format)
	for {
		mf := &dto.MetricFamily{}
		if err := dec.Decode(mf); err != nil {
			if err == io.EOF {
				break
			}
			node.Error = "parse metrics: " + err.Error()
			computeNodeHealth(node)
			return node
		}
		families = append(families, mf)
	}

	populateNodeFromFamilies(node, families)
	computeNodeHealth(node)
	return node
}

func populateNodeFromFamilies(node *NodeState, families []*dto.MetricFamily) {
	domainByName := map[string]*domainInfo{}
	poolByName := map[string]*storagePoolInfo{}

	getOrCreateDomain := func(name string) *domainInfo {
		if d, ok := domainByName[name]; ok {
			return d
		}
		d := &domainInfo{Name: name}
		domainByName[name] = d
		return d
	}

	getOrCreatePool := func(name string) *storagePoolInfo {
		if p, ok := poolByName[name]; ok {
			return p
		}
		p := &storagePoolInfo{Name: name}
		poolByName[name] = p
		return p
	}

	for _, fam := range families {
		name := fam.GetName()
		for _, m := range fam.Metric {
			lbl := map[string]string{}
			for _, l := range m.Label {
				lbl[l.GetName()] = l.GetValue()
			}
			val := 0.0
			if m.Gauge != nil {
				val = m.Gauge.GetValue()
			} else if m.Untyped != nil {
				val = m.Untyped.GetValue()
			} else if m.Counter != nil {
				val = m.Counter.GetValue()
			}

			switch name {
			case "libvirt_up":
				node.Up = node.Up || val == 1
			case "libvirt_domains":
				node.DomainCount = int(val)
			case "libvirt_domain_info_state":
				domainName := lbl["domain"]
				stateDesc := lbl["state_desc"]
				d := getOrCreateDomain(domainName)
				d.State = stateDesc
				d.StateCode = finiteMetricValue(val)

				switch val {
				case 1:
					node.DomainsRunning++
				case 3:
					node.DomainsPaused++
				case 5:
					node.DomainsShutoff++
				}
			case "libvirt_domain_info_virtual_cpus":
				d := getOrCreateDomain(lbl["domain"])
				d.VCPUs = finiteMetricValue(val)
			case "libvirt_domain_info_memory_usage_bytes":
				d := getOrCreateDomain(lbl["domain"])
				d.MemoryBytes = finiteMetricValue(val)
			case "libvirt_domain_info_maximum_memory_bytes":
				d := getOrCreateDomain(lbl["domain"])
				d.MaxMemoryBytes = finiteMetricValue(val)
			case "libvirt_domain_info_cpu_time_seconds_total":
				d := getOrCreateDomain(lbl["domain"])
				d.CPUTimeSeconds = finiteMetricValue(val)
			case "libvirt_storage_pool_state":
				p := getOrCreatePool(lbl["storage_pool"])
				p.State = finiteMetricValue(val)
			case "libvirt_storage_pool_capacity_bytes":
				p := getOrCreatePool(lbl["storage_pool"])
				p.CapacityBytes = finiteMetricValue(val)
			case "libvirt_storage_pool_allocation_bytes":
				p := getOrCreatePool(lbl["storage_pool"])
				p.AllocationBytes = finiteMetricValue(val)
			case "libvirt_storage_pool_available_bytes":
				p := getOrCreatePool(lbl["storage_pool"])
				p.AvailableBytes = finiteMetricValue(val)
			}
		}
	}

	node.Domains = sortedDomains(domainByName)
	node.StoragePools = sortedPools(poolByName)
}

func finiteMetricValue(v float64) *JSONFloat {
	if math.IsNaN(v) || math.IsInf(v, 0) {
		return nil
	}
	f := JSONFloat(v)
	return &f
}

func sortedDomains(m map[string]*domainInfo) []*domainInfo {
	if len(m) == 0 {
		return nil
	}
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	out := make([]*domainInfo, 0, len(m))
	for _, k := range keys {
		out = append(out, m[k])
	}
	return out
}

func sortedPools(m map[string]*storagePoolInfo) []*storagePoolInfo {
	if len(m) == 0 {
		return nil
	}
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	out := make([]*storagePoolInfo, 0, len(m))
	for _, k := range keys {
		out = append(out, m[k])
	}
	return out
}

func computeNodeHealth(node *NodeState) {
	status := healthHealthy
	var issues []string

	if !node.Up {
		status = worseHealth(status, healthCritical)
		issues = append(issues, "node is not reachable / scrape failed")
	}

	if node.Error != "" {
		status = worseHealth(status, healthCritical)
		issues = append(issues, "error during scrape: "+node.Error)
	}

	for _, d := range node.Domains {
		if d.State == "the domain is crashed" {
			status = worseHealth(status, healthCritical)
			issues = append(issues, "domain '"+d.Name+"' is crashed")
		}
	}

	node.Health = nodeHealth{Status: status, Issues: issues}
}

func buildClusterState(nodes []*NodeState, targetsFile string, duration float64) *ClusterState {
	summary := clusterSummary{
		TotalNodes:    len(nodes),
		OverallHealth: healthHealthy,
		HealthCounts:  map[string]int{healthHealthy: 0, healthWarning: 0, healthCritical: 0, healthUnknown: 0},
	}
	for _, n := range nodes {
		if n.Up {
			summary.NodesUp++
		} else {
			summary.NodesDown++
		}
		summary.HealthCounts[n.Health.Status]++
		summary.OverallHealth = worseHealth(summary.OverallHealth, n.Health.Status)

		summary.TotalDomains += n.DomainCount
		summary.DomainsRunning += n.DomainsRunning
		summary.DomainsPaused += n.DomainsPaused
		summary.DomainsShutoff += n.DomainsShutoff
	}
	return &ClusterState{
		GeneratedAt:           time.Now().Format(time.RFC3339),
		ScrapeDurationSeconds: JSONFloat(duration),
		TargetsFile:           targetsFile,
		Summary:               summary,
		Nodes:                 nodes,
	}
}
