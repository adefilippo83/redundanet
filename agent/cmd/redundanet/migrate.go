package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"runtime"
	"strings"
	"time"

	"github.com/adefilippo83/redundanet/agent/internal/migrate"
)

// nodeDataDir is where the node installer keeps the agent's data; as root,
// migrate-node defaults to it when it exists.
const nodeDataDir = "/var/lib/redundanet3"

func runMigrateNode(args []string) error {
	fs := flag.NewFlagSet("migrate-node", flag.ContinueOnError)
	dir, _ := commonFlags(fs)
	dryRun := fs.Bool("dry-run", false, "print the plan and change nothing")
	force := fs.Bool("force", false, "retire the stack although something is not yet on the native grid")
	rollback := fs.Bool("rollback", false, "start the stack again and forget the migration")
	composeDir := fs.String("compose-dir", migrate.DefaultComposeDir, "where the RedundaNet 2 compose file is installed")
	envFile := fs.String("env-file", migrate.DefaultEnvFile, "the stack's .env")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if runtime.GOOS != "linux" {
		return errors.New("migrate-node retires the Docker stack of a Linux node; there is none here")
	}
	if os.Geteuid() != 0 {
		return errors.New("migrate-node drives docker compose and reads /opt/redundanet: run it with sudo")
	}
	d := *dir
	if d == "" && os.Getenv("REDUNDANET_DATA_DIR") == "" {
		if _, err := os.Stat(identityPath(nodeDataDir)); err == nil {
			d = nodeDataDir
		}
	}
	d, err := dataDir(d)
	if err != nil {
		return err
	}
	cfg := migrate.Config{ComposeDir: *composeDir, EnvFile: *envFile, Project: migrate.DefaultProject, DataDir: d, Run: migrate.Exec, Now: time.Now}
	if *rollback {
		if err := migrate.Rollback(cfg); err != nil {
			return err
		}
		fmt.Println("the RedundaNet 2 stack is starting again, every service together and tinc first; the agent never stopped")
		return nil
	}
	if at, ok := migrate.Migrated(d); ok {
		return fmt.Errorf("this node retired its stack on %s; --rollback brings it back", at.Format("2006-01-02"))
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	c, err := localClient(d)
	if err != nil {
		return err
	}
	st, err := c.Status(ctx)
	if err != nil {
		return fmt.Errorf("the agent must be running and reachable (it stays up through the migration): %w", err)
	}
	agent := &migrate.Agent{Device: st.Device, Jobs: st.Jobs}
	if st.Transport != nil {
		for _, h := range st.Transport.Hubs {
			if h.Up {
				agent.HubsUp++
			}
		}
	}
	plan, err := migrate.NewPlan(cfg, agent)
	if err != nil {
		return err
	}
	fmt.Print(plan)
	if *dryRun {
		fmt.Println("dry run: nothing changed")
		return nil
	}
	if err := migrate.Apply(cfg, plan, agent, *force); err != nil {
		return err
	}
	fmt.Printf("the RedundaNet 2 stack (%s) is stopped and removed as containers; its files and the Tahoe shares stay on disk\n", strings.Join(plan.Services, ", "))
	if st, err := c.Status(ctx); err == nil {
		up := 0
		if st.Transport != nil {
			for _, h := range st.Transport.Hubs {
				if h.Up {
					up++
				}
			}
		}
		fmt.Printf("the agent is up, device %s, %d hub(s) connected; the status page keeps this node\n", st.Device, up)
	}
	fmt.Println("to undo: sudo redundanet migrate-node --rollback")
	return nil
}
