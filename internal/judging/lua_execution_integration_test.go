package judging

import (
	"archive/zip"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/nats-io/nats.go"
	"podtest/internal/artifact"
	"podtest/internal/db"
	"podtest/internal/problem"
)

func TestLuaSubmissionExecution(t *testing.T) {
	binary := os.Getenv("PODTEST_LUA_WORKER")
	if os.Getenv("PODTEST_ARTIFACT_ACCEPTANCE") != "1" || binary == "" {
		t.Skip("requires disposable PostgreSQL, HTTPS MinIO, JetStream and the Lua worker")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	defer cancel()
	must := func(err error) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
	}
	pool, err := pgxpool.New(ctx, os.Getenv("DATABASE_URL"))
	must(err)
	defer pool.Close()
	must(db.Migrate(ctx, pool))
	store, err := artifact.New(pool)
	must(err)
	must(store.Init(ctx))
	var user, contest, task string
	must(pool.QueryRow(ctx, `WITH u AS (INSERT INTO users(display_name) VALUES('Lua execution') RETURNING id),
		c AS (INSERT INTO contests(title,created_by,status) SELECT 'Lua execution',id,'open' FROM u RETURNING id),
		p AS (INSERT INTO problems(title,created_by) SELECT 'Echo',id FROM u RETURNING id),
		part AS (INSERT INTO contest_participants(contest_id,user_id) SELECT c.id,u.id FROM c,u),
		settings AS (INSERT INTO contest_settings(contest_id) SELECT id FROM c)
		SELECT u.id,c.id,p.id FROM u,c,p`).Scan(&user, &contest, &task))
	manager := problem.Manager{Pool: pool, Artifacts: store, EnableLua: true}
	revision, err := manager.Create(ctx, task, problem.RevisionInput{Format: "scripted_lua"})
	must(err)
	_, err = pool.Exec(ctx, `INSERT INTO problem_statements(revision_id,locale,source,rendered_html,renderer_version,digest) VALUES($1,'en','Echo','Echo','1',$2)`, revision.ID, fmt.Sprintf("%064x", 1))
	must(err)
	must(manager.SetLanguage(ctx, task, revision.ID, "posix_sh"))
	var archive bytes.Buffer
	zipper := zip.NewWriter(&archive)
	file, err := zipper.Create("runner.lua")
	must(err)
	_, err = file.Write([]byte(`local j = require('judge.runtime')
local handle = assert(tonumber(j.container_open('solution/source')))
local output = ''
while true do
  local chunk = j.stdout_read(handle)
  if chunk == nil then break end
  output = output .. chunk
end
local code
repeat code = j.container_status(handle); if not code then j.sleep(20) end until code
j.container_close(handle)
local ok = output == 'hello' and code == '0'
j.test_verdict('echo', ok and 'accepted' or 'wrong_answer', ok and 1000 or 0)
j.subgroup_score('main', ok and 1000 or 0)
j.problem_score(ok and 1000 or 0)
print('lua fixture completed')
`))
	must(err)
	must(zipper.Close())
	must(manager.SetPackage(ctx, user, task, revision.ID, archive.Bytes()))
	must(manager.Validate(ctx, task, revision.ID))
	must(manager.Publish(ctx, user, task, revision.ID))
	cp, err := manager.Attach(ctx, user, contest, problem.AttachInput{ProblemID: task, RevisionID: revision.ID, MaxScore: "1", VerdictVisibility: "tests"})
	must(err)
	subs := Submissions{Pool: pool, Artifacts: store}
	sub, err := subs.Submit(ctx, user, contest, cp, "posix_sh", []byte("printf hello\n"))
	must(err)
	nc, err := nats.Connect(os.Getenv("PODTEST_NATS_URL"))
	must(err)
	defer nc.Close()
	js, err := nc.JetStream()
	must(err)
	must(provisionStandardQueue(ctx, js))
	must(dispatch(ctx, pool, js))
	invoker := uuid.NewString()
	_, _, tlsEnv := artifactClients(t, invoker, &ArtifactControl{Pool: pool, Store: store})
	state := t.TempDir()
	must(os.Chmod(state, 0700))
	config := filepath.Join(t.TempDir(), "config.json")
	data, err := json.Marshal(map[string]any{"invoker_id": invoker, "state_dir": state, "podman_socket": os.Getenv("PODTEST_PODMAN_SOCKET"), "libpod_api_version": os.Getenv("PODTEST_LIBPOD_API_VERSION")})
	must(err)
	must(os.WriteFile(config, data, 0600))
	command := func() *exec.Cmd {
		cmd := exec.CommandContext(ctx, binary, "work-lua-once", config)
		cmd.Env = append(os.Environ(), tlsEnv...)
		return cmd
	}
	cmd := command()
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("Lua worker: %v\n%s", err, out)
	}
	items, err := subs.Attempts(ctx, sub.ID, "manager")
	must(err)
	if len(items) != 1 || items[0].Status != "finished" || items[0].Verdict == nil || *items[0].Verdict != "accepted" {
		log, _ := filepath.Glob(filepath.Join(state, "standard-runs", "*", "execution", "runner.log"))
		for _, path := range log {
			body, _ := os.ReadFile(path)
			t.Logf("runner log: %s", body)
		}
		t.Fatalf("unexpected Lua attempts: %+v", items)
	}
	var result, runnerLog bool
	must(pool.QueryRow(ctx, `SELECT EXISTS(SELECT FROM test_results WHERE attempt_id=$1 AND test_id='echo' AND verdict='accepted'),
		EXISTS(SELECT FROM attempt_artifacts WHERE attempt_id=$1 AND kind='runner_log')`, items[0].ID).Scan(&result, &runnerLog))
	if !result || !runnerLog {
		t.Fatalf("Lua result and runner log: %t, %t", result, runnerLog)
	}

	slow, err := subs.Submit(ctx, user, contest, cp, "posix_sh", []byte("exec sleep 30\n"))
	must(err)
	var attempt, run string
	must(pool.QueryRow(ctx, `SELECT id,run_id FROM judging_attempts WHERE submission_id=$1`, slow.ID).Scan(&attempt, &run))
	must(dispatch(ctx, pool, js))
	worker := command()
	var log bytes.Buffer
	worker.Stdout, worker.Stderr = &log, &log
	must(worker.Start())
	defer worker.Process.Kill()
	done := make(chan error, 1)
	go func() { done <- worker.Wait() }()
	containers := func() string {
		out, err := exec.CommandContext(ctx, "podman", "--remote", "--url", "unix://"+os.Getenv("PODTEST_PODMAN_SOCKET"),
			"ps", "--all", "--filter", "label=podtest.run_id="+run, "--format", "{{.State}}").Output()
		must(err)
		return strings.TrimSpace(string(out))
	}
	ticker := time.NewTicker(100 * time.Millisecond)
	defer ticker.Stop()
	for !strings.Contains(containers(), "running") {
		select {
		case err := <-done:
			t.Fatalf("worker exited before cancellation: %v\n%s", err, &log)
		case <-ctx.Done():
			t.Fatal(ctx.Err())
		case <-ticker.C:
		}
	}
	pending, err := subs.Cancel(ctx, user, slow.ID, attempt)
	must(err)
	if !pending {
		t.Fatal("Lua attempt was not active during cancellation")
	}
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Lua cancellation: %v\n%s", err, &log)
		}
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	items, err = subs.Attempts(ctx, slow.ID, "manager")
	must(err)
	if len(items) != 1 || items[0].Status != "cancelled" || items[0].CancelPending || containers() != "" {
		t.Fatalf("unexpected Lua cancellation: %+v", items)
	}
}
