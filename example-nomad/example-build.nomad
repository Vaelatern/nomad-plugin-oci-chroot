job "oci-build" {
  datacenters = ["dc1"]
  type        = "batch"

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
        args         = ["-c", "echo '=== BUILD START ===' && mkdir -p /tmp/build && cd /tmp/build && printf 'FROM alpine:latest\nCMD echo hello\n' > Dockerfile && docker build -t test . && docker run --rm test && echo '=== BUILD DONE ==='"]
        bind_sockets = ["/var/run/docker.sock"]
        force_pull   = false
      }

      resources {
        cpu    = 500
        memory = 512
      }
    }
  }
}
