package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/adefilippo83/redundanet/agent/internal/api"
	"github.com/adefilippo83/redundanet/agent/internal/backup"
	"github.com/adefilippo83/redundanet/agent/internal/snapshot"
)

// `backup` and `restore` are thin clients of the running agent's API, as
// the RFC's CLI section says: the agent does the work, the commands ask.

// parseInterspersed parses flags that come before or after positional
// arguments, which is how people type `restore start DEV SNAP --to DIR`.
func parseInterspersed(fs *flag.FlagSet, args []string) ([]string, error) {
	var positional []string
	for {
		if err := fs.Parse(args); err != nil {
			return nil, err
		}
		rest := fs.Args()
		if len(rest) == 0 {
			return positional, nil
		}
		i := 0
		for i < len(rest) && !strings.HasPrefix(rest[i], "-") {
			i++
		}
		positional = append(positional, rest[:i]...)
		if i == len(rest) {
			return positional, nil
		}
		args = rest[i:]
	}
}

func arg(pos []string, i int) string {
	if i < len(pos) {
		return pos[i]
	}
	return ""
}

func runBackup(args []string) error {
	if len(args) == 0 {
		return errors.New("backup: a subcommand is needed: add, remove, list, run, verify, cleanup, rebalance, limit, schedule, retention, exclude")
	}
	fs := flag.NewFlagSet("backup "+args[0], flag.ContinueOnError)
	dir, _ := commonFlags(fs)
	exclude := fs.String("exclude", "", "add: comma-separated excludes beside the defaults (base-name globs or relative prefixes)")
	dryRun := fs.Bool("dry-run", false, "cleanup: count what would be deleted and delete nothing")
	force := fs.Bool("force", false, "cleanup: lift the bound on how much one run may delete")
	upload := fs.Int("upload", -1, "limit: kilobytes per second this device sends over the mesh, 0 for no cap")
	every := fs.String("every", "", "schedule: between runs of each set, for example 24h (at most hourly)")
	verifyEvery := fs.String("verify-every", "", "schedule: between restore tests, for example 168h (at most daily)")
	cleanupEvery := fs.String("cleanup-every", "", "schedule: between cleanups, for example 168h (at most daily)")
	days := fs.Int("days", -1, "retention: keep everything this many days")
	daily := fs.Int("daily", -1, "retention: then one snapshot a day for this many days")
	weekly := fs.Int("weekly", -1, "retention: then one a week for this many weeks")
	monthly := fs.Int("monthly", -1, "retention: then one a month for this many months")
	download := fs.Int("download", -1, "limit: kilobytes per second this device receives over the mesh, 0 for no cap")
	pos, err := parseInterspersed(fs, args[1:])
	if err != nil {
		return err
	}
	c, err := localClient(*dir)
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	switch args[0] {
	case "add":
		if len(pos) < 2 {
			return errors.New("usage: backup add NAME FOLDER [FOLDER...] [--exclude a,b]")
		}
		set := backup.Set{Name: arg(pos, 0), Folders: pos[1:]}
		if *exclude != "" {
			set.Excludes = strings.Split(*exclude, ",")
		}
		if err := c.BackupAdd(ctx, set); err != nil {
			return err
		}
		fmt.Printf("set %s saved: %s\n", set.Name, strings.Join(set.Folders, ", "))
		return nil
	case "remove":
		if len(pos) != 1 {
			return errors.New("usage: backup remove NAME")
		}
		return c.BackupRemove(ctx, arg(pos, 0))
	case "list":
		st, err := c.Backups(ctx)
		if err != nil {
			return err
		}
		if len(st.Sets) == 0 {
			fmt.Println("no backup set; add one with: redundanet backup add NAME FOLDER")
			return nil
		}
		for _, rs := range st.Runs {
			var set *backup.Set
			for i := range st.Sets {
				if st.Sets[i].Name == rs.Set {
					set = &st.Sets[i]
				}
			}
			fmt.Printf("%s: %s\n", rs.Set, strings.Join(set.Folders, ", "))
			for _, n := range rs.Needs {
				fmt.Printf("  needs attention: %s\n", n)
			}
			switch {
			case rs.Running && rs.Progress != nil:
				fmt.Printf("  running: %s, %d files, %d bytes sent\n", rs.Progress.Phase, rs.Progress.Files, rs.Progress.Uploaded)
			case rs.LastErr != "":
				fmt.Printf("  last run %s failed: %s\n", rs.LastAt.Format(time.RFC3339), rs.LastErr)
			case rs.Last != nil:
				s := rs.Last.Snapshot
				fmt.Printf("  last run %s: %d files, %d bytes, %d sent, %d skipped%s, root %d\n", rs.LastAt.Format(time.RFC3339), s.Files, s.Bytes, s.Uploaded, s.Skipped, map[bool]string{true: ", partial", false: ""}[s.Partial], rs.Last.RootVersion)
				if why := rs.Last.SkipReasons(); why != "" {
					fmt.Printf("  skipped: %s\n", why)
				}
			default:
				fmt.Println("  not run yet")
			}
		}
		fmt.Printf("every %s; restore test every %s; cleanup every %s\n", st.Every, st.VerifyEvery, st.CleanupEvery)
		fmt.Printf("retention: everything %d days, then daily for %d days, weekly for %d weeks, monthly for %d months\n", st.Retention.Days, st.Retention.Daily, st.Retention.Weekly, st.Retention.Monthly)
		if b := st.Budget; b != nil {
			fmt.Printf("allocation: %.1f of %d GB used", b.UsedGB, b.AllowedGB)
			if b.Exhausted {
				fmt.Print(" (used up: prune snapshots or ask the operator for a larger allocation)")
			} else if b.Answered < b.Servers {
				fmt.Printf(" (from %d of %d servers)", b.Answered, b.Servers)
			}
			fmt.Println()
		}
		if st.UploadKBps > 0 || st.DownloadKBps > 0 {
			fmt.Printf("bandwidth: upload %s, download %s\n", capWord(st.UploadKBps), capWord(st.DownloadKBps))
		}
		if v := st.LastVerify; v != nil {
			fmt.Printf("last verified restore %s: %d of %d files read back, %d compared with unchanged sources, %d failed\n", v.At.Format(time.RFC3339), v.Verified, v.Sampled, v.Compared, len(v.Failed))
		}
		if st.VerifyStreak > 0 {
			fmt.Printf("restore tests passing %d weeks in a row\n", st.VerifyStreak)
		}
		for i, v := range st.Verifications {
			if i == 0 {
				fmt.Println("restore tests:")
			}
			result := "passed"
			if !v.Passed() {
				result = fmt.Sprintf("%d failed", len(v.Failed))
			}
			fmt.Printf("  %s  snapshot %s: %d of %d read back, %d compared, %s\n", v.At.Format("2006-01-02 15:04"), v.Snapshot, v.Verified, v.Sampled, v.Compared, result)
		}
		return nil
	case "run":
		if err := c.BackupRun(ctx, arg(pos, 0)); err != nil {
			return err
		}
		fmt.Println("backup requested; follow it with: redundanet backup list")
		return nil
	case "verify":
		if err := c.BackupVerify(ctx); err != nil {
			return err
		}
		fmt.Println("restore test requested; the result shows in: redundanet backup list")
		return nil
	case "cleanup":
		lctx, lcancel := context.WithTimeout(context.Background(), 30*time.Minute)
		defer lcancel()
		rep, err := c.BackupCleanup(lctx, *dryRun, *force)
		if err != nil {
			return err
		}
		what := "deleted"
		if rep.DryRun {
			what = "would be deleted"
		}
		fmt.Printf("roots: %d walked from %d of %d servers; %d objects reachable; %d held for the member\n", rep.Roots, rep.RootsAnswered, rep.Servers, rep.Reachable, rep.Listed)
		fmt.Printf("garbage: %d; %s: %d; waiting out the immunity: %d; holders not all answering: %d; failed: %d\n", rep.Garbage, what, rep.Tombstoned, rep.Recent, rep.Incomplete, rep.Failed)
		if rep.Error != "" {
			return errors.New(rep.Error)
		}
		return nil
	case "schedule":
		if *every == "" && *verifyEvery == "" && *cleanupEvery == "" {
			return errors.New("usage: backup schedule [--every 24h] [--verify-every 168h] [--cleanup-every 168h]")
		}
		var s api.Settings
		if *every != "" {
			s.Every = every
		}
		if *verifyEvery != "" {
			s.VerifyEvery = verifyEvery
		}
		if *cleanupEvery != "" {
			s.CleanupEvery = cleanupEvery
		}
		if err := c.Settings(ctx, s); err != nil {
			return err
		}
		fmt.Println("schedule saved; the scheduler uses it from now")
		return nil
	case "retention":
		if *days < 0 && *daily < 0 && *weekly < 0 && *monthly < 0 {
			return errors.New("usage: backup retention [--days N] [--daily N] [--weekly N] [--monthly N]")
		}
		st, err := c.Backups(ctx)
		if err != nil {
			return err
		}
		ret := st.Retention
		for v, dst := range map[*int]*int{days: &ret.Days, daily: &ret.Daily, weekly: &ret.Weekly, monthly: &ret.Monthly} {
			if *v >= 0 {
				*dst = *v
			}
		}
		if err := c.Settings(ctx, api.Settings{Retention: &ret}); err != nil {
			return err
		}
		fmt.Printf("retention: everything %d days, then daily for %d days, weekly for %d weeks, monthly for %d months\n", ret.Days, ret.Daily, ret.Weekly, ret.Monthly)
		return nil
	case "exclude":
		if len(pos) != 1 {
			return errors.New("usage: backup exclude NAME --exclude a,b   (replaces the set's own excludes; --exclude \"\" clears them)")
		}
		var excludes []string
		for _, e := range strings.Split(*exclude, ",") {
			if e = strings.TrimSpace(e); e != "" {
				excludes = append(excludes, e)
			}
		}
		if err := c.BackupExcludes(ctx, arg(pos, 0), excludes); err != nil {
			return err
		}
		fmt.Printf("excludes of %s: %s\n", arg(pos, 0), strings.Join(append([]string{}, excludes...), ", "))
		return nil
	case "limit":
		if *upload < 0 && *download < 0 {
			return errors.New("usage: backup limit [--upload KBPS] [--download KBPS] (kilobytes per second, 0 for no cap)")
		}
		st, err := c.Backups(ctx)
		if err != nil {
			return err
		}
		up, down := st.UploadKBps, st.DownloadKBps
		if *upload >= 0 {
			up = *upload
		}
		if *download >= 0 {
			down = *download
		}
		if err := c.BackupLimit(ctx, up, down); err != nil {
			return err
		}
		fmt.Printf("bandwidth: upload %s, download %s\n", capWord(up), capWord(down))
		return nil
	case "rebalance":
		lctx, lcancel := context.WithTimeout(context.Background(), 12*time.Hour)
		defer lcancel()
		st, err := c.BackupRebalance(lctx)
		if err != nil {
			return err
		}
		if st.Nothing {
			fmt.Println("nothing to convert: no encoding transition in the manifest, or no backup yet")
			return nil
		}
		fmt.Printf("%s to %s: %d of %d snapshots converted, %d objects re-encoded, %d bytes\n", st.From, st.To, st.Done, st.Snapshots, st.Objects, st.Bytes)
		if st.Err != "" {
			return errors.New(st.Err)
		}
		return nil
	}
	return fmt.Errorf("backup: unknown subcommand %q", args[0])
}

// capWord words one cap.
func capWord(kbps int) string {
	if kbps <= 0 {
		return "no cap"
	}
	return fmt.Sprintf("%d kB/s", kbps)
}

func runRestore(args []string) error {
	if len(args) == 0 {
		return errors.New("restore: a subcommand is needed: list, browse, start, status")
	}
	fs := flag.NewFlagSet("restore "+args[0], flag.ContinueOnError)
	dir, _ := commonFlags(fs)
	to := fs.String("to", "", "start: the directory to restore into")
	overwrite := fs.Bool("overwrite", false, "start: replace files that exist")
	pos, err := parseInterspersed(fs, args[1:])
	if err != nil {
		return err
	}
	c, err := localClient(*dir)
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	switch args[0] {
	case "list":
		l, err := c.RestoreList(ctx)
		if err != nil {
			return err
		}
		fmt.Printf("roots from %d of %d servers\n", l.Answered, l.Servers)
		for _, d := range l.Devices {
			fmt.Printf("device %s (root version %d)\n", d.Device.String(), d.Version)
			if d.Err != "" {
				fmt.Printf("  %s\n", d.Err)
				continue
			}
			for _, s := range d.Index.Snapshots {
				fmt.Printf("  %s  set %s, %d files, %d bytes%s\n", s.ID, s.Set, s.Files, s.Bytes, map[bool]string{true: ", partial", false: ""}[s.Partial])
			}
		}
		return nil
	case "browse":
		if len(pos) < 2 {
			return errors.New("usage: restore browse DEVICE SNAPSHOT [PATH]")
		}
		entry, d, err := c.RestoreBrowse(ctx, arg(pos, 0), arg(pos, 1), arg(pos, 2))
		if err != nil {
			return err
		}
		if d == nil {
			fmt.Printf("%s %s %d bytes\n", entry.Kind, entry.NameString(), entry.Size)
			return nil
		}
		for _, e := range d.Entries {
			switch e.Kind {
			case snapshot.KindDir:
				fmt.Printf("dir      %s/  (%d files, %d bytes)\n", e.NameString(), e.Entries, e.Bytes)
			case snapshot.KindFile:
				fmt.Printf("file     %s  %d bytes\n", e.NameString(), e.Size)
			case snapshot.KindSymlink:
				fmt.Printf("symlink  %s -> %s\n", e.NameString(), e.Target)
			default:
				fmt.Printf("skipped  %s  (%s)\n", e.NameString(), e.Reason)
			}
		}
		return nil
	case "start":
		if len(pos) < 2 || *to == "" {
			return errors.New("usage: restore start DEVICE SNAPSHOT [PATH] --to DIR [--overwrite]")
		}
		if err := c.RestoreStart(ctx, api.RestoreRequest{Device: arg(pos, 0), Snapshot: arg(pos, 1), Path: arg(pos, 2), Target: *to, Overwrite: *overwrite}); err != nil {
			return err
		}
		fmt.Println("restore started; follow it with: redundanet restore status")
		return nil
	case "status":
		st, err := c.Backups(ctx)
		if err != nil {
			return err
		}
		r := st.Restore
		switch {
		case r == nil:
			fmt.Println("no restore yet")
		case r.Running:
			fmt.Printf("running: %s from snapshot %s to %s\n", r.Path, r.Snap, r.Target)
		case r.Err != "":
			fmt.Printf("failed: %s\n", r.Err)
		default:
			fmt.Printf("done %s: %d files, %d bytes, %d directories, %d symlinks to %s\n", r.At.Format(time.RFC3339), r.Report.Files, r.Report.Bytes, r.Report.Dirs, r.Report.Symlinks, r.Target)
			for _, e := range r.Report.Exceptions {
				fmt.Printf("  exception: %s\n", e)
			}
			if len(r.Report.Exceptions) > 0 {
				fmt.Println("complete with exceptions")
			}
		}
		return nil
	}
	return fmt.Errorf("restore: unknown subcommand %q", args[0])
}

var _ = os.Stdout
