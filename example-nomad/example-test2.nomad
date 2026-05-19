job "oci-test2" {
  datacenters = ["dc1"]

  group "test" {
    task "basic" {
      driver = "oci-chroot"

      config {
        image   = "ghcr.io/vaelatern/temporal-cicd/builder:master"
        command = "/bin/sh"
        args    = ["-c", "echo PATH=\\$PATH && env"]
      }

      resources {
        cpu    = 500
        memory = 512
      }
    }
  }
}
