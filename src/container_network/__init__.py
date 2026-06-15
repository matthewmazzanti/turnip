"""container_network -- a persistent rootless container network: a routed
(Cilium-style) L3 fabric over podman netns.

`config` is the declarative model (loader + validator); `fabric` the legacy
literal model; `netns`/`nftlib` the mechanism; `verify` the checks; `main` the
`fabric {up|verify|down}` CLI.
"""
