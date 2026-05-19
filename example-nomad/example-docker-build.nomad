job "oci-docker" {
  datacenters = ["dc1"]

  group "test" {
    restart {
      attempts = 0
      mode     = "fail"
    }

    task "build" {
      driver = "oci-chroot"

      config {
        image        = "ghcr.io/vaelatern/temporal-cicd/builder:master"
        command      = "/bin/sh"
        args         = ["-c", "echo '=== BUILD START ===' && docker version && echo '=== BUILD DONE ==='"]
        bind_sockets = ["/var/run/docker.sock"]
      }

      resources {
        cpu    = 500
        memory = 512
      }
    }
  }
}
