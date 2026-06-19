"""Scenario: a single routed network with one directional flow (configs/router.json).

The expected properties below are HAND-AUTHORED ground truth -- the network this
config is meant to build -- not derived from turnip's model. The driver runs
`turnip up` on the config, runs this script, then `turnip down`; a nonzero exit
fails the scenario.

The flow matrix is the point: `zwave -> hass:443` is the one declared flow, so it
must connect, and everything else must be DROPPED at the router (a timeout against
a LIVE listener -- proving the SYN was dropped, not merely refused).
"""

from probe import Checks, Probe

p = Probe()
c = Checks()

# --- structural: each container's /32 address + default route (literal expectations)
print("structural:")
c.ok(p.addrs("zwave", "eth0") == {"10.0.0.11/32"}, "zwave eth0 == 10.0.0.11/32")
c.ok(p.addrs("hass", "eth0") == {"10.0.0.12/32"}, "hass eth0 == 10.0.0.12/32")
c.ok(p.addrs("proxy", "eth0") == {"10.0.0.13/32"}, "proxy eth0 == 10.0.0.13/32")
c.ok(p.has_default_via("zwave", "10.0.0.1"), "zwave default via 10.0.0.1")
c.ok(p.has_default_via("proxy", "10.0.0.1"), "proxy default via 10.0.0.1")

# --- reachability: the directional flow matrix, against live listeners
print("reachability:")
with p.listener("hass", 443), p.listener("proxy", 443):
    c.ok(p.connects("zwave", "10.0.0.12", 443), "zwave -> hass:443 ALLOWED (the one flow)")
    c.ok(not p.connects("zwave", "10.0.0.13", 443), "zwave -> proxy:443 DROPPED (no flow)")
    c.ok(not p.connects("proxy", "10.0.0.12", 443), "proxy -> hass:443 DROPPED (directional)")

c.done()
