package server

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"artificial.pt/pkg-go-shared/protocol"
	"artificial.pt/svc-artificial/internal/db"
)

type autoSpawnCall struct {
	taskID     int64
	parentNick string
}

func TestHandleTaskUpdateAutoSpawnsOnInProgressTransition(t *testing.T) {
	database, err := db.Open(filepath.Join(t.TempDir(), "artificial.db"))
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	defer database.Close()

	project, err := database.CreateProject("test", t.TempDir(), "")
	if err != nil {
		t.Fatalf("create project: %v", err)
	}
	if _, err := database.CreateEmployee("manager", "manager", "", ""); err != nil {
		t.Fatalf("create employee: %v", err)
	}
	task, err := database.CreateTask("runner task", "do work", "", project.ID, "commander")
	if err != nil {
		t.Fatalf("create task: %v", err)
	}

	hub := NewHub(database, 0)
	calls := make(chan autoSpawnCall, 2)
	hub.autoSpawnRunnerForTask = func(taskID int64, parentNick string) {
		calls <- autoSpawnCall{taskID: taskID, parentNick: parentNick}
	}

	payload, _ := json.Marshal(map[string]any{
		"id":       task.ID,
		"status":   "in_progress",
		"assignee": "manager",
	})
	hub.handleTaskUpdate(&client{nick: "manager"}, protocol.WSMessage{
		Type: protocol.MsgTaskUpdate,
		Data: payload,
	})

	select {
	case got := <-calls:
		if got.taskID != task.ID {
			t.Fatalf("auto-spawn task id = %d, want %d", got.taskID, task.ID)
		}
		if got.parentNick != "manager" {
			t.Fatalf("auto-spawn parent = %q, want manager", got.parentNick)
		}
	case <-time.After(time.Second):
		t.Fatal("auto-spawn was not called")
	}

	updated, err := database.GetTask(task.ID)
	if err != nil {
		t.Fatalf("get task: %v", err)
	}
	if updated.Status != "in_progress" {
		t.Fatalf("task status = %q, want in_progress", updated.Status)
	}
	if updated.Assignee != "manager" {
		t.Fatalf("task assignee = %q, want manager", updated.Assignee)
	}

	repeatPayload, _ := json.Marshal(map[string]any{
		"id":     task.ID,
		"status": "in_progress",
	})
	hub.handleTaskUpdate(&client{nick: "manager"}, protocol.WSMessage{
		Type: protocol.MsgTaskUpdate,
		Data: repeatPayload,
	})
	assertNoAutoSpawnCall(t, calls)

	task2, err := database.CreateTask("non runner task", "do work", "", project.ID, "commander")
	if err != nil {
		t.Fatalf("create second task: %v", err)
	}
	todoPayload, _ := json.Marshal(map[string]any{
		"id":     task2.ID,
		"status": "todo",
	})
	hub.handleTaskUpdate(&client{nick: "manager"}, protocol.WSMessage{
		Type: protocol.MsgTaskUpdate,
		Data: todoPayload,
	})
	assertNoAutoSpawnCall(t, calls)
}

func TestHandleTaskUpdateAutoSpawnsRunnerRowThroughSharedPath(t *testing.T) {
	database, err := db.Open(filepath.Join(t.TempDir(), "artificial.db"))
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	defer database.Close()

	workerBin := setupRunnerSpawnTestEnvironment(t)
	repoPath := initGitRepo(t)
	project, err := database.CreateProject("test", repoPath, "")
	if err != nil {
		t.Fatalf("create project: %v", err)
	}
	task, err := database.CreateTask("runner task", "do work", "manager", project.ID, "commander")
	if err != nil {
		t.Fatalf("create task: %v", err)
	}

	s := New(database, 0, workerBin)
	payload, _ := json.Marshal(map[string]any{
		"id":     task.ID,
		"status": "in_progress",
	})
	s.Hub.handleTaskUpdate(&client{nick: "manager"}, protocol.WSMessage{
		Type: protocol.MsgTaskUpdate,
		Data: payload,
	})

	runner := waitForActiveRunner(t, database, task.ID)
	if runner.TaskID != task.ID {
		t.Fatalf("runner task id = %d, want %d", runner.TaskID, task.ID)
	}
	if runner.ParentNick != "manager" {
		t.Fatalf("runner parent = %q, want manager", runner.ParentNick)
	}
	runners, err := database.ListRunnersForTask(task.ID)
	if err != nil {
		t.Fatalf("list runners: %v", err)
	}
	if len(runners) != 1 {
		t.Fatalf("runner count = %d, want 1", len(runners))
	}
}

func TestHandleRunnerCompleteDoesNotCreateReview(t *testing.T) {
	database, err := db.Open(filepath.Join(t.TempDir(), "artificial.db"))
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	defer database.Close()

	project, err := database.CreateProject("test", t.TempDir(), "")
	if err != nil {
		t.Fatalf("create project: %v", err)
	}
	task, err := database.CreateTask("runner task", "do work", "", project.ID, "commander")
	if err != nil {
		t.Fatalf("create task: %v", err)
	}
	runner, err := database.CreateTaskRunner(task.ID, "runner-test", "manager", "/tmp/worktree", "runner/test", "master", "")
	if err != nil {
		t.Fatalf("create runner: %v", err)
	}

	hub := NewHub(database, 0)
	payload, _ := json.Marshal(protocol.RunnerCompletePayload{
		Summary:    "implemented",
		BranchName: "runner/test",
		Commits:    []string{"abc123"},
	})
	hub.handleRunnerComplete(nil, protocol.WSMessage{
		Type: protocol.MsgRunnerComplete,
		ID:   runner.ID,
		Data: payload,
	})

	updatedRunner, err := database.GetTaskRunner(runner.ID)
	if err != nil {
		t.Fatalf("get runner: %v", err)
	}
	if updatedRunner.Status != protocol.RunnerStatusComplete {
		t.Fatalf("runner status = %q, want %q", updatedRunner.Status, protocol.RunnerStatusComplete)
	}
	if updatedRunner.LastSummary != "implemented" {
		t.Fatalf("runner summary = %q, want implemented", updatedRunner.LastSummary)
	}
	if updatedRunner.FinishedAt == "" {
		t.Fatal("runner finished_at was not set")
	}

	updatedTask, err := database.GetTask(task.ID)
	if err != nil {
		t.Fatalf("get task: %v", err)
	}
	if updatedTask.Status != "in_qa" {
		t.Fatalf("task status = %q, want in_qa", updatedTask.Status)
	}

	reviews, err := database.ListPendingReviews()
	if err != nil {
		t.Fatalf("list reviews: %v", err)
	}
	if len(reviews) != 0 {
		t.Fatalf("pending reviews = %d, want 0", len(reviews))
	}
}

func assertNoAutoSpawnCall(t *testing.T, calls <-chan autoSpawnCall) {
	t.Helper()
	select {
	case got := <-calls:
		t.Fatalf("unexpected auto-spawn call for task %d parent %q", got.taskID, got.parentNick)
	default:
	}
}

func setupRunnerSpawnTestEnvironment(t *testing.T) string {
	t.Helper()

	t.Setenv("HOME", t.TempDir())
	binDir := t.TempDir()
	writeExecutable(t, filepath.Join(binDir, "claude"), "#!/bin/sh\nexit 0\n")
	workerBin := filepath.Join(binDir, "cmd-worker")
	writeExecutable(t, workerBin, "#!/bin/sh\nexit 0\n")
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))
	return workerBin
}

func writeExecutable(t *testing.T, path, body string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(body), 0755); err != nil {
		t.Fatalf("write executable %s: %v", path, err)
	}
}

func initGitRepo(t *testing.T) string {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git binary not found")
	}
	dir := t.TempDir()
	runGit := func(args ...string) {
		t.Helper()
		cmd := exec.Command("git", args...)
		cmd.Dir = dir
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("git %v failed: %s: %v", args, string(out), err)
		}
	}
	runGit("init")
	runGit("config", "user.email", "test@example.com")
	runGit("config", "user.name", "Test User")
	if err := os.WriteFile(filepath.Join(dir, "README.md"), []byte("test\n"), 0644); err != nil {
		t.Fatalf("write README: %v", err)
	}
	runGit("add", "README.md")
	runGit("commit", "-m", "initial")
	return dir
}

func waitForActiveRunner(t *testing.T, database *db.DB, taskID int64) protocol.TaskRunner {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	var lastErr error
	for time.Now().Before(deadline) {
		runner, err := database.GetActiveRunnerForTask(taskID)
		if err == nil {
			return runner
		}
		lastErr = err
		time.Sleep(25 * time.Millisecond)
	}
	t.Fatalf("active runner was not created for task %d: %v", taskID, lastErr)
	return protocol.TaskRunner{}
}
