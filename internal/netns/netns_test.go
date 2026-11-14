package netns

import (
	"testing"
)

func TestParseMainRouteTableIgnoresThroneTun(t *testing.T) {
	sample := `0.0.0.0/1 dev nekobox-tun scope link 
default via 172.19.0.1 dev tun0 proto static metric 50 
default via 192.168.1.1 dev enp34s0 proto dhcp src 192.168.1.3 metric 100 
128.0.0.0/1 dev nekobox-tun scope link 
192.168.1.0/24 dev enp34s0 proto kernel scope link src 192.168.1.3 metric 100 
`
	info, err := ParseMainRouteTable(sample, "auto", "auto")
	if err != nil {
		t.Fatalf("ParseMainRouteTable failed: %v", err)
	}
	if info.Interface != "enp34s0" {
		t.Errorf("Interface = %q, want enp34s0", info.Interface)
	}
	if info.Gateway != "192.168.1.1" {
		t.Errorf("Gateway = %q, want 192.168.1.1", info.Gateway)
	}
	if info.LocalIP != "192.168.1.3" {
		t.Errorf("LocalIP = %q, want 192.168.1.3", info.LocalIP)
	}
}

func TestParseMainRouteTableFallbackScopeLinkSrc(t *testing.T) {
	sample := `default via 10.0.0.1 dev eth1 proto static metric 100
10.0.0.0/24 dev eth1 proto kernel scope link src 10.0.0.88 metric 100
`
	info, err := ParseMainRouteTable(sample, "auto", "auto")
	if err != nil {
		t.Fatalf("ParseMainRouteTable failed: %v", err)
	}
	if info.Interface != "eth1" || info.Gateway != "10.0.0.1" || info.LocalIP != "10.0.0.88" {
		t.Fatalf("unexpected WANInfo: %+v", info)
	}
}
