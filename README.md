<p align="center">
<img src="./.github/logo.svg" width="80">
<br><br>
<img alt="GitHub Tag" src="https://img.shields.io/github/v/tag/MizuchiLabs/orbitd?label=Version">
<img alt="GitHub License" src="https://img.shields.io/github/license/MizuchiLabs/orbitd">
<img alt="GitHub Issues or Pull Requests" src="https://img.shields.io/github/issues/MizuchiLabs/orbitd">
</p>

# Orbitd

A lightweight container update daemon that automatically keeps your Docker containers up-to-date.

Orbitd monitors running containers for new image versions and seamlessly recreates them with the latest digest while preserving all configuration, networks, volumes, and labels. Perfect for self-hosted services and Docker Compose setups.

## Features

- **Automatic Updates**: Monitors containers and updates them when new images are available
- **Flexible Policies**: Choose between digest-only updates or semantic versioning (patch/minor/major)
- **Label-Based Control**: Opt-in or opt-out containers using simple Docker labels
- **Automatic Cleanup**: Optionally remove old images after successful updates
- **Configuration Preservation**: Maintains all container settings, volumes, networks, and labels
- **Lightweight** - Single binary with minimal resource footprint

## Quick Start

```bash
# Monitor all containers, check every 12 hours
orbitd start

# Opt-in mode: only update containers with orbitd.enable=true
orbitd start --require-label

# Check every 5 minutes with patch-level updates
orbitd start --interval 5m --policy patch
```

## Update Policies

Orbitd supports four update policies:

- **`digest`** (default): Updates to new image digests while keeping the same tag (e.g., `nginx:latest`)
- **`patch`**: Updates patch versions only (e.g., `1.2.3` → `1.2.4`, follows `~1.2.3`)
- **`minor`**: Updates minor and patch versions (e.g., `1.2.3` → `1.3.0`, follows `^1.2.3`)
- **`major`**: Updates to any newer version (e.g., `1.2.3` → `2.0.0`)

Note: Semantic versioning policies (patch/minor/major) only work with tags that follow [SemVer](https://semver.org/). If a tag isn't valid SemVer, Orbitd falls back to digest-based updates.

## Container Control

Control which containers Orbitd monitors using the `orbitd.enable` label:

### Monitor All Containers (Default)

By default, Orbitd monitors all running containers except those explicitly disabled:

```yaml
services:
  app:
    image: myapp:latest
    # This container will be monitored

  database:
    image: postgres:15
    labels:
      - "orbitd.enable=false" # Exclude from monitoring
```

### Opt-In Mode

Use `--require-label` to only monitor containers explicitly enabled:

```bash
orbitd start --require-label
```

```yaml
services:
  app:
    image: myapp:latest
    labels:
      - "orbitd.enable=true" # Only this container is monitored

  database:
    image: postgres:15
    # This container will be ignored
```

> Note: You can set per container update policy by adding a label e.g. `orbitd.policy=patch` to it.

## Installation

### Docker

```bash
docker run -d \
   --name orbitd \
   -v /var/run/docker.sock:/var/run/docker.sock \
   ghcr.io/mizuchilabs/orbitd:latest
```

### Docker Compose

```yaml
services:
  orbitd:
    image: ghcr.io/mizuchilabs/orbitd:latest
    container_name: orbitd
    restart: unless-stopped
    volumes:
      - /var/run/docker.sock:/var/run/docker.sock:ro
    environment:
      - ORBITD_INTERVAL=12h
      - ORBITD_POLICY=digest
      - ORBITD_CLEANUP=true
```

### Binary

Download the latest [release](https://github.com/mizuchilabs/orbitd/releases) for your platform and run with `./orbitd`

## Configuration

Orbitd can be configured via command-line flags or environment variables:

| Flag              | Environment Variable   | Default  | Description                                             |
| ----------------- | ---------------------- | -------- | ------------------------------------------------------- |
| `--policy`        | `ORBITD_POLICY`        | `digest` | Update policy (patch, minor, major, digest)             |
| `--interval`      | `ORBITD_INTERVAL`      | `12h`    | How often to check for updates (e.g., 5m, 1h, 24h)      |
| `--cleanup`       | `ORBITD_CLEANUP`       | `true`   | Remove old images after successful updates              |
| `--require-label` | `ORBITD_REQUIRE_LABEL` | `false`  | Require `orbitd.enable=true` label to update containers |
| `--debug`         | `ORBITD_DEBUG`         | `false`  | Enable debug logging for detailed output                |

## License

Apache 2.0 License - see [LICENSE](LICENSE) for details

## Contributing

Contributions are welcome! Please feel free to submit a Pull Request.
