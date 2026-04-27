package server

import (
	"path/filepath"
	"strings"
	"testing"

	"artificial.pt/pkg-go-shared/protocol"
	"artificial.pt/svc-artificial/internal/db"
)

func newProjectAssignmentTestHub(t *testing.T) (*Hub, *db.DB) {
	t.Helper()
	database, err := db.Open(filepath.Join(t.TempDir(), "artificial.db"))
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	t.Cleanup(func() { database.Close() })
	return NewHub(database, 0), database
}

func TestAssignEmployeesToProjectSuccessAndPerEmployeeFailures(t *testing.T) {
	hub, database := newProjectAssignmentTestHub(t)
	if _, err := database.CreateEmployee("ceo", "ceo", "", ""); err != nil {
		t.Fatalf("create ceo: %v", err)
	}
	alice, err := database.CreateEmployee("alice", "worker", "", "")
	if err != nil {
		t.Fatalf("create alice: %v", err)
	}
	bob, err := database.CreateEmployee("bob", "worker", "", "")
	if err != nil {
		t.Fatalf("create bob: %v", err)
	}
	if _, err := database.CreateEmployee("lead", "commander", "", ""); err != nil {
		t.Fatalf("create lead: %v", err)
	}
	project, err := database.CreateProject("Project Alpha", t.TempDir(), "")
	if err != nil {
		t.Fatalf("create project: %v", err)
	}

	result := hub.assignEmployeesToProject("ceo", protocol.ProjectAssignmentRequest{
		ProjectName:       project.Name,
		EmployeeIDs:       []int64{alice.ID, 999},
		EmployeeNicknames: []string{bob.Nickname, "lead"},
	})

	if result.Error != "" {
		t.Fatalf("assignment returned error: %s", result.Error)
	}
	if result.SuccessCount != 2 || result.FailureCount != 2 {
		t.Fatalf("counts = success %d failure %d, want 2/2", result.SuccessCount, result.FailureCount)
	}
	if len(result.Results) != 4 {
		t.Fatalf("results = %d, want 4", len(result.Results))
	}

	want := map[string]bool{
		"alice": true,
		"bob":   true,
	}
	assigned, err := database.ListProjectEmployees(project.ID)
	if err != nil {
		t.Fatalf("list project employees: %v", err)
	}
	if len(assigned) != len(want) {
		t.Fatalf("assigned employees = %d, want %d", len(assigned), len(want))
	}
	for _, emp := range assigned {
		if !want[emp.Nickname] {
			t.Fatalf("unexpected assigned employee %q", emp.Nickname)
		}
		delete(want, emp.Nickname)
	}

	var missingIDFailed, commanderFailed bool
	for _, r := range result.Results {
		if r.Identifier == "#999" && !r.OK && strings.Contains(r.Error, "not found") {
			missingIDFailed = true
		}
		if r.Nickname == "lead" && !r.OK && strings.Contains(r.Error, "not a worker") {
			commanderFailed = true
		}
	}
	if !missingIDFailed {
		t.Fatal("missing employee ID did not produce a per-employee failure")
	}
	if !commanderFailed {
		t.Fatal("non-worker employee did not produce a per-employee failure")
	}
}

func TestAssignEmployeesToProjectInvalidProject(t *testing.T) {
	hub, database := newProjectAssignmentTestHub(t)
	if _, err := database.CreateEmployee("ceo", "ceo", "", ""); err != nil {
		t.Fatalf("create ceo: %v", err)
	}
	worker, err := database.CreateEmployee("alice", "worker", "", "")
	if err != nil {
		t.Fatalf("create worker: %v", err)
	}

	result := hub.assignEmployeesToProject("ceo", protocol.ProjectAssignmentRequest{
		ProjectID:   404,
		EmployeeIDs: []int64{worker.ID},
	})

	if !strings.Contains(result.Error, "project 404 not found") {
		t.Fatalf("error = %q, want project not found", result.Error)
	}
}

func TestAssignEmployeesToProjectRequiresCEO(t *testing.T) {
	hub, database := newProjectAssignmentTestHub(t)
	worker, err := database.CreateEmployee("alice", "worker", "", "")
	if err != nil {
		t.Fatalf("create worker: %v", err)
	}
	project, err := database.CreateProject("Project Alpha", t.TempDir(), "")
	if err != nil {
		t.Fatalf("create project: %v", err)
	}

	result := hub.assignEmployeesToProject("alice", protocol.ProjectAssignmentRequest{
		ProjectID:   project.ID,
		EmployeeIDs: []int64{worker.ID},
	})

	if !strings.Contains(result.Error, "restricted to CEO") {
		t.Fatalf("error = %q, want CEO restriction", result.Error)
	}
	assigned, err := database.ListProjectEmployees(project.ID)
	if err != nil {
		t.Fatalf("list project employees: %v", err)
	}
	if len(assigned) != 0 {
		t.Fatalf("assigned employees = %d, want 0", len(assigned))
	}
}
