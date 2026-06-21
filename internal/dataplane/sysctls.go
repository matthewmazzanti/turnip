package dataplane

import (
	"fmt"
	"os"
	"strings"
)

// RouterSysctls is the sysctl set for a router netns:
//
//   - ip_forward on (we route);
//   - all.rp_filter=0 so the per-veth values are authoritative (the kernel uses
//     max(conf.all, conf.<if>), and a fresh netns may not default all to 0);
//   - ipv6 disabled router-wide (the routed model has no L2 path between containers, so
//     killing v6 on the router severs inter-container v6);
//   - then per fabric veth: proxy_arp=1 (answer the gateway ARP / a future uplink) and
//     rp_filter=1 (STRICT -- the anti-spoof pin, paired with that veth's /32 route).
//
// Apply AFTER the veths exist (the per-veth conf.<if> dirs). The uplink veth's own
// rp_filter is added by the host edge, when that veth is created -- not here.
func RouterSysctls(routerIfs []string) map[string]string {
	s := map[string]string{
		"net.ipv4.ip_forward":                "1",
		"net.ipv4.conf.all.rp_filter":        "0",
		"net.ipv6.conf.all.disable_ipv6":     "1",
		"net.ipv6.conf.default.disable_ipv6": "1",
	}
	for _, rif := range routerIfs {
		s["net.ipv4.conf."+rif+".proxy_arp"] = "1"
		s["net.ipv4.conf."+rif+".rp_filter"] = "1"
	}
	return s
}

// WriteSysctls writes each `net.x.y = value` by translating the dotted key to its
// /proc/sys path. Interface names (hyphens, no dots) keep the dot->slash mapping
// unambiguous. There is no netlink verb for these, so this MUST run inside the target
// netns -- the caller wraps it in a set.Enter (setns) episode.
func WriteSysctls(sysctls map[string]string) error {
	for key, val := range sysctls {
		path := "/proc/sys/" + strings.ReplaceAll(key, ".", "/")
		if err := os.WriteFile(path, []byte(val), 0); err != nil {
			return fmt.Errorf("sysctl %s=%s: %w", key, val, err)
		}
	}
	return nil
}
