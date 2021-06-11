package networkpolicy

import (
	"net"

	v1 "k8s.io/api/core/v1"
)

func getPodIPs(podStatus v1.PodStatus) []string {
	addresses := []string{}

	for _, podIP := range podStatus.PodIPs {
		addresses = append(addresses, podIP.String())
	}

	return addresses
}

func IPtoIPNet(ip net.IP) *net.IPNet {
	maskLen := 128
	if ip.To4() != nil {
		maskLen = 32
	}
	return &net.IPNet{
		IP:   ip,
		Mask: net.CIDRMask(maskLen, maskLen),
	}
}
