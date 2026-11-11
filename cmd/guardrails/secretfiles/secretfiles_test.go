package secretfiles

import "testing"

func TestMatch(t *testing.T) {
	const home = "/home/u"
	tests := []struct {
		path string
		want bool
	}{
		{"/home/u/.aws/credentials", true},
		{"/home/u/.aws/sso/cache/abc.json", true},
		{"/home/u/.ssh/id_ed25519", true},
		{"/home/u/.ssh/id_ed25519.pub", false},
		{"/home/u/.ssh/known_hosts", false},
		{"/home/u/.kube/config", true},
		{"/work/app/.env", true},
		{"/work/app/.env.production", true},
		{"/work/app/.env.example", false},
		{"/work/app/prod.tfvars", true},
		{"/work/app/prod.tfvars.example", false},
		{"/work/app/key.pem", true},
		{"/home/u/.claude/.credentials.json", true},
		{"/home/u/.claude-work/.credentials.json", true},
		{"/work/app/.credentials.json", false},
		{"/work/app/main.go", false},
		{"/work/app/environment.md", false},
	}
	for _, tt := range tests {
		if got := Match(tt.path, home); got != tt.want {
			t.Errorf("Match(%q) = %v, want %v", tt.path, got, tt.want)
		}
	}
}
