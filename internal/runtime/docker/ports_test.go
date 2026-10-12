package docker

import (
	"github.com/docker/go-connections/nat"
	"hivepanel-worker/internal/allocation"
	hiveruntime "hivepanel-worker/internal/runtime"
	"testing"
)

func TestAllAllocationsPublishTCPAndUDP(t *testing.T) {
	cell := hiveruntime.RuntimeCell{Allocations: []allocation.Allocation{
		{IP: "0.0.0.0", Port: 7777}, {IP: "127.0.0.1", Port: 27015},
		{IP: "0.0.0.0", Port: 7777},
	}}
	exposed, bindings, err := allocationPorts(cell)
	if err != nil {
		t.Fatal(err)
	}
	if len(exposed) != 4 || len(bindings) != 4 {
		t.Fatalf("expected four protocol/port pairs: %v", bindings)
	}
	for _, item := range cell.Allocations[:2] {
		for _, protocol := range []string{"tcp", "udp"} {
			port, _ := nat.NewPort(protocol, map[int]string{7777: "7777", 27015: "27015"}[item.Port])
			if len(bindings[port]) != 1 || bindings[port][0].HostIP != item.IP {
				t.Fatalf("missing binding %s: %v", port, bindings)
			}
		}
	}
}

func TestLegacyPrimaryAndInvalidPorts(t *testing.T) {
	_, bindings, err := allocationPorts(hiveruntime.RuntimeCell{AllocationIP: "::", AllocationPort: 25565})
	if err != nil || len(bindings) != 2 {
		t.Fatalf("legacy primary: %v %v", bindings, err)
	}
	for _, port := range []int{0, -1, 65536} {
		if _, _, err := allocationPorts(hiveruntime.RuntimeCell{AllocationIP: "0.0.0.0", AllocationPort: port}); err == nil {
			t.Fatalf("accepted invalid port %d", port)
		}
	}
}
