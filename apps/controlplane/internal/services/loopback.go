// Package services allocates stable per-service loopback IPs from
// 127.77.0.0/16: fnv32a(service_id) mapped into 127.77.1.1–127.77.254.254
// (skipping .0/.255 octets), then linear probing until unused in the group.
package services

import (
	"fmt"
	"hash/fnv"
	"net"
)

const (
	// usable (octet3,octet4) pairs: 254*254
	space = 254 * 254
)

// addrAt maps index i in [0,space) to 127.77.(1+i/254).(1+i%254).
func addrAt(i int) string {
	return fmt.Sprintf("127.77.%d.%d", 1+i/254, 1+i%254)
}

// Allocate returns an unused loopback IP for serviceID. inUse reports which
// IPs are taken within the group. Deterministic for the same inputs.
func Allocate(serviceID string, inUse func(ip string) bool) (string, error) {
	h := fnv.New32a()
	_, _ = h.Write([]byte(serviceID))
	start := int(h.Sum32() % uint32(space))
	for k := 0; k < space; k++ {
		ip := addrAt((start + k) % space)
		if !inUse(ip) {
			return ip, nil
		}
	}
	return "", fmt.Errorf("loopback address space exhausted")
}

// Valid reports whether ip is inside the allocatable range.
func Valid(ip string) bool {
	parsed := net.ParseIP(ip)
	if parsed == nil {
		return false
	}
	v4 := parsed.To4()
	if v4 == nil || v4[0] != 127 || v4[1] != 77 {
		return false
	}
	return v4[2] >= 1 && v4[2] <= 254 && v4[3] >= 1 && v4[3] <= 254
}
