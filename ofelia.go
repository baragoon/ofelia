package main

import (
	"fmt"
	"os"
	"time"

	"github.com/baragoon/ofelia/cli"
	"github.com/baragoon/ofelia/core"
	"github.com/jessevdk/go-flags"
	"github.com/op/go-logging"
)

var version string
var build string

const logFormat = "%{time} %{color} %{shortfile} ▶ %{level} %{color:reset} %{message}"

func buildLogger() core.Logger {
	stdout := logging.NewLogBackend(os.Stdout, "", 0)
	// Set the backends to be used.
	logging.SetBackend(stdout)
	logging.SetFormatter(logging.MustStringFormatter(logFormat))
	return logging.MustGetLogger("ofelia")
}

func main() {
       // Respect TZ environment variable if set
	       if tz := os.Getenv("TZ"); tz != "" {
		       if loc, err := time.LoadLocation(tz); err == nil {
			       time.Local = loc
		       } else {
			       fmt.Fprintf(os.Stderr, "Warning: invalid TZ value '%s': %v\n", tz, err)
		       }
	       }
	       logger := buildLogger()
	       parser := flags.NewNamedParser("ofelia", flags.Default)
	       if _, err := parser.AddCommand("daemon", "daemon process", "", &cli.DaemonCommand{Logger: logger}); err != nil {
		       fmt.Fprintf(os.Stderr, "Error adding daemon command: %v\n", err)
		       os.Exit(1)
	       }
	       if _, err := parser.AddCommand("validate", "validates the config file", "", &cli.ValidateCommand{Logger: logger}); err != nil {
		       fmt.Fprintf(os.Stderr, "Error adding validate command: %v\n", err)
		       os.Exit(1)
	       }

	       if _, err := parser.Parse(); err != nil {
		       if flagsErr, ok := err.(*flags.Error); ok && flagsErr.Type == flags.ErrHelp {
			       fmt.Printf("\nBuild information\n  commit: %s\n  date: %s\n", version, build)
			       os.Exit(0)
		       }
		       // Print error to stderr for visibility
		       fmt.Fprintf(os.Stderr, "Error: %v\n", err)
		       os.Exit(1)
	       }
}
