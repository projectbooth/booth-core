package config

import "testing"

func TestQualifyNATSURL(t *testing.T) {
	cases := []struct{ in, ns, want string }{
		{"nats://booth-core-nats:4222", "booth-system", "nats://booth-core-nats.booth-system.svc.cluster.local:4222"},
		{"nats://booth-core-nats", "booth-system", "nats://booth-core-nats.booth-system.svc.cluster.local"},
		{"nats://nats.example.com:4222", "booth-system", "nats://nats.example.com:4222"},
		{"nats://booth-core-nats.other.svc:4222", "booth-system", "nats://booth-core-nats.other.svc:4222"},
		{"::not a url", "booth-system", "::not a url"},
	}
	for _, c := range cases {
		if got := qualifyNATSURL(c.in, c.ns); got != c.want {
			t.Errorf("qualifyNATSURL(%q, %q) = %q, want %q", c.in, c.ns, got, c.want)
		}
	}
}
