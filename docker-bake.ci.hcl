// CI-only overlay: enables shared registry cache hosted on ghcr.
// Invoked via `make push BAKE_FILES="-f docker-bake.ci.hcl"`.
//
// Was previously `type=gha` but the GHA cache backend's Azure-hosted endpoint
// has had recurring 4xx outages that buildkit can't gracefully degrade from
// (HTML response → JSON parse error → fatal solve failure). Registry cache
// runs on the same ghcr that already hosts the images, so cache failures
// surface the same way image push failures do.

target "_common" {
  cache-from = ["type=registry,ref=${IMAGE_BASE}/buildcache:cache"]
  cache-to   = ["type=registry,ref=${IMAGE_BASE}/buildcache:cache,mode=max,image-manifest=true,oci-mediatypes=true"]
}
