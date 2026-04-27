package server

import (
	"encoding/json"
	"path/filepath"
	"testing"

	"artificial.pt/pkg-go-shared/protocol"
	"artificial.pt/svc-artificial/internal/db"
)

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
