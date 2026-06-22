package dataplane

import (
	"fmt"
	"os"
	"strings"
)

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
