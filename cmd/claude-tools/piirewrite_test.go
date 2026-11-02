package main

import "testing"

func TestPIIRewrite(t *testing.T) {
	const r, rp = "CT redact --pii --private ", "CT redact --pii "
	tests := []struct{ name, in, want string }{
		// psql: force --csv so the header names the columns, then restore the asked format.
		{"psql -t -A", `psql -h localhost -U app -d db -t -A -c "select email from users"`,
			r + `--in=csv --to=unaligned --no-header -- psql -h localhost -U app -d db --csv -c "select email from users"`},
		{"psql grouped -tA", `psql -tA -c 'select 1'`, r + `--in=csv --to=unaligned --no-header -- psql --csv -c 'select 1'`},
		{"psql default aligned", `psql -c "select 1"`, r + `--in=csv --to=aligned --footer -- psql --csv -c "select 1"`},
		{"psql --csv as is", `psql --csv -c x`, r + `--in=csv -- psql --csv -c x`},
		{"psql -A -F quoted sep", `psql -A -F ',' -c x`, r + `--in=csv --to=unaligned --footer --sep=',' -- psql --csv -c x`},
		{"psql -F attached", `psql -t -A -F$'\t' -c x`, r + `--in=csv --to=unaligned --no-header --sep=$'\t' -- psql --csv -c x`},
		{"psql long flags", `psql --tuples-only --no-align --field-separator=';' -c x`,
			r + `--in=csv --to=unaligned --no-header --sep=';' -- psql --csv -c x`},
		{"psql -t aligned", `psql -t -c x`, r + `--in=csv --to=aligned --no-header -- psql --csv -c x`},
		{"psql grouped with -c last", `psql -X -h h -p 5432 -U u -d db -Atc "select 1"`,
			r + `--in=csv --to=unaligned --no-header -- psql -X -h h -p 5432 -U u -d db --csv -c "select 1"`},
		{"psql grouped keeps other letters", `psql -p 1 -d db -AtXc 'select 1'`,
			r + `--in=csv --to=unaligned --no-header -- psql -p 1 -d db --csv -Xc 'select 1'`},
		{"psql second group keeps its letters", `psql -t -qA -c x`, r + `--in=csv --to=unaligned --no-header -- psql --csv -q -c x`},
		{"psql -P pager is fine", `psql -P pager=off -c x`, r + `--in=csv --to=aligned --footer -- psql --csv -P pager=off -c x`},
		{"psql -P format overrides --csv: values only", `psql -P format=wrapped -c x`, r + `--in=text -- psql -P format=wrapped -c x`},
		{"rds-data formattedRecords as text", `aws rds-data execute-statement --sql x --format-records-as JSON --query formattedRecords --output text`,
			r + `--in=auto -- aws rds-data execute-statement --sql x --format-records-as JSON --query formattedRecords --output text --include-result-metadata`},
		// --private only where the output reaches the tool result: a piped, captured or
		// redirected call would hand the next reader <private> lines.
		{"env assignment and later pipe", `PGPASSWORD=x psql -c y | head -3`,
			`PGPASSWORD=x ` + rp + `--in=csv --to=aligned --footer -- psql --csv -c y | head -3`},
		{"inside && and $( )", `cd /x && n=$(psql -tAc "select count(*)") && echo $n`,
			`cd /x && n=$(` + rp + `--in=csv --to=unaligned --no-header -- psql --csv -c "select count(*)") && echo $n`},
		{"piped to jq", `duckdb -json -c x | jq .`, rp + `--in=json -- duckdb -json -c x | jq .`},
		{"block piped", `{ psql -c x; } | wc -l`, `{ ` + rp + `--in=csv --to=aligned --footer -- psql --csv -c x; } | wc -l`},
		{"process substitution", `diff <(psql -c a) f`, `diff <(` + rp + `--in=csv --to=aligned --footer -- psql --csv -c a) f`},
		{"stdout to file", `psql -c x > out.csv`, rp + `--in=csv --to=aligned --footer -- psql --csv -c x > out.csv`},
		{"stdout appended", `psql -c x 1>> out.csv`, rp + `--in=csv --to=aligned --footer -- psql --csv -c x 1>> out.csv`},
		{"stderr to file keeps wrap", `psql -c x 2> err.log`, r + `--in=csv --to=aligned --footer -- psql --csv -c x 2> err.log`},
		{"last pipe stage keeps wrap", `cat q.sql | duckdb -json`, `cat q.sql | ` + r + `--in=json -- duckdb -json`},
		{"two psql calls", `psql -c a; psql -c b`,
			r + `--in=csv --to=aligned --footer -- psql --csv -c a; ` + r + `--in=csv --to=aligned --footer -- psql --csv -c b`},
		// duckdb: every call that runs SQL, forced to -json; the default (agent) mode gets --cap.
		{"duckdb -csv on s3", `duckdb -csv -c "select * from 's3://acme-prod-clientdata/data/x.parquet'"`,
			r + `--in=json --to=csv -- duckdb -json -c "select * from 's3://acme-prod-clientdata/data/x.parquet'"`},
		{"duckdb -list -noheader -separator", `duckdb -list -noheader -separator ',' -c "from read_parquet('a.parquet')"`,
			r + `--in=json --to=unaligned --no-header --sep=',' -- duckdb -json -c "from read_parquet('a.parquet')"`},
		{"duckdb default mode", `duckdb -c "from 'x.csv'"`, r + `--in=json --to=markdown --cap -- duckdb -json -c "from 'x.csv'"`},
		{"duckdb db file", `duckdb clients.duckdb -c "select email from people"`,
			r + `--in=json --to=markdown --cap -- duckdb -json clients.duckdb -c "select email from people"`},
		{"duckdb own session db", `duckdb ~/.local/share/claude-sessions/sessions.duckdb -c "select count(*) from tool_calls"`,
			r + `--in=json --to=markdown --cap -- duckdb -json ~/.local/share/claude-sessions/sessions.duckdb -c "select count(*) from tool_calls"`},
		{"duckdb -f file", `duckdb -f q.sql`, r + `--in=json --to=markdown --cap -- duckdb -json -f q.sql`},
		{"duckdb stdin file", `duckdb < q.sql`, r + `--in=json --to=markdown --cap -- duckdb -json < q.sql`},
		{"duckdb -no-agent: no cap", `duckdb -no-agent -c x`, r + `--in=json --to=markdown -- duckdb -json -no-agent -c x`},
		{"duckdb -agent: cap", `duckdb -agent -c x`, r + `--in=json --to=markdown --cap -- duckdb -json -agent -c x`},
		{"duckdb -line on db file reading parquet", `duckdb lake.duckdb -line -c "from 'p/*.parquet'"`,
			r + `--in=json --to=line -- duckdb lake.duckdb -json -c "from 'p/*.parquet'"`},
		{"duckdb -json kept", `duckdb -json -c "from 'x.parquet'"`, r + `--in=json -- duckdb -json -c "from 'x.parquet'"`},
		{"duckdb -jsonlines kept", `duckdb -jsonlines -c "from 'x.parquet'"`, r + `--in=json -- duckdb -jsonlines -c "from 'x.parquet'"`},
		// aws: rds-data gets column names, Athena is masked by its ColumnInfo.
		{"rds-data adds metadata", `aws rds-data execute-statement --resource-arn a --sql "select 1"`,
			r + `--in=json -- aws rds-data execute-statement --resource-arn a --sql "select 1" --include-result-metadata`},
		{"rds-data metadata already there", `aws rds-data execute-statement --include-result-metadata --sql x`,
			r + `--in=json -- aws rds-data execute-statement --include-result-metadata --sql x`},
		{"athena results", `aws athena get-query-results --query-execution-id q`,
			r + `--in=json -- aws athena get-query-results --query-execution-id q`},
		{"athena text output", `aws --output text athena get-query-results --query-execution-id q`,
			r + `--in=auto -- aws --output text athena get-query-results --query-execution-id q`},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := piiRewrite(tc.in, "CT")
			if !ok || got != tc.want {
				t.Errorf("piiRewrite(%s)\n got %v %s\nwant %s", tc.in, ok, got, tc.want)
			}
		})
	}
	for _, keep := range []string{
		`duckdb -ui`, `duckdb --version`, `duckdb -version`, `duckdb --help`,
		`echo psql -c x`, `git commit -m "psql -t -A"`, `which psql duckdb`,
		`CT redact --pii --in=csv -- psql --csv -c x`, `/home/user/.claude/bin/claude-tools redact --pii -- duckdb -json -c "from 'a.parquet'"`,
		`aws rds-data begin-transaction --resource-arn a`, `aws athena start-query-execution --query-string x`,
		`python3 -c "import duckdb; duckdb.sql('from x.parquet').show()"`,
		`psql -c "unterminated`,
	} {
		if got, ok := piiRewrite(keep, "CT"); ok {
			t.Errorf("rewrote %s\n  to %s", keep, got)
		}
	}
}
