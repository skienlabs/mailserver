// Command skienmail runs the Skien Mails server: SMTP (inbound, submission),
// outbound delivery queue and the webmail/admin web app, in one process.
//
// Maintenance commands (run inside the container with `docker exec skienmail skienmail ...`):
//
//	skienmail reset-password <email>   set a new temporary password (e.g. a locked-out admin)
//	skienmail backup <file>            write a consistent copy of the database while running
//	skienmail version
package main

import (
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"syscall"

	"github.com/skienlabs/mailserver/internal/core"
)

func main() {
	cfg := core.LoadConfig()
	if len(os.Args) > 1 {
		os.Exit(runCommand(cfg, os.Args[1:]))
	}

	level := slog.LevelInfo
	if cfg.LogLevelDebug {
		level = slog.LevelDebug
	}
	log := slog.New(slog.NewTextHandler(os.Stdout, &slog.HandlerOptions{Level: level}))
	slog.SetDefault(log)

	app, err := core.NewApp(cfg, log)
	if err != nil {
		log.Error("startup failed", "err", err)
		os.Exit(1)
	}
	if err := app.Start(); err != nil {
		log.Error("startup failed", "err", err)
		os.Exit(1)
	}
	log.Info(core.ProductName+" is running", "version", core.Version, "hostname", app.Hostname())

	sig := make(chan os.Signal, 1)
	signal.Notify(sig, syscall.SIGINT, syscall.SIGTERM)
	<-sig
	log.Info("shutting down")
	app.Shutdown()
}

func runCommand(cfg *core.Config, args []string) int {
	switch args[0] {
	case "version", "--version", "-v":
		fmt.Println(core.ProductName, core.Version)
		return 0
	case "reset-password":
		if len(args) != 2 {
			fmt.Fprintln(os.Stderr, "usage: skienmail reset-password <email>")
			return 2
		}
		pw, err := core.ResetPasswordCLI(cfg, args[1])
		if err != nil {
			fmt.Fprintln(os.Stderr, "error:", err)
			return 1
		}
		fmt.Printf("Temporary password for %s: %s\nThey will choose a new password at the next sign-in.\n", args[1], pw)
		return 0
	case "backup":
		if len(args) != 2 {
			fmt.Fprintln(os.Stderr, "usage: skienmail backup <file.db>")
			return 2
		}
		if err := core.BackupCLI(cfg, args[1]); err != nil {
			fmt.Fprintln(os.Stderr, "error:", err)
			return 1
		}
		fmt.Println("Database copied to", args[1], "— also back up the mail/ and avatars/ folders of the data volume.")
		return 0
	case "help", "-h", "--help":
		fmt.Println("usage: skienmail [reset-password <email> | backup <file.db> | version]")
		return 0
	}
	fmt.Fprintf(os.Stderr, "unknown command %q (try: skienmail help)\n", args[0])
	return 2
}
