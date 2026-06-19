"""Scenario: macvlan + ipvlan children on real LANs (configs/linklan.json).

Each child sits directly on its parent's LAN segment and reaches the `world` node
WITHOUT traversing the host -- the link bypasses every router. macvlan and ipvlan use
separate parents (a device is a macvlan master XOR an ipvlan master). `world` listens
on :8888 on both LANs.
"""

from probe import Checks, Probe

p = Probe()
c = Checks()

print("macvlan child on the eth1 LAN reaches world directly:")
c.ok(p.addrs("mv", "lan0") == {"192.168.1.50/24"}, "mv lan0 == 192.168.1.50/24")
c.ok(p.link_kind("mv", "lan0") == "macvlan", "mv lan0 is a macvlan")
c.ok(p.connects("mv", "192.168.1.2", 8888), "mv -> world(192.168.1.2):8888 over the LAN")

print("ipvlan child on the eth2 LAN reaches world directly:")
c.ok(p.addrs("iv", "lan1") == {"192.168.2.50/24"}, "iv lan1 == 192.168.2.50/24")
c.ok(p.link_kind("iv", "lan1") == "ipvlan", "iv lan1 is an ipvlan")
c.ok(p.connects("iv", "192.168.2.2", 8888), "iv -> world(192.168.2.2):8888 over the LAN")

c.done()
