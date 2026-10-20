package app

import "testing"

func TestPrivateAddr(t *testing.T) {
	for addr, ok := range map[string]bool{
		"127.0.0.1:8090": true, "localhost:8090": true, "[::1]:8090": true, "100.101.2.3:8090": true,
		"0.0.0.0:8090": false, ":8090": false, "192.168.1.5:8090": false, "8.8.8.8:80": false, "nohost": false,
	} {
		if err := PrivateAddr(addr); (err == nil) != ok {
			t.Errorf("%s: %v", addr, err)
		}
	}
	t.Setenv("PMT_CONTAINER", "1")
	if err := PrivateAddr(":8090"); err != nil {
		t.Error(err)
	}
}
