# Install

Vibe coded documentation. For understanding deployment. Better than nothing.

## Binary

The plugin binary is named **`oci-chroot`**.

### Build from source

Requires Go 1.26+.

```sh
go build -o oci-chroot .
```

Or use the Makefile:

```sh
make build
```

### Pre-built binaries

Pre-built binaries are attached to each [GitHub Release](https://github.com/Vaelatern/nomad-plugin-oci-chroot/releases) under the names:

- `oci-chroot-linux-amd64`
- `oci-chroot-linux-arm64`
- `oci-chroot-linux-ppc64`

## Placement

Copy the binary into Nomad's **plugin directory**. The default is:

```
/opt/nomad/plugins/oci-chroot
```

Create the directory if it does not exist:

```sh
sudo mkdir -p /opt/nomad/plugins
sudo cp oci-chroot /opt/nomad/plugins/oci-chroot
sudo chmod +x /opt/nomad/plugins/oci-chroot
```

If your Nomad client uses a custom `plugin_dir` in `nomad.hcl`, place it there instead.

## Nomad client configuration

Add a `plugin` block to your Nomad client config (`/etc/nomad.d/nomad.hcl` or similar):

```hcl
plugin "oci-chroot" {
  config {
    # Required: enable the driver.
    enabled = true

    # Optional: use host buildah binary instead of the embedded OCI puller.
    # When false (default), images are pulled internally using
    # go-containerregistry. Set to true if buildah is installed on the
    # host and you prefer to use it.
    host_buildah = false
  }
}
```

Then restart the Nomad client:

```sh
sudo systemctl restart nomad
```

Verify the driver was registered by checking Nomad logs or running:

```sh
nomad node status -self -verbose | grep oci-chroot
```

## Job specification

Use the driver in a Nomad job as follows:

```hcl
job "example" {
  datacenters = ["dc1"]

  group "example" {
    task "example" {
      driver = "oci-chroot"

      config {
        image        = "alpine:latest"
        command      = "/bin/sh"
        args         = ["-c", "echo hello"]
        bind_sockets = []                  # optional, host sockets to mount
        force_pull   = false               # optional, always re-pull image
      }

      resources {
        cpu    = 500
        memory = 512
      }
    }
  }
}
```

### Task config options

| Option         | Type           | Required | Description                                |
|----------------|----------------|----------|--------------------------------------------|
| `image`        | `string`       | yes      | OCI container image reference              |
| `command`      | `string`       | no       | Command to run (default `/bin/sh`)         |
| `args`         | `list(string)` | no       | Arguments to the command                   |
| `bind_sockets` | `list(string)` | no       | Host Unix socket paths to bind-mount inside |
| `force_pull`   | `bool`         | no       | Always re-pull the image on each run       |

## Image backends

### Embedded (default)

Uses `go-containerregistry` to pull and extract OCI images. No external dependencies.

### Host buildah

Set `host_buildah = true` in the plugin config. Requires `buildah` to be installed
on the Nomad client host. The driver will shell out to `buildah from` and
`buildah mount` to obtain the rootfs.
