// Command migrate applies and rolls back the versioned database migrations.
//
//	migrate up            apply every pending migration
//	migrate down          roll every applied migration back
//	migrate steps <n>     move n migrations forward (n > 0) or back (n < 0)
//	migrate version       print the current version and dirty flag
//
// The connection string comes from DATABASE_URL.
package main

import (
	"fmt"
	"os"
	"strconv"

	"github.com/NicolasPaterno/backend-challenge-go/migrations"
)

func main() {
	if err := run(os.Args[1:]); err != nil {
		fmt.Fprintf(os.Stderr, "migrate: %v\n", err)
		os.Exit(1)
	}
}

func run(args []string) error {
	databaseURL := os.Getenv("DATABASE_URL")
	if databaseURL == "" {
		return fmt.Errorf("DATABASE_URL is required")
	}

	const usage = "usage: migrate <up|down|steps n|version>"
	if len(args) == 0 {
		return fmt.Errorf("%s", usage)
	}
	command, rest := args[0], args[1:]

	// Rejecting extra operands caught a Compose entrypoint that appended the
	// subcommand instead of replacing it, silently running `migrate up down`.
	want := 0
	if command == "steps" {
		want = 1
	}
	if len(rest) != want {
		return fmt.Errorf("%s", usage)
	}

	var err error
	switch command {
	case "up":
		err = migrations.Up(databaseURL)
	case "down":
		err = migrations.Down(databaseURL)
	case "steps":
		n, convErr := strconv.Atoi(rest[0])
		if convErr != nil {
			return fmt.Errorf("steps must be an integer, got %q", rest[0])
		}
		err = migrations.Steps(databaseURL, n)
	case "version":
	default:
		return fmt.Errorf("unknown command %q", command)
	}
	if err != nil {
		return err
	}

	version, dirty, err := migrations.Version(databaseURL)
	if err != nil {
		return err
	}
	fmt.Printf("schema version %d (dirty=%t)\n", version, dirty)
	return nil
}
