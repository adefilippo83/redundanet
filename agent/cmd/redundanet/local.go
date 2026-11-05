package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"time"

	"github.com/adefilippo83/redundanet/agent/internal/api"
)

// status, open and logs talk to the running agent through its local API.

func localClient(dir string) (*api.Client, error) {
	d, err := dataDir(dir)
	if err != nil {
		return nil, err
	}
	// On a node set up by the node installer the agent's data lives under
	// /var/lib/redundanet3. Root with no agent of its own talks to that
	// one, so `sudo redundanet backup list` works without the flags the
	// service user needs.
	if dir == "" && os.Getenv("REDUNDANET_DATA_DIR") == "" && runtime.GOOS == "linux" && os.Geteuid() == 0 {
		if _, err := os.Stat(filepath.Join(d, "api.addr")); err != nil {
			if _, err := os.Stat(filepath.Join(nodeDataDir, "api.addr")); err == nil {
				d = nodeDataDir
			}
		}
	}
	addr, token, err := api.ReadClientConfig(d)
	if err != nil {
		return nil, err
	}
	return api.NewClient(addr, token), nil
}

func runStatus(args []string) error {
	fs := flag.NewFlagSet("status", flag.ContinueOnError)
	dir, _ := commonFlags(fs)
	asJSON := fs.Bool("json", false, "print the full snapshot as JSON")
	if err := fs.Parse(args); err != nil {
		return err
	}
	c, err := localClient(*dir)
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	s, err := c.Status(ctx)
	if err != nil {
		return fmt.Errorf("the agent did not answer: %w", err)
	}
	if *asJSON {
		b, _ := json.MarshalIndent(s, "", "  ")
		fmt.Println(string(b))
		return nil
	}
	fmt.Printf("%s %s, up %s, network %s\n", "redundanet", s.Version, s.Uptime, s.Network)
	fmt.Printf("member %s (%s), device %s\n", s.Member, s.MemberCode, s.Device)
	if s.Membership != nil {
		m := s.Membership
		fmt.Printf("manifest revision %d, %d members, %d hubs", m.Revision, m.Members, m.Hubs)
		if m.Expired {
			fmt.Print(", signature expired")
		}
		if m.LastError != "" {
			fmt.Printf(", last fetch failed: %s", m.LastError)
		}
		fmt.Println()
	}
	if s.Transport != nil {
		for _, h := range s.Transport.Hubs {
			state := "down"
			if h.Up {
				state = "up over " + h.Carrier + ", seen as " + h.Observed
			}
			fmt.Printf("hub %s: %s\n", h.Hub, state)
		}
		for _, p := range s.Transport.Sessions {
			path := "no path"
			switch {
			case p.Direct != "" && p.Relay:
				path = "direct " + p.Direct + " and relay"
			case p.Direct != "":
				path = "direct " + p.Direct
			case p.Relay:
				path = "relay"
			}
			fmt.Printf("peer %s: %s\n", p.Peer[:8], path)
		}
	}
	if s.Update != nil {
		u := s.Update
		switch {
		case !u.Enabled:
			fmt.Println("updates: disabled in this build")
		case u.Available != "":
			fmt.Printf("updates: %s available (running %s)\n", u.Available, u.Running)
		default:
			fmt.Printf("updates: none newer on the %s channel\n", u.Channel)
		}
	}
	if s.Service != nil {
		switch {
		case s.Service.Running:
			fmt.Println("service: installed and running")
		case s.Service.Installed:
			fmt.Println("service: installed, not running")
		default:
			fmt.Println("service: not installed")
		}
	}
	return nil
}

func runOpen(args []string) error {
	fs := flag.NewFlagSet("open", flag.ContinueOnError)
	dir, _ := commonFlags(fs)
	noBrowser := fs.Bool("print", false, "only print the link")
	if err := fs.Parse(args); err != nil {
		return err
	}
	c, err := localClient(*dir)
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	link, err := c.LoginLink(ctx)
	if err != nil {
		return fmt.Errorf("the agent did not answer: %w", err)
	}
	fmt.Println(link)
	fmt.Fprintln(os.Stderr, "the link works once and for two minutes")
	if *noBrowser {
		return nil
	}
	var cmd *exec.Cmd
	switch runtime.GOOS {
	case "darwin":
		cmd = exec.Command("open", link)
	case "windows":
		cmd = exec.Command("rundll32", "url.dll,FileProtocolHandler", link)
	default:
		cmd = exec.Command("xdg-open", link)
	}
	if err := cmd.Start(); err != nil {
		return nil // the link is printed; a headless box has no browser
	}
	return nil
}

func runLogs(args []string) error {
	fs := flag.NewFlagSet("logs", flag.ContinueOnError)
	dir, _ := commonFlags(fs)
	n := fs.Int("n", 100, "lines")
	if err := fs.Parse(args); err != nil {
		return err
	}
	c, err := localClient(*dir)
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	lines, err := c.Logs(ctx, *n)
	if err != nil {
		return err
	}
	for _, l := range lines {
		var v struct {
			Time  time.Time `json:"time"`
			Level string    `json:"level"`
			Msg   string    `json:"msg"`
			Attrs string    `json:"attrs"`
		}
		if json.Unmarshal(l, &v) == nil {
			fmt.Printf("%s %-5s %s %s\n", v.Time.Format("15:04:05"), v.Level, v.Msg, v.Attrs)
		}
	}
	return nil
}
