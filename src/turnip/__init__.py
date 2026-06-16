"""turnip -- a persistent rootless container network: a routed (Cilium-style)
L3 network over podman netns.

`config` is the declarative model (pure pydantic: types + validation, no IO);
`netns`/`nftlib` the pure mechanism (explicit args, no env reads, no mutable
state); `main` the imperative shell -- it does the IO (config + env), resolves the
runtime, derives names, and drives `up`/`down`. Being rewritten config-driven one
milestone at a time (IMPLEMENTATION-PLAN.md); the old literal-driven mechanism is
parked in `*.py.bak` (`main`/`verify`) alongside the legacy `fabric` model.
"""
