package ugcvisibility

import "testing"

func TestHidesAll(t *testing.T) {
	cases := map[string]bool{All: false, Local: false, None: true, "": false, "bogus": false}
	for policy, want := range cases {
		if got := HidesAll(policy); got != want {
			t.Errorf("HidesAll(%q) = %v, want %v", policy, got, want)
		}
	}
}

func TestHidesNote(t *testing.T) {
	remote := "remote.example"
	cases := []struct {
		name   string
		policy string
		host   *string
		want   bool
	}{
		{name: "all keeps remote", policy: All, host: &remote, want: false},
		{name: "all keeps local", policy: All, want: false},
		{name: "local hides remote", policy: Local, host: &remote, want: true},
		{name: "local keeps local", policy: Local, want: false},
		{name: "none hides local", policy: None, want: true},
		{name: "none hides remote", policy: None, host: &remote, want: true},
		{name: "unknown behaves like all", policy: "", host: &remote, want: false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := HidesNote(tc.policy, tc.host); got != tc.want {
				t.Errorf("HidesNote(%q, %v) = %v, want %v", tc.policy, tc.host, got, tc.want)
			}
		})
	}
}
