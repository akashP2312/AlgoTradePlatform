package app

import (
	"fmt"
	"net"
	"os"
	"strings"
)

// PrivateAddr refuses to bind anything but loopback or the Tailscale range
// (100.64.0.0/10), so no page or counter ever faces the internet. Inside a
// container (PMT_CONTAINER=1) the unspecified address is also allowed: the
// compose file publishes the port on 127.0.0.1 of the host only.
func PrivateAddr(addr string) error {
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		return err
	}
	if strings.EqualFold(host, "localhost") {
		return nil
	}
	ip := net.ParseIP(host)
	if host == "" {
		ip = net.IPv4zero
	}
	_, cgnat, _ := net.ParseCIDR("100.64.0.0/10")
	switch {
	case ip == nil:
	case ip.IsLoopback(), cgnat.Contains(ip):
		return nil
	case ip.IsUnspecified() && os.Getenv("PMT_CONTAINER") == "1":
		return nil
	}
	return fmt.Errorf("%s: bind to loopback or a Tailscale address only", addr)
}
