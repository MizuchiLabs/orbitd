<p align="center">
<img src="./.github/logo.svg" width="80">
<br><br>
<img alt="GitHub Tag" src="https://img.shields.io/github/v/tag/MizuchiLabs/orbitd?label=Version">
<img alt="GitHub License" src="https://img.shields.io/github/license/MizuchiLabs/orbitd">
<img alt="GitHub Issues or Pull Requests" src="https://img.shields.io/github/issues/MizuchiLabs/orbitd">
</p>

# Orbitd

A lightweight, set-and-forget container update daemon for Docker.

Orbitd monitors your containers and automatically updates them when new images are available—preserving all configuration, networks, volumes, and labels.

### Features

- **Zero Configuration** — Works out of the box with sensible defaults
- **Automatic Rollback** — Restores previous container on update failure
- **Flexible Policies** — Digest-only or semantic versioning (patch/minor/major)
- **Label Control** — Opt-in or opt-out specific containers
- **Image Cleanup** — Removes old images after successful updates

## Quick Start

```yaml
# docker-compose.yml
services:
  orbitd:
    image: ghcr.io/mizuchilabs/orbitd:latest
    restart: unless-stopped
    volumes:
      - /var/run/docker.sock:/var/run/docker.sock:ro
```

That's it. Orbitd will check all containers every 12 hours and update them when new digests are available.

## Configuration

All settings are optional. Configure via environment variables or CLI flags:

| Environment Variable   | CLI Flag          | Default  | Description                        |
| ---------------------- | ----------------- | -------- | ---------------------------------- |
| `ORBITD_INTERVAL`      | `--interval`      | `12h`    | Check frequency (e.g., `5m`, `1h`) |
| `ORBITD_POLICY`        | `--policy`        | `digest` | Update policy (see below)          |
| `ORBITD_CLEANUP`       | `--cleanup`       | `true`   | Remove old images after updates    |
| `ORBITD_REQUIRE_LABEL` | `--require-label` | `false`  | Only update labeled containers     |
| `ORBITD_DEBUG`         | `--debug`         | `false`  | Enable verbose logging             |

### Update Policies

| Policy   | Behavior                       | Example                     |
| -------- | ------------------------------ | --------------------------- |
| `digest` | Same tag, new digest (default) | `nginx:1.25` → latest build |
| `patch`  | Patch versions only            | `1.2.3` → `1.2.9`           |
| `minor`  | Minor + patch versions         | `1.2.3` → `1.9.0`           |
| `major`  | Any newer version              | `1.2.3` → `2.0.0`           |

> Semver policies require valid semver tags. Non-semver tags fall back to digest updates.

## Container Labels

Control individual containers with labels:

```yaml
services:
  # Updated automatically (default behavior)
  app:
    image: myapp:latest

  # Excluded from updates
  database:
    image: postgres:15
    labels:
      - "orbitd.enable=false"

  # Custom policy for this container
  api:
    image: myapi:1.0.0
    labels:
      - "orbitd.policy=minor"
```

### Opt-In Mode

With `ORBITD_REQUIRE_LABEL=true`, only containers with `orbitd.enable=true` are monitored:

```yaml
services:
  # Only this container is updated
  app:
    image: myapp:latest
    labels:
      - "orbitd.enable=true"

  # Ignored
  database:
    image: postgres:15
```

## Installation

### Docker (recommended)

```bash
docker run -d \
  --name orbitd \
  --restart unless-stopped \
  -v /var/run/docker.sock:/var/run/docker.sock:ro \
  ghcr.io/mizuchilabs/orbitd:latest
```

### Binary

Download from [releases](https://github.com/mizuchilabs/orbitd/releases) and run:

```bash
./orbitd start
```

## License

Apache 2.0 License - see [LICENSE](LICENSE) for details

## Contributing

Contributions are welcome! Please feel free to submit a Pull Request.
