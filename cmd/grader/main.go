package main

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"os"

	"github.com/ruddervirt/aileron/internal/grader"
)

type graderOutput struct {
	Results []graderCommandResult `json:"results"`
	Error   string                `json:"error,omitempty"`
}

type graderCommandResult struct {
	Stdout   string `json:"stdout"`
	Stderr   string `json:"stderr"`
	ExitCode int32  `json:"exitCode"`
}

func main() {
	slog.SetDefault(slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelInfo})))

	namespace := os.Getenv("GRADE_NAMESPACE")
	vmName := os.Getenv("GRADE_VM_NAME")
	user := os.Getenv("GRADE_USER")
	password := os.Getenv("GRADE_PASSWORD")
	domain := os.Getenv("GRADE_DOMAIN")
	methodStr := os.Getenv("GRADE_METHOD")
	commandsJSON := os.Getenv("GRADE_COMMANDS")

	if namespace == "" || vmName == "" || commandsJSON == "" {
		writeOutput(graderOutput{Error: "GRADE_NAMESPACE, GRADE_VM_NAME, and GRADE_COMMANDS are required"})
		os.Exit(1)
	}

	var commands []string
	if err := json.Unmarshal([]byte(commandsJSON), &commands); err != nil {
		writeOutput(graderOutput{Error: fmt.Sprintf("failed to parse GRADE_COMMANDS: %v", err)})
		os.Exit(1)
	}

	method, err := grader.ParseGradeMethod(methodStr)
	if err != nil {
		writeOutput(graderOutput{Error: err.Error()})
		os.Exit(1)
	}

	results, err := grader.Grade(method, namespace, vmName, user, password, domain, commands)
	if err != nil {
		writeOutput(graderOutput{Error: fmt.Sprintf("grading failed: %v", err)})
		os.Exit(1)
	}

	output := graderOutput{
		Results: make([]graderCommandResult, len(results)),
	}
	for i, r := range results {
		output.Results[i] = graderCommandResult{
			Stdout:   r.Stdout,
			Stderr:   r.Stderr,
			ExitCode: r.ExitCode,
		}
	}

	writeOutput(output)
}

func writeOutput(output graderOutput) {
	if err := json.NewEncoder(os.Stdout).Encode(output); err != nil {
		slog.Error("failed to encode grader output", "error", err)
		os.Exit(1)
	}
}
