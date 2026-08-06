# Cluster Monitoring

This document covers the two mechanisms for monitoring multiple libvirt hypervisors:

- `GET /metrics?target=<URI>` - scrape a remote (or local) libvirt instance using a QEMU URI
- `GET /cluster` - aggregated JSON view of all hypervisors listed in a targets file

Both use QEMU connection URIs, which are the standard way libvirt exposes remote access. Unix sockets are **not** suitable for remote cluster nodes.

## Supported QEMU URIs

| URI | Description |
|-----|-------------|
| `qemu:///system` | Local system daemon |
| `qemu:///session` | Local user session |
| `qemu+ssh://user@host/system` | Remote over SSH |
| `qemu+tcp://host/system` | Remote over TCP (insecure, dev only) |
| `qemu+tls://host/system` | Remote over TLS |

## Quick Start

### 1. Prepare SSH access for remote nodes

```bash
ssh-keygen -t ed25519 -f ~/.ssh/id_rsa_libvirt -C libvirt-exporter
ssh-copy-id -i ~/.ssh/id_rsa_libvirt.pub root@node1.example.com
ssh-copy-id -i ~/.ssh/id_rsa_libvirt.pub root@node2.example.com

# Test
virsh -c qemu+ssh://root@node1.example.com/system list
```

Notes for the exporter:

- The exporter uses the Go SSH client, not the `ssh` binary. Place the private key at `~/.ssh/id_rsa` (or one of `id_ed25519`, `id_ecdsa`, `id_rsa`) so it is picked up automatically.
- When running in a container, mount the private key into the container (e.g. `~/.ssh/id_rsa` at `/.ssh/id_rsa` for `FROM scratch` images).
- Unknown SSH host keys are accepted on first connect and written to `~/.config/libvirt/known_hosts` if writable. If you prefer strict verification, mount a pre-populated `known_hosts` file to that path.

### 2. Start the exporter

```bash
./prometheus-libvirt-exporter --web.listen-address=:9177
```

### 3. Scrape a single remote target

```bash
curl 'http://localhost:9177/metrics?target=qemu+ssh://root@node1.example.com/system' | grep libvirt_up
```

### 4. Configure and query `/cluster`

Create `/etc/libvirt-exporter/libvirt-targets.yml`:

```yaml
- targets:
  - qemu:///system
  labels:
    datacenter: dc1
    role: local

- targets:
  - qemu+ssh://root@node1.example.com/system
  - qemu+ssh://root@node2.example.com/system
  labels:
    datacenter: dc1
    role: compute
```

Query cluster state:

```bash
curl http://localhost:9177/cluster | jq '.cluster.summary'
curl http://localhost:9177/cluster | jq '.cluster.nodes[] | {host, up, domain_count}'
```

## `/metrics?target=`

### cURL

```bash
# Local
curl http://localhost:9177/metrics

# Remote
curl 'http://localhost:9177/metrics?target=qemu+ssh://root@node1.example.com/system'
```

### Prometheus

```yaml
scrape_configs:
  - job_name: 'libvirt'
    static_configs:
      - targets:
        - qemu+ssh://root@node1.example.com/system
        - qemu+ssh://root@node2.example.com/system
    metrics_path: /metrics
    relabel_configs:
      - source_labels: [__address__]
        target_label: __param_target
      - source_labels: [__param_target]
        target_label: instance
      - target_label: __address__
        replacement: localhost:9177
```

## `/cluster`

### Target file format

The file is a list of target groups:

```yaml
- targets:
  - qemu+ssh://root@node1.example.com/system
  - qemu+ssh://root@node2.example.com/system
  labels:
    datacenter: dc1

- targets:
  - qemu+ssh://root@node3.example.com/system
  labels:
    datacenter: dc2
```

Default path: `/etc/libvirt-exporter/libvirt-targets.yml`. Override with `?targets_file=`.

### JSON response

```json
{
  "cluster": {
    "generated_at": "2024-08-03T20:00:00Z",
    "scrape_duration_seconds": 2.45,
    "targets_file": "/etc/libvirt-exporter/libvirt-targets.yml",
    "summary": {
      "total_nodes": 3,
      "nodes_up": 3,
      "nodes_down": 0,
      "overall_health": "healthy",
      "health_counts": {
        "healthy": 3,
        "warning": 0,
        "critical": 0,
        "unknown": 0
      },
      "total_domains": 42,
      "domains_running": 38,
      "domains_paused": 1,
      "domains_shutoff": 3
    },
    "nodes": [
      {
        "host": "qemu+ssh://root@node1.example.com/system",
        "connection_uri": "qemu+ssh://root@node1.example.com/system",
        "labels": { "datacenter": "dc1" },
        "up": true,
        "scrape_duration_seconds": 0.85,
        "health": { "status": "healthy", "issues": [] },
        "domain_count": 8,
        "domains_running": 7,
        "domains_paused": 0,
        "domains_shutoff": 1,
        "domains": [...],
        "storage_pools": [...]
      }
    ]
  }
}
```

### Health levels

- `healthy` - all systems operational
- `warning` - non-critical issues
- `critical` - unreachable node or crashed domain
- `unknown` - status could not be determined

The cluster `overall_health` is the worst node health.

## Endpoint comparison

| Feature | `/metrics` | `/metrics?target=` | `/cluster` |
|---------|-----------|-------------------|-----------|
| Response format | Prometheus text | Prometheus text | JSON |
| Scope | Local instance | Single URI | All targets in file |
| Configuration | Command flags | Query parameter | YAML targets file |
| Use case | Prometheus scraping | Prometheus multi-target scraping | Dashboards / APIs |
| Health aggregation | No | No | Yes |

## SSH Configuration

A minimal `~/.ssh/config` for monitoring:

```
Host *.example.com
    User root
    IdentityFile ~/.ssh/id_rsa_libvirt
    StrictHostKeyChecking no
    UserKnownHostsFile /dev/null
    ConnectTimeout 10
    ControlMaster auto
    ControlPath ~/.ssh/sockets/%r@%h:%p
    ControlPersist 10m
```

Create the socket directory:

```bash
mkdir -p ~/.ssh/sockets
```

## Performance notes

- Remote scrapes over SSH take ~1-3 seconds each.
- `/cluster` scrapes up to **16 nodes concurrently**.
- Increase `--exporter.timeout` for high-latency networks:
  ```bash
  ./prometheus-libvirt-exporter --exporter.timeout=10s
  ```
- Use different scrape intervals for local vs remote targets:
  ```yaml
  scrape_configs:
    - job_name: 'libvirt-local'
      scrape_interval: 15s
      static_configs:
        - targets: ['localhost:9177']
    - job_name: 'libvirt-remote'
      scrape_interval: 60s
      ...
  ```

## Security checklist

- [ ] Use dedicated SSH keys for monitoring
- [ ] Use a non-root user if possible and add it to the `libvirt` group on remote hosts
- [ ] Restrict SSH access via firewall
- [ ] Enable audit logging on remote libvirt instances
- [ ] Use TLS for the exporter HTTP endpoint if exposed

## Troubleshooting

### Remote target scrape fails

```bash
# Verify SSH
ssh root@node1.example.com virsh list

# Verify libvirt URI
virsh -c qemu+ssh://root@node1.example.com/system list

# Try from the exporter user
sudo -u prometheus-libvirt-exporter \
  virsh -c qemu+ssh://root@node1.example.com/system list
```

### `/cluster` returns error

```bash
# Check file exists and is valid YAML
ls -la /etc/libvirt-exporter/libvirt-targets.yml
yamllint /etc/libvirt-exporter/libvirt-targets.yml

# Try custom file
curl 'http://localhost:9177/cluster?targets_file=/tmp/test-targets.yml' | jq .
```

### Scrapes time out

- Increase `--exporter.timeout`
- Check network latency: `ping node1.example.com`
- Enable SSH connection reuse via `ControlMaster`

## References

- [Libvirt Connection URIs](https://libvirt.org/uri.html)
- [Prometheus Multi-target Exporter Pattern](https://prometheus.io/docs/guides/multi-target-exporter/)
