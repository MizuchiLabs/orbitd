<p align="center">
<img src="./.github/logo.svg" width="80">
<br><br>
<img alt="GitHub Tag" src="https://img.shields.io/github/v/tag/MizuchiLabs/orbitd?label=Version">
<img alt="GitHub License" src="https://img.shields.io/github/license/MizuchiLabs/orbitd">
<img alt="GitHub Issues or Pull Requests" src="https://img.shields.io/github/issues/MizuchiLabs/orbitd">
</p>

# Orbitd

A lightweight, set-and-forget container update daemon for Docker.

Orbitd monitors your containers and automatically updates them when new images are available, preserving all configuration, networks, volumes, and labels.

### Features

- **Zero Configuration**: Works out of the box with sensible defaults
- **Automatic Rollback**: Restores previous container on update failure
- **Flexible Policies**: Digest-only or semantic versioning (patch/minor/major)
- **Label Control**: Opt-in or opt-out specific containers
- **Image Cleanup**: Removes old images after successful updates

## Quick Start

```yaml
services:
  orbitd:
    image: ghcr.io/mizuchilabs/orbitd:latest
    container_name: orbitd
    restart: unless-stopped
    volumes:
      - /var/run/docker.sock:/var/run/docker.sock:ro
```

That's it. Orbitd will check all containers every 12 hours (by default) and update them when new digests are available.

Since v0.1.9, you can also run orbitd in docker swarm:

```yaml
services:
  orbitd:
    image: ghcr.io/mizuchilabs/orbitd:latest
    volumes:
      - /var/run/docker.sock:/var/run/docker.sock:ro
    deploy:
      replicas: 1
      placement:
        constraints:
          - node.role == manager
```

## Configuration

All settings are optional. Configure via environment variables or CLI flags:

| Environment Variable   | CLI Flag          | Default      | Description                          |
| ---------------------- | ----------------- | ------------ | ------------------------------------ |
| `ORBITD_SCHEDULE`      | `--schedule`      | `@every 12h` | Cron schedule or interval descriptor |
| `ORBITD_POLICY`        | `--policy`        | `digest`     | Update policy (see below)            |
| `ORBITD_CLEANUP`       | `--cleanup`       | `true`       | Remove old images after updates      |
| `ORBITD_REQUIRE_LABEL` | `--require-label` | `false`      | Only update labeled containers       |
| `ORBITD_DEBUG`         | `--debug`         | `false`      | Enable verbose logging               |

### Scheduling

Orbitd uses standard cron expressions or interval descriptors for scheduling updates via the `ORBITD_SCHEDULE` environment variable (or `--schedule` flag).

**Examples:**

- `@every 12h` - Run every 12 hours from start (Default)
- `@every 30m` - Run every 30 minutes
- `0 3 * * *` - Run daily at 3:00 AM
- `0 0 * * 0` - Run weekly on Sunday at midnight

### Update Policies

| Policy   | Behavior                       | Example                     |
| -------- | ------------------------------ | --------------------------- |
| `digest` | Same tag, new digest (default) | `nginx:1.25` → latest build |
| `patch`  | Patch versions only            | `1.2.3` → `1.2.9`           |
| `minor`  | Minor + patch versions         | `1.2.3` → `1.9.0`           |
| `major`  | Any newer version              | `1.2.3` → `2.0.0`           |

> Semver policies require valid semver tags. Non-semver tags fall back to digest updates.

## Container Labels

By default, orbitd monitors **all** running containers. Use `ORBITD_REQUIRE_LABEL=true` to switch to opt-in mode, where only containers with `orbitd.enable=true` are monitored.

You can also override the update policy per container with `orbitd.policy`:

```yaml
services:
  # Opt-in to monitoring (required when require-label is enabled)
  app:
    image: myapp:latest
    labels:
      - "orbitd.enable=true"

  # Override policy for this container
  api:
    image: myapi:1.0.0
    labels:
      - "orbitd.enable=true"
      - "orbitd.policy=minor"

  # Not monitored (no label)
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

## Private Registries

Orbitd uses Docker's default credential chain to authenticate with private registries. To pull images from private repositories, mount your Docker config file into the orbitd container:

```yaml
services:
  orbitd:
    image: ghcr.io/mizuchilabs/orbitd:latest
    volumes:
      - /var/run/docker.sock:/var/run/docker.sock:ro
      - /home/youruser/.docker/config.json:/root/.docker/config.json:ro
```

This config file is populated by running `docker login` on your host. Orbitd will use these credentials for both image tag discovery and pulling.

## License

Apache 2.0 License - see [LICENSE](LICENSE) for details

## Contributing

Contributions are welcome! Please feel free to submit a Pull Request.
