package dataplane

import (
	"fmt"
	"os"
	"strings"
)

// Sysctl is one `net.x.y = Val` write. The set is an ORDERED slice, not a map, because order
// matters: net.ipv4.ip_forward is "special" -- writing it re-derives the per-interface RFC1812
// router defaults (send_redirects / accept_source_route flip toward enabled), so it must precede
// the conf.* pins that turn them back off. WriteSysctls applies the slice in order.
type Sysctl struct{ Key, Val string }

// Sys is a terse constructor for building ordered Sysctl lists: dp.Sys("net.ipv4.ip_forward", "1").
func Sys(key, val string) Sysctl { return Sysctl{Key: key, Val: val} }

// WriteSysctls writes each `net.x.y = Val` in order by translating the dotted key to its
// /proc/sys path. Interface names (hyphens, no dots) keep the dot->slash mapping
// unambiguous. There is no netlink verb for these, so this MUST run inside the target
// netns -- the caller wraps it in a set.Enter (setns) episode.
func WriteSysctls(sysctls []Sysctl) error {
	for _, s := range sysctls {
		path := "/proc/sys/" + strings.ReplaceAll(s.Key, ".", "/")
		if err := os.WriteFile(path, []byte(s.Val), 0); err != nil {
			return fmt.Errorf("sysctl %s=%s: %w", s.Key, s.Val, err)
		}
	}
	return nil
}
