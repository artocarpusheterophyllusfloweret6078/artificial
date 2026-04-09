package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"time"

	"artificial.pt/pkg-go-shared/protocol"
	"artificial.pt/cmd-worker/internal/harness"
	"artificial.pt/cmd-worker/internal/hub"
)

func main() {
	serverURL := flag.String("server", "localhost:3456", "svc-artificial host:port")
	employeeID := flag.Int64("employee-id", 0, "Employee ID to connect as")
	preWorkerID := flag.Int64("worker-id", 0, "Pre-assigned worker ID (from spawn endpoint)")
	flag.Parse()

	if *employeeID == 0 {
		fmt.Fprintln(os.Stderr, "error: --employee-id required")
		os.Exit(1)
	}

	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt)
	defer cancel()

	// 1. Fetch employee config from svc-artificial
	empConfig, err := fetchEmployeeConfig(*serverURL, *employeeID)
	if err != nil {
		slog.Error("fetch employee config", "err", err)
		os.Exit(1)
	}
	nick := empConfig.Employee.Nickname
	slog.Info("employee config loaded", "nick", nick, "channels", empConfig.Channels)

	// 2. Register worker (or use pre-assigned ID from spawn endpoint)
	var workerID int64
	if *preWorkerID > 0 {
		workerID = *preWorkerID
		slog.Info("using pre-assigned worker", "worker_id", workerID)
	} else {
		var err error
		workerID, err = registerWorker(*serverURL, *employeeID)
		if err != nil {
			slog.Error("register worker", "err", err)
			os.Exit(1)
		}
		slog.Info("worker registered", "worker_id", workerID)
	}
	defer updateWorkerStatus(*serverURL, workerID, "offline")

	// 3. Determine project path and channels
	projectPath := "."
	if empConfig.Project != nil && empConfig.Project.Path != "" {
		projectPath = empConfig.Project.Path
	}
	// ACP spec requires absolute cwd path
	if !filepath.IsAbs(projectPath) {
		if abs, err := filepath.Abs(projectPath); err == nil {
			projectPath = abs
		}
	}
	var projectID int64
	if empConfig.Project != nil {
		projectID = empConfig.Project.ID
	}
	channels := empConfig.Channels
	if len(channels) == 0 {
		channels = []string{"general"}
	}

	// 4. Create the harness based on employee config
	var h harness.Harness

	// Connect to hub WebSocket (handler set after harness is created)
	hubClient := hub.New(*serverURL, nick, func(msg protocol.WSMessage) {
		handleHubMessage(msg, nick, h)
	})
	go hubClient.ConnectWithRetry(ctx)

	cfg := harness.Config{
		Nickname:             nick,
		Role:                 empConfig.Employee.Role,
		Persona:              empConfig.Employee.Persona,
		Channels:             channels,
		ProjectPath:          projectPath,
		ProjectID:            projectID,
		Returning:            empConfig.Returning,
		ResumeSessionID:      empConfig.PreviousSessionID,
		CompanyKnowledgePath: empConfig.CompanyKnowledgePath,
		ACPURL:               empConfig.Employee.ACPURL,
		ACPProvider:          empConfig.Employee.ACPProvider,
		Model:                empConfig.Employee.Model,
	}

	switch empConfig.Employee.Harness {
	case "acp":
		slog.Info("using ACP harness", "provider", cfg.ACPProvider, "url", cfg.ACPURL)
		h = harness.NewACP(cfg, hubClient)
	case "codex":
		slog.Info("using Codex harness", "model", cfg.Model)
		h = harness.NewCodex(ctx, cfg, hubClient)
	default:
		slog.Info("using Claude harness")
		h = harness.NewClaude(ctx, cfg, hubClient)
	}

	// 5. Start the harness
	if err := h.Start(); err != nil {
		slog.Error("start harness", "err", err)
		os.Exit(1)
	}
	defer h.Stop()

	// 6. Report transcript path based on harness type
	if ch, ok := h.(*harness.Claude); ok {
		go func() {
			if tp := ch.WaitForTranscript(40 * time.Second); tp != "" {
				slog.Info("transcript discovered", "path", tp)
				reportTranscriptPath(*serverURL, workerID, tp)
			} else {
				slog.Warn("transcript path not discovered after timeout")
			}
		}()
	}
	if ah, ok := h.(*harness.ACP); ok {
		if tp := ah.TranscriptPath(); tp != "" {
			slog.Info("acp transcript path", "path", tp)
			reportTranscriptPath(*serverURL, workerID, tp)
		}
	}
	if cx, ok := h.(*harness.Codex); ok {
		if lp := cx.LogPath(); lp != "" {
			slog.Info("codex log path", "path", lp)
			reportTranscriptPath(*serverURL, workerID, lp)
		}
	}

	// 7. Wait for harness to exit
	result := h.Wait()

	slog.Info("harness exited", "exit_code", result.ExitCode, "session_id", result.SessionID)

	if result.SessionID != "" {
		updateWorkerSessionID(*serverURL, workerID, result.SessionID)
	}
}

// handleHubMessage processes messages from svc-artificial and pushes them to the harness.
func handleHubMessage(msg protocol.WSMessage, myNick string, h harness.Harness) {
	if h == nil {
		return
	}

	switch msg.Type {
	case protocol.MsgMessage:
		var m protocol.Message
		json.Unmarshal(msg.Data, &m)
		if m.Sender == myNick {
			return // skip own messages
		}
		content := fmt.Sprintf("%s: %s", m.Sender, m.Text)
		if m.Channel != "general" {
			content = fmt.Sprintf("[#%s] %s: %s", m.Channel, m.Sender, m.Text)
		}
		h.PushNotification(content, map[string]string{
			"chat_id": m.Channel,
			"user":    m.Sender,
		})

	case protocol.MsgDM:
		var m protocol.Message
		json.Unmarshal(msg.Data, &m)
		if m.Sender == myNick {
			return
		}
		h.PushNotification(
			fmt.Sprintf("[DM from %s]: %s", m.Sender, m.Text),
			map[string]string{"chat_id": "dm-" + m.Sender, "user": m.Sender},
		)

	case protocol.MsgMemberJoined:
		if msg.Nick != myNick {
			h.PushNotification(
				fmt.Sprintf("%s joined the team", msg.Nick),
				map[string]string{"chat_id": "general", "user": msg.Nick},
			)
		}

	case protocol.MsgMemberLeft:
		h.PushNotification(
			fmt.Sprintf("%s left the team", msg.Nick),
			map[string]string{"chat_id": "general", "user": msg.Nick},
		)

	case protocol.MsgTopicChanged:
		if msg.Nick != myNick {
			h.PushNotification(
				fmt.Sprintf("%s set topic for #%s: %s", msg.Nick, msg.Channel, msg.Topic),
				map[string]string{"chat_id": msg.Channel, "user": msg.Nick},
			)
		}

	case protocol.MsgTaskCreated, protocol.MsgTaskUpdated:
		var t protocol.Task
		json.Unmarshal(msg.Data, &t)
		action := "created"
		if msg.Type == protocol.MsgTaskUpdated {
			action = "updated"
		}
		h.PushNotification(
			fmt.Sprintf("Task #%d \"%s\" %s: status=%s, assignee=%s", t.ID, t.Title, action, t.Status, t.Assignee),
			map[string]string{"chat_id": "tasks", "user": t.CreatedBy},
		)

	case protocol.MsgReviewResponded:
		var review protocol.Review
		json.Unmarshal(msg.Data, &review)
		content := fmt.Sprintf("[Commander responded to review #%d \"%s\"]: %s", review.ID, review.Title, review.Response)
		h.PushNotification(content, map[string]string{
			"chat_id": "review",
			"user":    "commander",
		})

	case protocol.MsgWorkerNotify:
		from := msg.From
		if from == "" {
			from = "system"
		}
		h.PushNotification(
			fmt.Sprintf("[directive from %s]: %s", from, msg.Text),
			map[string]string{"chat_id": "directive", "user": from},
		)

	case protocol.MsgWorkerCommand:
		// PTY-specific: forward slash commands if harness supports it
		if ph, ok := h.(harness.PTYHarness); ok && msg.Text != "" {
			slog.Info("sending command to agent", "command", msg.Text)
			ph.SendCommand(msg.Text)
		}

	case protocol.MsgWorkerTTYInput:
		// PTY-specific: raw keystrokes
		if ph, ok := h.(harness.PTYHarness); ok && msg.Text != "" {
			ph.WriteRaw([]byte(msg.Text))
		}

	case protocol.MsgWorkerTTYResize:
		// PTY-specific: resize terminal
		if ph, ok := h.(harness.PTYHarness); ok && msg.Text != "" {
			ph.Resize(msg.Text)
		}
	}
}

// ── HTTP helpers for svc-artificial API ─────────────────────────────────

func fetchEmployeeConfig(serverURL string, employeeID int64) (protocol.EmployeeConfig, error) {
	url := fmt.Sprintf("http://%s/api/employees/%d", serverURL, employeeID)
	resp, err := http.Get(url)
	if err != nil {
		return protocol.EmployeeConfig{}, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		body, _ := io.ReadAll(resp.Body)
		return protocol.EmployeeConfig{}, fmt.Errorf("status %d: %s", resp.StatusCode, body)
	}
	var config protocol.EmployeeConfig
	if err := json.NewDecoder(resp.Body).Decode(&config); err != nil {
		return protocol.EmployeeConfig{}, err
	}
	return config, nil
}

func registerWorker(serverURL string, employeeID int64) (int64, error) {
	body := fmt.Sprintf(`{"employee_id":%d,"pid":%d}`, employeeID, os.Getpid())
	resp, err := http.Post(
		fmt.Sprintf("http://%s/api/workers", serverURL),
		"application/json",
		strings.NewReader(body),
	)
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()
	var w protocol.Worker
	json.NewDecoder(resp.Body).Decode(&w)
	return w.ID, nil
}

func updateWorkerStatus(serverURL string, workerID int64, status string) {
	body := fmt.Sprintf(`{"status":"%s"}`, status)
	req, _ := http.NewRequest("PUT",
		fmt.Sprintf("http://%s/api/workers/%d/status", serverURL, workerID),
		strings.NewReader(body),
	)
	req.Header.Set("Content-Type", "application/json")
	http.DefaultClient.Do(req)
}

func updateWorkerSessionID(serverURL string, workerID int64, sessionID string) {
	body := fmt.Sprintf(`{"session_id":%q}`, sessionID)
	req, _ := http.NewRequest("PUT",
		fmt.Sprintf("http://%s/api/workers/%d/session", serverURL, workerID),
		strings.NewReader(body),
	)
	req.Header.Set("Content-Type", "application/json")
	http.DefaultClient.Do(req)
	slog.Info("session id stored", "worker_id", workerID, "session_id", sessionID)
}

func reportTranscriptPath(serverURL string, workerID int64, path string) {
	body := fmt.Sprintf(`{"transcript_path":%q}`, path)
	req, _ := http.NewRequest("PUT",
		fmt.Sprintf("http://%s/api/workers/%d/transcript", serverURL, workerID),
		strings.NewReader(body),
	)
	req.Header.Set("Content-Type", "application/json")
	http.DefaultClient.Do(req)
}

func truncate(s string, max int) string {
	if len(s) <= max {
		return s
	}
	return s[:max] + "..."
}

