"""Scenario: the host edge -- egress (configs/uplink.json), against the `world` node.

`world` runs a persistent listener on :8888. Default-deny across the uplink: only a
container with `egress` may initiate out; everything else is dropped at the router.
(Ingress DNAT is driven at the testScript level -- it originates on `world`.)
"""

from probe import Checks, Probe

p = Probe()
c = Checks()

print("uplink egress (default-deny; only `egress` containers reach out):")
c.ok(p.has_default_via("out", "10.0.0.1"), "out default via 10.0.0.1")
c.ok(p.connects("out", "192.168.1.2", 8888), "out -> world:8888 ALLOWED (has egress)")
c.ok(not p.connects("quiet", "192.168.1.2", 8888), "quiet -> world:8888 DENIED (no egress)")

c.done()
