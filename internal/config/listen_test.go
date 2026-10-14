package config

import (
	"strings"
	"testing"
)

func TestListenAddressValidated(t *testing.T) {
	for _, tc := range []struct {
		addr string
		ok   bool
	}{
		{"[::]:8080", true}, {"0.0.0.0:8080", true}, {":8080", true}, {"127.0.0.1:18080", true}, {"localhost:80", true},
		{"[:]:8080", false}, {"8080", false}, {"[::]:http2", false}, {"[::]:70000", false},
	} {
		c := Default()
		c.Asterisk.ARIPassword, c.Asterisk.AMIPassword = "x", "x"
		c.HTTP.Listen = tc.addr
		err := c.validate()
		if (err == nil) != tc.ok {
			t.Errorf("%q: %v", tc.addr, err)
		}
		if err != nil && !strings.Contains(err.Error(), "VOIPD_HTTP_LISTEN") {
			t.Errorf("%q: error does not name the variable: %v", tc.addr, err)
		}
	}
}
