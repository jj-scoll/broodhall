package main

import "testing"

// Ways a Bash command can reach a credential file without a plain "cat .env", and ordinary
// commands that must keep their output.
func TestCaptureBashEvasionsAndFalsePositives(t *testing.T) {
	dropped := []string{
		`cat "$HOME/.ssh/id_rsa"`,
		`cat ~/.ssh/id_*`,
		`cat .e[n]v`,
		`cat .en?`,
		`sed -n 1p ./.env`,
		`tail -f /work/app/../app/.env`,
		`head -c 100 <.env`,
		`cp .env /tmp/x && cat /tmp/x`,
		`set -a; . ./.env; env`,
		`source .env && printenv`,
		`while read l; do echo $l; done < .env.local`,
		`grep -r KEY --include=.env .`,
		`cat ~/.config/gh/hosts.yml`,
		`cat ~/.kube/config`,
		`scp host:.env .`,
		`cat {.env,x}`,
		`openssl x509 -in server.pem -text`,
		`terraform plan -var-file=prod.tfvars`,
	}
	kept := []string{
		`ls -la`,
		`go build ./... && go test ./...`,
		`git commit -m "update docs"`,
		`cat README.md`,
		`cat ~/.ssh/config`,
		`cat ~/.ssh/known_hosts`,
		`cat .env.example`,
		`grep -rn "environment" .`,
		`env | sort`,
		`kubectl get pods -A`,
		`aws s3 ls s3://bucket/credentials-docs/`,
		`cat /work/app/dotenv.go`,
	}
	run := func(cmd string) string {
		_, out, _ := captureToolCall(capCfg(), capProjectCwd, "Bash", `{"command":`+jsonStr(cmd)+`}`, capOutputBody, false)
		return out
	}
	for _, c := range dropped {
		if out := run(c); out == capOutputBody {
			t.Errorf("%q: output kept, want dropped", c)
		}
	}
	for _, c := range kept {
		if out := run(c); out != capOutputBody {
			t.Errorf("%q: output dropped, want kept", c)
		}
	}
}
