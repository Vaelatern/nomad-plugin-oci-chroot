job "oci-path" {
  datacenters = ["dc1"]

  group "test" {
    task "basic" {
      driver = "oci-chroot"

      config {
        image   = "ghcr.io/vaelatern/temporal-cicd/builder:master"
        command = "/bin/sh"
        args    = ["-c", "PATH=/usr/bin:/bin:/usr/local/bin ls /"]
      }

      resources {
        cpu    = 500
        memory = 512
      }
    }
  }
}
