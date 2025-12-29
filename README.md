# Orbitd 🛰️

A lightweight, zero-configuration container update daemon that keeps your Docker containers automatically up-to-date.

Orbitd monitors your running containers and seamlessly updates them to the latest image versions while preserving all configuration, networks, volumes, and labels. Perfect for self-hosted services, homelab setups, and Docker Compose environments.

## Features

- **Automatic Updates** - Continuously monitors containers for new image versions
- **Automatic Cleanup** - Optionally removes old images after successful updates
- **Smart Detection** - Only updates when image digest actually changes
- **Lightweight** - Single binary with minimal resource footprint

## Quick Start

### Using Docker (Recommended)

```bash
docker run -d \
   --name orbitd \
   -v /var/run/docker.sock:/var/run/docker.sock \
   -e ORBITD_POLICY=digest \
   -e ORBITD_INTERVAL=12h \
   -e ORBITD_CLEANUP=true \
   ghcr.io/mizuchilabs/orbitd:latest
```

### Using Docker Compose

```yaml
services:
  orbitd:
    image: ghcr.io/mizuchilabs/orbitd:latest
    container_name: orbitd
    volumes:
      - /var/run/docker.sock:/var/run/docker.sock
      # - ~/.docker/config.json:/root/.docker/config.json:ro # optional (for private registries)
    environment:
      - ORBITD_POLICY=digest
      - ORBITD_INTERVAL=12h
      - ORBITD_CLEANUP=true
    restart: unless-stopped
```

### Using Binary

Download the latest [release](https://github.com/mizuchilabs/orbitd/releases) for your platform and run with `./orbitd`

## Configuration

Orbitd can be configured via command-line flags or environment variables:

| Flag         | Environment Variable | Default  | Description                                        |
| ------------ | -------------------- | -------- | -------------------------------------------------- |
| `--policy`   | `ORBITD_POLICY`      | `digest` | Update policy (patch, minor, major, digest)        |
| `--interval` | `ORBITD_INTERVAL`    | `12h`    | How often to check for updates (e.g., 5m, 1h, 24h) |
| `--cleanup`  | `ORBITD_CLEANUP`     | `true`   | Remove old images after successful updates         |
| `--debug`    | `ORBITD_DEBUG`       | `false`  | Enable debug logging for detailed output           |

## How It Works

1. **Discovery** - Scans all running containers on the Docker host
2. **Update Check** - Pulls the latest version of each container's image
3. **Digest Comparison** - Compares image digests to detect actual changes
4. **Recreation** - If updated, stops the old container and recreates it with:
   - Same name and configuration
   - All environment variables preserved
   - All volume mounts intact
   - All network connections maintained
   - All labels and metadata preserved
5. **Cleanup** - Optionally removes the old image to free disk space

### Additional notes:

> - Only updates containers that are currently running, stopped containers are left untouched.
> - Updates only when image digest actually changes.
> - By default watches all containers, you can disable watching a container by adding a label `orbitd.enable=false` to it.
> - You can set per container update policy by adding a label e.g. `orbitd.policy=patch` to it.

## License

Apache 2.0 License - see [LICENSE](LICENSE) for details

## Contributing

Contributions are welcome! Please feel free to submit a Pull Request.
