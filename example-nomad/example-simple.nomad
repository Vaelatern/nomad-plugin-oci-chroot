job "oci-simple" {
  datacenters = ["dc1"]

  group "test" {
    task "basic" {
      driver = "oci-chroot"

      config {
        image   = "ghcr.io/vaelatern/temporal-cicd/builder:master"
        command = "/bin/sh"
        args    = ["-c", "echo HELLO && /bin/busybox ls / 2>&1"]
      }

      resources {
        cpu    = 500
        memory = 512
      }
    }
  }
}
