variable "IMAGE_BASE" {
  default = "ghcr.io/ruddervirt/aileron"
}

variable "SHA_TAG" {
  default = "dev"
}

# Whether to also move the floating ":latest" tag. Main-branch CI sets this so
# latest tracks the tip of main; release builds (tagged vX.Y.Z) leave it false
# so a release never silently moves latest.
variable "MOVE_LATEST" {
  default = "false"
}

function "tags_for" {
  params = [suffix, tag]
  result = MOVE_LATEST == "true" ? [
    "${IMAGE_BASE}${suffix}:${tag}",
    "${IMAGE_BASE}${suffix}:latest",
    ] : [
    "${IMAGE_BASE}${suffix}:${tag}",
  ]
}

target "_common" {
  context    = "."
  dockerfile = "Dockerfile"
}

group "default" {
  targets = [
    "manager",
    "coordinator",
    "aileron-ui",
    "vncgateway",
    "egress-bridge",
    "helper",
    "sidecar",
    "grader",
  ]
}

target "manager" {
  inherits = ["_common"]
  target   = "manager"
  tags     = tags_for("", SHA_TAG)
}

target "coordinator" {
  inherits = ["_common"]
  target   = "coordinator"
  tags     = tags_for("/coordinator", SHA_TAG)
}

target "aileron-ui" {
  inherits = ["_common"]
  target   = "aileron-ui"
  tags     = tags_for("/aileron-ui", SHA_TAG)
}

target "egress-bridge" {
  inherits = ["_common"]
  target   = "egress-bridge"
  tags     = tags_for("/egress-bridge", SHA_TAG)
}

target "helper" {
  inherits = ["_common"]
  target   = "helper"
  tags     = tags_for("/helper", SHA_TAG)
}

target "sidecar" {
  inherits = ["_common"]
  target   = "sidecar"
  tags     = tags_for("/sidecar", SHA_TAG)
}

# Grader worker — core (aileron) image, tagged with the aileron SHA_TAG.
target "grader" {
  inherits = ["_common"]
  target   = "grader"
  tags     = tags_for("/grader", SHA_TAG)
}

# Core (aileron) VNC gateway: the merged Go gateway+bridge, tagged with the
# aileron SHA_TAG. guacd is an upstream sidecar image, not built here.
target "vncgateway" {
  inherits = ["_common"]
  target   = "vncgateway"
  tags     = tags_for("/vncgateway", SHA_TAG)
}
