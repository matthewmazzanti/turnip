"""Scenario: container links -- host-netdev holes that bypass the routers (configs/links.json).

Hand-authored ground truth for the network links.json builds. Links are the
deliberate L2 trust escape: OUTSIDE every router's nft policy. The node provides
the borrowed anchors (the `br-lan` bridge, the `net-phys` dummy) declaratively.

NOTE on phys: `net-phys` is a DUMMY standing in for a movable NIC. The move / rename /
borrow path (device leaves init, lands renamed + addressed in the container) is
identical to a real NIC; only the kernel's return-to-init on netns destroy differs (a
dummy is reaped, a real NIC returns) -- relied-upon kernel behaviour, not turnip code,
so it isn't asserted here.
"""

from probe import Checks, Probe

p = Probe()
c = Checks()

# veth->bridge: two containers share the br-lan L2 segment and reach each other with
# NO flow entry -- the policy bypass (a router network would drop this).
print("veth->bridge (shared L2 segment, outside nft policy):")
c.ok(p.addrs("br1", "eth0") == {"192.168.50.10/24"}, "br1 eth0 == 192.168.50.10/24")
c.ok(p.addrs("br2", "eth0") == {"192.168.50.11/24"}, "br2 eth0 == 192.168.50.11/24")
with p.listener("br2", 9000):
    c.ok(p.connects("br1", "192.168.50.11", 9000), "br1 -> br2:9000 over br-lan (no flow needed)")

# veth->host: point-to-point into the root netns; turnip leaves the host end bare (the
# operator routes to it), so we assert the container address + the host end's presence.
print("veth->host (point-to-point; host end bare in init):")
c.ok(p.addrs("p2p", "eth0") == {"10.9.0.2/30"}, "p2p eth0 == 10.9.0.2/30")
c.ok(p.init_iface_exists("vethL-p2p-eth0"), "p2p host end vethL-p2p-eth0 present in init")

# phys: the existing host device is MOVED in, renamed to the configured name, and is
# BORROWED -- gone from init while the container holds it.
print("phys (moved in, renamed, borrowed):")
c.ok(p.addrs("ph", "eth9") == {"192.168.9.10/24"}, "ph eth9 == 192.168.9.10/24 (renamed)")
c.ok(not p.init_iface_exists("net-phys"), "net-phys gone from init (moved into ph)")

c.done()
