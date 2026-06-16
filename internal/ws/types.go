package ws

// CommandResult stores the output and status of a command
type CommandResult struct {
	Stdout   string
	Stderr   string
	ExitCode int32
}
