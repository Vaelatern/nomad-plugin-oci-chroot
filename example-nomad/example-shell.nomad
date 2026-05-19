# Minimal example: interactive shell inside the chroot
job "oci-shell" {
  datacenters = ["dc1"]

  group "debug" {
    network {
      mode = "host"
    }

    task "shell" {
      driver = "oci-chroot"

      config {
        image        = "ghcr.io/vaelatern/temporal-cicd/builder:master"
        command      = "/bin/sh"
        bind_sockets = ["/var/run/docker.sock"]
      }

      resources {
        cpu    = 500
        memory = 512
      }
    }
  }
}
