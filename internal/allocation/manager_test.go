package allocation

import (
	"net"
	"testing"
)

func TestAllocateRejectsOccupiedUDPPort(t *testing.T) {
	packet, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer packet.Close()
	item := Allocation{IP: "127.0.0.1", Port: packet.LocalAddr().(*net.UDPAddr).Port}
	manager := NewManager([]Allocation{item})
	if _, err := manager.Allocate(); err == nil {
		t.Fatal("allocated a port with an existing UDP listener")
	}
}

func TestReservationConflictIsAtomic(t *testing.T) {
	old := Allocation{IP: "127.0.0.1", Port: 34001}
	other := Allocation{IP: "127.0.0.1", Port: 34002}
	manager := NewManager([]Allocation{old, other})
	manager.ReserveMany([]Allocation{old, other})
	if err := manager.ReplaceCellReservations([]Allocation{old}, []Allocation{other}); err == nil {
		t.Fatal("accepted another Cell's reservation")
	}
	if !manager.IsReserved(old) || !manager.IsReserved(other) {
		t.Fatal("failed operation lost reservations")
	}
}
