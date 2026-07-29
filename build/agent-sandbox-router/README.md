# Agent Sandbox router image

Agent Sandbox v0.5.3 contains the supported Go router, but does not publish a
router image. This build packages that upstream source without copying or
forking it into Sandherd.

The source commit, source archive checksum, Go builder, and Dockerfile frontend
are pinned. The runtime is a non-root scratch image containing only the static
router binary.

Build and push it to a registry reachable by the target cluster:

```sh
AGENT_SANDBOX_ROUTER_IMAGE=registry.example.com/sandherd/agent-sandbox-router:v0.5.3 \
  make agent-sandbox-router-container

docker push registry.example.com/sandherd/agent-sandbox-router:v0.5.3
```

Pass that complete image reference to the installation script as
`ROUTER_IMAGE`. Publishing Sandherd images is intentionally left to the release
pipeline; the platform installation does not assume a registry or credentials.
