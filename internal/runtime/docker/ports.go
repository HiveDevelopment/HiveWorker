package docker

import (
	"errors"
	"fmt"
	"github.com/docker/go-connections/nat"
	"hivepanel-worker/internal/allocation"
	hiveruntime "hivepanel-worker/internal/runtime"
	"net"
)

func allocationPorts(cell hiveruntime.RuntimeCell) (nat.PortSet, nat.PortMap, error) {
	allocations := cell.Allocations
	if len(allocations) == 0 {
		allocations = []allocation.Allocation{{IP: cell.AllocationIP, Port: cell.AllocationPort}}
	}
	exposed := nat.PortSet{}
	bindings := nat.PortMap{}
	seen := map[string]bool{}
	for _, item := range allocations {
		if net.ParseIP(item.IP) == nil || item.Port < 1 || item.Port > 65535 {
			return nil, nil, errors.New("invalid container allocation")
		}
		for _, protocol := range []string{"tcp", "udp"} {
			port := nat.Port(fmt.Sprintf("%d/%s", item.Port, protocol))
			key := fmt.Sprintf("%s:%d/%s", item.IP, item.Port, protocol)
			if seen[key] {
				continue
			}
			seen[key] = true
			exposed[port] = struct{}{}
			bindings[port] = append(bindings[port], nat.PortBinding{HostIP: item.IP, HostPort: fmt.Sprint(item.Port)})
		}
	}
	return exposed, bindings, nil
}
