job "oci-test" {
  datacenters = ["dc1"]

  group "test" {
    task "basic" {
      driver = "oci-chroot"

      config {
        image   = "ghcr.io/vaelatern/temporal-cicd/builder:master"
        command = "/bin/sh"
        args    = ["-c", "echo '=== TEST ===' && ls /usr/bin/docker /bin/docker 2>&1 && which docker && docker version 2>&1 | head -5 && echo '=== DONE ==='"]
      }

      resources {
        cpu    = 500
        memory = 512
      }
    }
  }
}
