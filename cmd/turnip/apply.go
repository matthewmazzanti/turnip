// apply.go -- the imperative driver: walk a Plan and push it to the live netns Set. The
// inverse of model.go's pure lowering -- this is where the fds, the set.Enter setns episodes,
// the dataplane effectful primitives, and the progress output live. apply is total over a
// valid Plan: every fallible resolution already happened in buildModel, so the only errors
// here are real runtime/IO faults.

package main

import (
	"fmt"
	"os"

	"git.lan/mmazzanti/turnip/internal/config"
	dp "git.lan/mmazzanti/turnip/internal/dataplane"
	"git.lan/mmazzanti/turnip/internal/netns"
	"git.lan/mmazzanti/turnip/internal/nftlib"
)

// applyPlan pushes a fully-resolved Plan onto the freshly-bootstrapped Set: loopback up in
// every netns, then the per-network routed fabric, then the per-container local setup.
func applyPlan(set *netns.Set, plan *Plan) error {
	// loopback up in every netns (routers + containers); all fds are present post-Bootstrap.
	for _, sp := range plan.Specs {
		fd, _ := set.FD(sp.Name)
		if err := dp.SetLoUp(fd); err != nil {
			return fmt.Errorf("%s lo: %w", sp.Name, err)
		}
	}
	for _, np := range plan.Networks {
		if err := applyNetwork(set, np); err != nil {
			return err
		}
	}
	for _, cp := range plan.Containers {
		if err := applyContainer(set, plan.Owner, cp); err != nil {
			return err
		}
	}
	return nil
}

// applyNetwork wires one network's routed fabric in its router netns: the gateway, the routed
// veths into attached containers, the optional host edge, then the sysctls and nft policy
// (built from the plan's resolved inputs and applied last, after the veths they reference
// exist). Drives the live Set the rootful parent holds (netlink/nft over the fd, set.Enter for
// the per-process-netns bits).
func applyNetwork(set *netns.Set, np NetworkPlan) error {
	routerFd, ok := set.FD(np.Router)
	if !ok {
		return fmt.Errorf("router netns %q missing from the bootstrap set", np.Name)
	}
	if err := dp.CreateGateway(routerFd, np.Gateway); err != nil {
		return fmt.Errorf("network %q: %w", np.Name, err)
	}
	fmt.Printf("  router %s: gateway %s/%d on %s\n", np.Name, np.Gateway.Addr, config.HOSTPrefix, np.Gateway.IfName)

	var routerIfs []string
	for _, ep := range np.Endpoints {
		contFd, ok := set.FD("container:" + ep.Container)
		if !ok {
			return fmt.Errorf("container netns %q missing from the bootstrap set", ep.Container)
		}
		if err := dp.Connect(routerFd, contFd, np.Gateway.Addr, ep.Endpoint); err != nil {
			return fmt.Errorf("network %q connect %q: %w", np.Name, ep.Container, err)
		}
		routerIfs = append(routerIfs, ep.Endpoint.RouterIf)
		fmt.Printf("    %s: %s %s/%d -> gw %s%s <-> %s\n",
			ep.Container, ep.Endpoint.ContIf, ep.Endpoint.IP, config.HOSTPrefix,
			np.Gateway.Addr, defaultMark(ep.Endpoint.Default), ep.Endpoint.RouterIf)
	}

	// uplink (the host edge): the /31 veth across init<->router + host NAT. Wired before the
	// sysctls/nft so the uplink veth exists when they reference it (its rp_filter + egress allows).
	uplinkRouterIf := ""
	var edge *dp.Edge
	if np.Uplink != nil {
		u := np.Uplink
		if err := dp.HostEdgeConnect(routerFd, u.Uplink); err != nil {
			return fmt.Errorf("network %q uplink: %w", np.Name, err)
		}
		if err := dp.ConfigureHostNAT(np.Name, u.Uplink, u.ContainerIPs, u.DNATs); err != nil {
			return fmt.Errorf("network %q host nat: %w", np.Name, err)
		}
		uplinkRouterIf = u.Uplink.RouterIf
		edge = &u.Edge
		fmt.Printf("    uplink: %s <-> %s (%s/%d), host masquerade + %d route(s) + %d dnat\n",
			u.Uplink.HostIf, u.Uplink.RouterIf, u.Uplink.HostIP, config.LINKPrefix, len(u.ContainerIPs), len(u.DNATs))
	}

	// sysctls: applied AFTER the veths exist (the per-veth conf.<if> dirs). /proc/sys is
	// per-process-netns with no netlink verb, so it needs a setns episode (set.Enter).
	sysctls := dp.RouterSysctls(routerIfs, uplinkRouterIf)
	if err := set.Enter(np.Router, func() error { return dp.WriteSysctls(sysctls) }); err != nil {
		return fmt.Errorf("network %q sysctls: %w", np.Name, err)
	}
	fmt.Printf("    sysctls: ip_forward + per-veth proxy_arp/rp_filter (strict) + ipv6 off\n")

	// nft acts on the process netns, so apply it inside a set.Enter episode: the forked nft
	// child inherits the router netns.
	rs := dp.BuildNFT(np.Flows, edge)
	if err := set.Enter(np.Router, func() error { return nftlib.Load(rs) }); err != nil {
		return fmt.Errorf("network %q nft: %w", np.Name, err)
	}
	fmt.Printf("    nft: forward flow matrix (%d flow(s)) + input lockdown\n", len(np.Flows))
	return nil
}

// applyContainer writes a container's generated /etc/hosts (chowned to the owner so podman
// bind-mounts it to /etc/hosts cleanly) and connects its links.
func applyContainer(set *netns.Set, owner netns.Owner, cp ContainerPlan) error {
	if err := os.WriteFile(cp.HostsPath, []byte(cp.Hosts), 0o644); err != nil {
		return fmt.Errorf("container %q hosts: %w", cp.Name, err)
	}
	if err := os.Chown(cp.HostsPath, owner.UID, owner.GID); err != nil {
		return fmt.Errorf("container %q hosts chown: %w", cp.Name, err)
	}
	if len(cp.Links) > 0 {
		contFd, ok := set.FD(cp.Netns)
		if !ok {
			return fmt.Errorf("container netns %q missing from the bootstrap set", cp.Name)
		}
		for _, spec := range cp.Links {
			if err := dp.LinkConnect(contFd, spec); err != nil {
				return err
			}
		}
		fmt.Printf("  container %s: hosts written + %d link(s)\n", cp.Name, len(cp.Links))
		return nil
	}
	fmt.Printf("  container %s: hosts written\n", cp.Name)
	return nil
}

// defaultMark is the " (default)" suffix for the per-endpoint progress line when it owns the
// container's default route.
func defaultMark(d bool) string {
	if d {
		return " (default)"
	}
	return ""
}
