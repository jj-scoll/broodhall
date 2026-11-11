package main

import (
	"path/filepath"
	"testing"
)

// Project-level and common credential files are protected by name, in any directory; obvious
// templates are not. The Python-opens-a-file case stays a documented limit of the guard.
func TestSecretDefaultsProjectFiles(t *testing.T) {
	env := secretEnv(t)
	for _, c := range []struct{ cmd, want string }{
		{"cat .env", "context:secret-file:mask"},
		{"grep KEY .env", "context:secret-file:mask"},
		{"cat sub/dir/.env", "context:secret-file:mask"},
		{"cat .env.local", "context:secret-file:mask"},
		{"head -3 .env.production", "context:secret-file:mask"},
		{"cat ~/.kube/config", "context:secret-file:mask"},
		{"cat .npmrc", "context:secret-file:mask"},
		{"cat ~/.pypirc", "context:secret-file:mask"},
		{"cat ~/.git-credentials", "context:secret-file:mask"},
		{"cat terraform.tfvars", "context:secret-file:mask"},
		{"cat prod.auto.tfvars", "context:secret-file:mask"},
		{"cat certs/server.pem", "context:secret-file:mask"},
		{"cat ~/.config/gcloud/application_default_credentials.json", "context:secret-file:mask"},
		{"cat ~/.config/gcloud/credentials.db", "context:secret-file:mask"},
		// Same behaviour as the existing paths: other tools ask, writes ask.
		{"cp .env /tmp/x", "ask:secret-file:cp"},
		{"base64 certs/server.pem", "ask:secret-file:base64"},
		{"echo K=v >> .env", "ask:secret-file:echo"},
		{"ls -la .env", "allow"},
		// Templates and near-misses pass.
		{"cat .env.example", "allow"},
		{"cat .env.sample", "allow"},
		{"cat .env.template", "allow"},
		{"cat terraform.tfvars.example", "allow"},
		{"cat .envrc", "allow"},
		{"cat environment.md", "allow"},
		{"cat main.tf", "allow"},
		{"cat notes.pem.txt", "allow"},
		// Documented limit: a script that opens the file is not seen.
		{`python3 -c "print(open('.env').read())"`, "allow"},
	} {
		if got := decide(env, c.cmd).Kind(); got != c.want {
			t.Errorf("%q: got %s, want %s", c.cmd, got, c.want)
		}
	}
	if got, ok := secretRewrite(env, "grep KEY .env", "CT"); !ok || got != "CT redact --secrets -- grep KEY .env" {
		t.Errorf("rewrite: %q %v", got, ok)
	}
	for _, f := range []string{".env", "app/.env.local", "x.tfvars", "k.pem", ".npmrc"} {
		if d, ok := secretFileToolDecision("Read", map[string]any{"file_path": filepath.Join(env.Cwd, f)}, env.Cwd, env.Home, ""); !ok || d.Exit != 2 {
			t.Errorf("Read %s: %+v %v", f, d, ok)
		}
		if d, ok := secretFileToolDecision("Write", map[string]any{"file_path": filepath.Join(env.Cwd, f)}, env.Cwd, env.Home, ""); !ok || !d.Ask {
			t.Errorf("Write %s: %+v %v", f, d, ok)
		}
	}
	for _, f := range []string{".env.example", "x.tfvars.example", "main.tf"} {
		if _, ok := secretFileToolDecision("Read", map[string]any{"file_path": filepath.Join(env.Cwd, f)}, env.Cwd, env.Home, ""); ok {
			t.Errorf("Read %s stopped", f)
		}
	}
}
