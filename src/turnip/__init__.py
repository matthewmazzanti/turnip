"""turnip -- a persistent rootless container network: a routed (Cilium-style)
L3 network over podman netns.

`config` is the declarative model (loader + validator); `fabric` the legacy
literal model (being retired); `netns`/`nftlib` the mechanism; `verify` the
checks; `main` the `turnip {up|verify|down}` CLI.
"""
