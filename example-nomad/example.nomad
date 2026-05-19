# oci-chroot driver example - runs a task inside an OCI image's filesystem
# with socket passthrough for docker build

job "oci-example" {
  datacenters = ["dc1"]

  # Tell Nomad to use the host network so Docker has access
  group "build" {
    network {
      mode = "host"
    }

    task "docker-builder" {
      driver = "oci-chroot"

      config {
        image        = "ghcr.io/vaelatern/temporal-cicd/builder:master"
        command      = "/bin/sh"
        args         = ["-c", "docker info && docker build --help"]
        bind_sockets = ["/var/run/docker.sock"]
      }

      resources {
        cpu    = 2000
        memory = 2048
      }
    }
  }
}
