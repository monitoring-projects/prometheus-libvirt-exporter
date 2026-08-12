package main

import (
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"strings"
	"time"

	kingpin "github.com/alecthomas/kingpin/v2"
	"github.com/digitalocean/go-libvirt"
	exporter "github.com/inovex/prometheus-libvirt-exporter/pkg/exporter"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	"github.com/prometheus/common/promslog"
	"github.com/prometheus/common/promslog/flag"
	prometheus_version "github.com/prometheus/common/version"
	"github.com/prometheus/exporter-toolkit/web"
	webflag "github.com/prometheus/exporter-toolkit/web/kingpinflag"
)

var (
	version string
	logger  *slog.Logger
)

func main() {

	prometheus_version.Version = version

	var (
		libvirtURI = kingpin.Flag("libvirt.uri",
			"Libvirt URI from which to extract metrics.",
		).Default("/var/run/libvirt/libvirt-sock-ro").String()
		driver = kingpin.Flag("libvirt.driver",
			fmt.Sprintf("Available drivers: %s (Default), %s, %s and %s ", libvirt.QEMUSystem, libvirt.QEMUSession, libvirt.XenSystem, libvirt.TestDefault),
		).Default(string(libvirt.QEMUSystem)).String()
		timeout = kingpin.Flag("exporter.timeout",
			"Maximum libvirt API call duration.",
		).Default("3s").Duration()
		maxConcurrentCollects = kingpin.Flag("exporter.max-concurrent-collects",
			"Maximum number of concurrent collects (min: 1).",
		).Default("4").Int()
		libvirtSSHKeyFile = kingpin.Flag("libvirt.ssh-key-file",
			"Path to a private SSH key file used for qemu+ssh:// URIs.",
		).Default("").String()
	)

	metricsPath := kingpin.Flag(
		"web.telemetry-path", "Path under which to expose metrics",
	).Default("/metrics").String()
	healthPath := kingpin.Flag(
		"web.health-path", "Path under which to expose the health endpoint. Set to non-empty to enable.",
	).Default("").String()
	toolkitFlags := webflag.AddFlags(kingpin.CommandLine, ":9177")

	promlogConfig := &promslog.Config{}
	flag.AddFlags(kingpin.CommandLine, promlogConfig)
	kingpin.Version(prometheus_version.Print("libvirt_exporter"))
	kingpin.HelpFlag.Short('h')
	kingpin.Parse()
	logger = promslog.New(promlogConfig)

	// ensure maxConcurrentCollects is not less than 1
	if *maxConcurrentCollects < 1 {
		logger.Info("max-concurrent-collects must be at least 1, setting to 1")
		*maxConcurrentCollects = 1
	}

	logger.Info("Starting libvirt_exporter", "version", prometheus_version.Info())
	logger.Info("Build context", "build_context", prometheus_version.BuildContext())
	logger.Info("Timeout value", "timeout_value", *timeout)
	logger.Info("Max concurrent collects", "max_concurrent_collects", *maxConcurrentCollects)

	// Create local exporter and register it with the default registry
	localExporter, err := exporter.NewLibvirtExporter(*libvirtURI, libvirt.ConnectURI(*driver), logger, *timeout, *maxConcurrentCollects, *libvirtSSHKeyFile)
	if err != nil {
		panic(err)
	}
	prometheus.MustRegister(localExporter)

	// Setup metrics handler with remote target support
	http.HandleFunc(*metricsPath, metricsHandler(*timeout, *maxConcurrentCollects, *libvirtSSHKeyFile))
	http.HandleFunc("/cluster", clusterHandler(logger, *timeout, *libvirtSSHKeyFile))
	http.HandleFunc("/discover", discoverHandler(logger))
	logger.Info("Cluster endpoint enabled", "path", "/cluster")
	logger.Info("Discovery endpoint enabled", "path", "/discover")

	if *healthPath != "" {
		http.HandleFunc(*healthPath, localExporter.HealthHandler)
		logger.Info("Health endpoint enabled", "path", *healthPath)
	}
	if *metricsPath != "/" {
		landingCnf := web.LandingConfig{
			Name:        "Libvirt Exporter",
			Description: "Prometheus Libvirt Exporter",
			Version:     prometheus_version.Info(),
			Links: []web.LandingLinks{
				{
					Address: *metricsPath,
					Text:    "Metrics",
				},
				{
					Address: "/cluster",
					Text:    "Cluster State (JSON)",
				},
				{
					Address: "/discover",
					Text:    "Service Discovery (JSON)",
				},
			},
		}
		if *healthPath != "" {
			landingCnf.Links = append(landingCnf.Links, web.LandingLinks{
				Address: *healthPath,
				Text:    "Health",
			})
		}
		landingPage, err := web.NewLandingPage(landingCnf)
		if err != nil {
			logger.Error("Failed to generate landing page", "msg", err)
			os.Exit(1)
		}
		http.Handle("/", landingPage)
	}

	srv := &http.Server{}
	if err = web.ListenAndServe(srv, toolkitFlags, logger); err != nil {
		logger.Error("Failed to start server", "msg", err)
		os.Exit(1)
	}
}

// metricsHandler returns metrics for either the local instance or a remote target.
// If the 'target' query parameter is provided, it scrapes that remote libvirt URI.
// Otherwise, it returns metrics from the default prometheus registry (local instance).
func metricsHandler(timeout time.Duration, maxConcurrentCollects int, sshKeyFile string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		target := r.URL.Query().Get("target")
		// URL query decoding turns literal '+' into a space, which breaks
		// libvirt schemes like "qemu+ssh". Restore '+' before parsing.
		target = strings.ReplaceAll(target, " ", "+")

		// If target parameter is provided, scrape remote libvirt instance
		if target != "" {
			if !strings.Contains(target, "://") && !strings.HasPrefix(target, "/") {
				logger.Error("Invalid target query parameter", "target", target)
				http.Error(w, fmt.Sprintf("target must be a valid libvirt URI, got %q", target), http.StatusBadRequest)
				return
			}
			logger.Debug("Scraping remote target via metrics endpoint", "target", target)

			// Create a new registry for this remote target
			registry := prometheus.NewRegistry()
			remoteExporter, err := exporter.NewLibvirtExporter(target, libvirt.ConnectURI(target), logger, timeout, maxConcurrentCollects, sshKeyFile)
			if err != nil {
				logger.Error("Failed to create remote exporter", "target", target, "error", err)
				http.Error(w, fmt.Sprintf("Failed to create exporter for target %q: %v", target, err), http.StatusBadRequest)
				return
			}
			registry.MustRegister(remoteExporter)
			h := promhttp.HandlerFor(registry, promhttp.HandlerOpts{})
			h.ServeHTTP(w, r)
			return
		}

		// If no target parameter, return local metrics using the default handler
		promhttp.Handler().ServeHTTP(w, r)
	}
}
