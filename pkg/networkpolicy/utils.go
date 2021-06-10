package networkpolicy

import (
	v1 "k8s.io/api/core/v1"
)

func getPodIPs(podStatus v1.PodStatus) []string {
	addresses := []string{}

	for _, podIP := range podStatus.PodIPs {
		addresses = append(addresses, podIP.String())
	}

	return addresses
}
