package eventbus

import "testing"

func TestSubject(t *testing.T) {
	cases := []struct {
		workspace, eventType, want string
		wantErr                    bool
	}{
		{"acme-analytics", "dashboard.created", "booth.acme-analytics.dashboard.created", false},
		{"acme-analytics", "dataset.written", "booth.acme-analytics.dataset.written", false},
		{"UPPERCASE", "dashboard.created", "", true},
		{"acme-analytics", "notdotted", "", true},
		{"", "dashboard.created", "", true},
	}

	for _, c := range cases {
		got, err := Subject(c.workspace, c.eventType)
		if c.wantErr {
			if err == nil {
				t.Errorf("Subject(%q, %q) expected error, got %q", c.workspace, c.eventType, got)
			}
			continue
		}
		if err != nil {
			t.Errorf("Subject(%q, %q) unexpected error: %v", c.workspace, c.eventType, err)
			continue
		}
		if got != c.want {
			t.Errorf("Subject(%q, %q) = %q, want %q", c.workspace, c.eventType, got, c.want)
		}
	}
}
