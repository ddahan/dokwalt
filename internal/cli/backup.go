package cli

import (
	"bufio"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/spf13/cobra"
	"golang.org/x/term"

	"github.com/ddahan/dokwalt/internal/api"
	"github.com/ddahan/dokwalt/internal/backup"
	"github.com/ddahan/dokwalt/internal/ui"
)

// Off-site backups: the daemon dumps and uploads; these commands set it up,
// list, download and restore through the daemon, so the bucket's
// credentials never leave the server.

func backupCommands() []*cobra.Command {
	return []*cobra.Command{backupSetupCmd(), backupDisableCmd(), backupsCmd(), backupNowCmd(), backupDownloadCmd(), backupRestoreCmd()}
}

func backupSetupCmd() *cobra.Command {
	var (
		in          api.BackupConfig
		r2Account   string
		secretStdin bool
	)
	cmd := &cobra.Command{
		Use:   "backup:setup",
		Short: "Back up every database and DokWalt's state every night to Cloudflare R2 (or any S3 storage)",
		Long: "Store the bucket's settings on the server and check them by writing, listing and deleting a test object. " +
			"Every night at --time (server local time) the server dumps each database of each Postgres service, " +
			"their roles and DokWalt's own state, uploads them, and keeps the newest backup of the last --keep-daily days " +
			"and --keep-weekly weeks. Run it again to change a setting: flags you leave out keep their value.\n\n" +
			"The secret key is read from a prompt, from --secret-key-stdin, or from DOKWALT_BACKUP_SECRET_KEY — " +
			"never from a flag, so it stays out of your shell history. It's stored encrypted on the server.",
		Example: "  dokwalt backup:setup --r2-account 1a2b3c… --bucket backups --access-key 7f8e…\n" +
			"  dokwalt backup:setup --time 04:30 --keep-daily 14\n" +
			"  dokwalt backup:setup --endpoint https://s3.eu-west-3.amazonaws.com --region eu-west-3 --bucket my-backups --access-key AKIA…",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			s, err := connect(cmd.Context())
			if err != nil {
				return err
			}
			defer s.Close()
			if r2Account != "" {
				if in.Endpoint != "" {
					return errors.New("use --r2-account or --endpoint, not both")
				}
				in.Endpoint = "https://" + r2Account + ".r2.cloudflarestorage.com"
				in.Region = "auto"
			}
			var cur api.BackupStatus
			if err := s.c.Get(cmd.Context(), "/v1/backups/config", &cur); err != nil {
				return err
			}
			switch {
			case secretStdin:
				line, err := bufio.NewReader(os.Stdin).ReadString('\n')
				if err != nil && !errors.Is(err, io.EOF) {
					return err
				}
				in.SecretKey = strings.TrimSpace(line)
			case os.Getenv("DOKWALT_BACKUP_SECRET_KEY") != "":
				in.SecretKey = os.Getenv("DOKWALT_BACKUP_SECRET_KEY")
			case !cur.Configured || in.AccessKey != "":
				// A new access key comes with its own secret.
				if !term.IsTerminal(int(os.Stdin.Fd())) {
					return errors.New("missing secret key: use --secret-key-stdin or DOKWALT_BACKUP_SECRET_KEY")
				}
				fmt.Print(ui.YellowS.Render("? ") + "Secret access key: ")
				b, err := term.ReadPassword(int(os.Stdin.Fd()))
				fmt.Println()
				if err != nil {
					return err
				}
				in.SecretKey = strings.TrimSpace(string(b))
			}
			var st api.BackupStatus
			if err := ui.Spin("Checking the bucket (write, list, delete a test object)", func() error {
				return s.c.Post(cmd.Context(), "/v1/backups/config", in, &st)
			}); err != nil {
				return err
			}
			ui.Success("Backups set up on %s", s.name)
			fmt.Println()
			printBackupStatus(st)
			if st.Last == nil {
				fmt.Println()
				ui.Hint("Make the first one now: %s", ui.Code("dokwalt backup:now"))
			}
			return nil
		},
	}
	f := cmd.Flags()
	f.StringVar(&r2Account, "r2-account", "", "Cloudflare account ID (sets the R2 endpoint)")
	f.StringVar(&in.Endpoint, "endpoint", "", "S3 endpoint URL, for other providers")
	f.StringVar(&in.Region, "region", "", `S3 region (default "auto", right for R2)`)
	f.StringVar(&in.Bucket, "bucket", "", "bucket name")
	f.StringVar(&in.AccessKey, "access-key", "", "access key ID")
	f.BoolVar(&secretStdin, "secret-key-stdin", false, "read the secret access key from stdin")
	f.StringVar(&in.Prefix, "prefix", "", `folder in the bucket (default "dokwalt/<server hostname>")`)
	f.StringVar(&in.Time, "time", "", `daily backup time, server local time (default "03:00")`)
	f.IntVar(&in.KeepDaily, "keep-daily", 0, "days to keep the newest backup of (default 7)")
	f.IntVar(&in.KeepWeekly, "keep-weekly", 0, "weeks to keep the newest backup of (default 4)")
	return cmd
}

func backupDisableCmd() *cobra.Command {
	return &cobra.Command{
		Use: "backup:disable", Short: "Stop nightly backups and forget the bucket's settings (backups in the bucket stay)",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			s, err := connect(cmd.Context())
			if err != nil {
				return err
			}
			defer s.Close()
			if err := s.c.Delete(cmd.Context(), "/v1/backups/config", nil); err != nil {
				return err
			}
			ui.Success("Backups turned off on %s", s.name)
			ui.Hint("Existing backups stay in the bucket. Set up again with %s", ui.Code("dokwalt backup:setup"))
			return nil
		},
	}
}

func printBackupStatus(st api.BackupStatus) {
	if !st.Configured {
		ui.Warn("Backups aren't set up")
		ui.Hint("Set them up with %s — see %s", ui.Code("dokwalt backup:setup"), ui.Code("dokwalt docs backups"))
		return
	}
	next := st.Next.Format("Mon 2 Jan 15:04")
	if st.Running {
		next = ui.YellowS.Render("running now")
	} else if !st.Next.After(time.Now()) {
		next = "within a minute"
	}
	fmt.Println(ui.KV(
		"Storage", st.Bucket+"/"+st.Prefix+ui.MutedS.Render("  "+st.Endpoint),
		"Schedule", fmt.Sprintf("every day at %s %s · next %s", st.Time, st.TimeZone, next),
		"Retention", fmt.Sprintf("the newest backup of the last %d days and %d weeks", st.KeepDaily, st.KeepWeekly),
		"Last run", lastRun(st.Last),
	))
}

func lastRun(r *api.BackupRun) string {
	if r == nil {
		return ui.MutedS.Render("never")
	}
	when := ui.Ago(r.Started)
	switch r.Status {
	case "ok":
		return ui.GreenS.Render("✓ ok") + fmt.Sprintf(" %s · %s, %d files in %s", when, ui.Bytes(uint64(r.Size)), r.Files, ui.Duration(time.Duration(r.Duration)*time.Millisecond))
	case "partial":
		return ui.YellowS.Render("! incomplete") + " " + when + ui.MutedS.Render("\n"+indent(r.Error))
	}
	return ui.RedS.Render("✗ failed") + " " + when + ui.MutedS.Render("\n"+indent(r.Error))
}

func indent(s string) string {
	return "  " + strings.ReplaceAll(strings.TrimSpace(s), "\n", "\n  ")
}

func backupsCmd() *cobra.Command {
	return &cobra.Command{
		Use: "backups [id]", Short: "Show the backup setup and the backups in the bucket, or the files of one backup",
		Example: "  dokwalt backups\n  dokwalt backups latest",
		Args:    cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			s, err := connect(cmd.Context())
			if err != nil {
				return err
			}
			defer s.Close()
			if len(args) == 1 {
				var m backup.Manifest
				if err := s.c.Get(cmd.Context(), "/v1/backups/"+url.PathEscape(args[0]), &m); err != nil {
					return err
				}
				if gf.json {
					return printJSON(m)
				}
				printManifest(m)
				return nil
			}
			var st api.BackupStatus
			if err := s.c.Get(cmd.Context(), "/v1/backups/config", &st); err != nil {
				return err
			}
			if !st.Configured {
				if gf.json {
					return printJSON(api.BackupList{Status: st, Backups: []backup.Manifest{}})
				}
				printBackupStatus(st)
				return nil
			}
			var list api.BackupList
			if err := s.c.Get(cmd.Context(), "/v1/backups", &list); err != nil {
				return err
			}
			if gf.json {
				return printJSON(list)
			}
			printBackupStatus(list.Status)
			fmt.Println()
			if len(list.Backups) == 0 {
				ui.Hint("No backups in the bucket yet — %s makes one now", ui.Code("dokwalt backup:now"))
				return nil
			}
			var rows [][]string
			for _, m := range list.Backups {
				status := ui.GreenS.Render("complete")
				if len(m.Errors) > 0 {
					status = ui.YellowS.Render(fmt.Sprintf("%d problem(s)", len(m.Errors)))
				}
				rows = append(rows, []string{m.ID, m.Started.Local().Format("Mon 2 Jan 15:04"), m.Trigger, ui.Bytes(uint64(m.Size())), fmt.Sprint(len(m.Files)), status})
			}
			fmt.Println(ui.Table([]string{"ID", "STARTED", "TRIGGER", "SIZE", "FILES", "STATUS"}, rows))
			ui.Hint("Files of one backup: %s · restore: %s", ui.Code("dokwalt backups <id>"), ui.Code("dokwalt backup:restore <id> --database <name>"))
			return nil
		},
	}
}

func printManifest(m backup.Manifest) {
	fmt.Println(ui.TitleS.Render("◆ Backup "+m.ID) + ui.MutedS.Render(fmt.Sprintf("  %s · %s · %s · %s",
		m.Started.Local().Format("Mon 2 Jan 2006 15:04"), m.Trigger, m.Host, ui.Duration(time.Duration(m.DurationMS)*time.Millisecond))))
	fmt.Println()
	var rows [][]string
	for _, f := range m.Files {
		what := "DokWalt state"
		switch f.Kind {
		case backup.KindPostgres:
			what = "database " + f.Database
		case backup.KindGlobals:
			what = "roles"
		}
		from := ""
		if f.App != "" {
			from = f.App + "/" + f.Stage + " " + f.Service
		}
		rows = append(rows, []string{f.Path, what, from, ui.Bytes(uint64(f.Size))})
	}
	fmt.Println(ui.Table([]string{"FILE", "CONTENT", "FROM", "SIZE"}, rows))
	for _, e := range m.Errors {
		ui.Warn("Not backed up: %s", e)
	}
	ui.Hint("Download: %s · restore: %s", ui.Code("dokwalt backup:download "+m.ID+" [file]"), ui.Code("dokwalt backup:restore "+m.ID+" <file>"))
}

func backupNowCmd() *cobra.Command {
	return &cobra.Command{
		Use: "backup:now", Short: "Make an off-site backup right away",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			s, err := connect(cmd.Context())
			if err != nil {
				return err
			}
			defer s.Close()
			ev, err := runOp(cmd.Context(), s, "POST", "/v1/backups/run", nil)
			if err != nil {
				return err
			}
			ui.Success("Backup %s uploaded", ui.Bold.Render(ev.Message))
			ui.Hint("See it: %s", ui.Code("dokwalt backups "+ev.Message))
			return nil
		},
	}
}

func backupDownloadCmd() *cobra.Command {
	var output string
	cmd := &cobra.Command{
		Use: "backup:download <id> [file]", Short: "Download a backup, or one of its files, to this machine",
		Long: "Download through the server (the bucket's credentials stay there) and check every file against the " +
			"checksum recorded at backup time. Without a file, the whole backup goes into a folder named after it.",
		Example: "  dokwalt backup:download latest\n" +
			"  dokwalt backup:download 20261010T030000Z postgres-production-db/blog.dump\n" +
			"  dokwalt backup:download latest dokwalt.db -o ~/Backups",
		Args: cobra.RangeArgs(1, 2),
		RunE: func(cmd *cobra.Command, args []string) error {
			s, err := connect(cmd.Context())
			if err != nil {
				return err
			}
			defer s.Close()
			var m backup.Manifest
			if err := s.c.Get(cmd.Context(), "/v1/backups/"+url.PathEscape(args[0]), &m); err != nil {
				return err
			}
			files := m.Files
			dir := output
			if len(args) == 2 {
				f, ok := m.File(args[1])
				if !ok {
					return fmt.Errorf("no file %q in backup %s — list them with `dokwalt backups %s`", args[1], m.ID, m.ID)
				}
				files = []backup.File{f}
				if dir == "" {
					dir = "."
				}
			} else if dir == "" {
				dir = m.Host + "-" + m.ID
			}
			for _, f := range files {
				// One backup file per folder level at most, from our own
				// manifest; still, never write outside dir.
				local := filepath.Join(dir, filepath.FromSlash(f.Path))
				if len(args) == 2 {
					local = filepath.Join(dir, filepath.Base(f.Path))
				}
				if rel, err := filepath.Rel(dir, local); err != nil || strings.HasPrefix(rel, "..") {
					return fmt.Errorf("refusing to write %s outside %s", f.Path, dir)
				}
				if err := downloadBackupFile(cmd, s, m.ID, f, local); err != nil {
					return err
				}
			}
			if len(args) == 1 {
				ui.Success("Backup %s saved in %s (%s, %d files)", m.ID, ui.Bold.Render(dir), ui.Bytes(uint64(m.Size())), len(files))
			}
			return nil
		},
	}
	cmd.Flags().StringVarP(&output, "output", "o", "", "folder to write into (default: <host>-<id> for a whole backup, the current folder for one file)")
	return cmd
}

func downloadBackupFile(cmd *cobra.Command, s *session, id string, f backup.File, local string) error {
	if err := os.MkdirAll(filepath.Dir(local), 0o755); err != nil {
		return err
	}
	part := local + ".part"
	out, err := os.OpenFile(part, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	defer os.Remove(part) // no-op once renamed
	h := sha256.New()
	err = ui.Spin(fmt.Sprintf("Downloading %s (%s)", f.Path, ui.Bytes(uint64(f.Size))), func() error {
		_, err := s.c.Download(cmd.Context(), "/v1/backups/"+url.PathEscape(id)+"/files/"+escapeBackupPath(f.Path), io.MultiWriter(out, h))
		return err
	})
	if cerr := out.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		return fmt.Errorf("download %s: %w", f.Path, err)
	}
	if hex.EncodeToString(h.Sum(nil)) != f.SHA256 {
		return fmt.Errorf("%s doesn't match its checksum: the download or the backup is damaged", f.Path)
	}
	if err := os.Rename(part, local); err != nil {
		return err
	}
	ui.Success("Saved %s (%s, checksum OK)", ui.Bold.Render(local), ui.Bytes(uint64(f.Size)))
	return nil
}

func escapeBackupPath(p string) string {
	parts := strings.Split(p, "/")
	for i, s := range parts {
		parts[i] = url.PathEscape(s)
	}
	return strings.Join(parts, "/")
}

func backupRestoreCmd() *cobra.Command {
	var database, confirm string
	cmd := &cobra.Command{
		Use:   "backup:restore <id> [file]",
		Short: "Restore a database (or a server's roles) from an off-site backup",
		Long: "Download the file on the server, check its checksum, and restore it into the Postgres service it came from, " +
			"as that server's superuser: a database dump replaces the database's objects (pg_restore --clean), or creates " +
			"the database if it's missing; globals.sql adds missing roles. The app keeps running: stop it first " +
			"(`dokwalt stop -a <app>`) if it writes a lot. On a fresh server, restore globals.sql before the dumps.",
		Example: "  dokwalt backup:restore latest --database blog\n" +
			"  dokwalt backup:restore 20261010T030000Z postgres-production-db/globals.sql\n" +
			"  dokwalt backup:restore latest postgres-production-db/blog.dump --confirm blog",
		Args: cobra.RangeArgs(1, 2),
		RunE: func(cmd *cobra.Command, args []string) error {
			s, err := connect(cmd.Context())
			if err != nil {
				return err
			}
			defer s.Close()
			var m backup.Manifest
			if err := s.c.Get(cmd.Context(), "/v1/backups/"+url.PathEscape(args[0]), &m); err != nil {
				return err
			}
			f, err := pickRestoreFile(m, args[1:], database)
			if err != nil {
				return err
			}
			where := fmt.Sprintf("%s/%s (service %s)", f.App, f.Stage, f.Service)
			switch f.Kind {
			case backup.KindPostgres:
				if confirm != f.Database && !ui.ConfirmName(fmt.Sprintf("This replaces database %s in %s with its copy from %s (%s).",
					f.Database, where, m.ID, m.Started.Local().Format("Mon 2 Jan 15:04")), f.Database) {
					return errors.New("aborted")
				}
			case backup.KindGlobals:
				if confirm != "roles" && !ui.Confirm(fmt.Sprintf("Add the roles missing from %s, from backup %s?", where, m.ID)) {
					return errors.New("aborted")
				}
			}
			if _, err := runOp(cmd.Context(), s, "POST", "/v1/backups/"+url.PathEscape(m.ID)+"/restore", api.BackupRestore{File: f.Path}); err != nil {
				return err
			}
			ui.Success("Restored %s from backup %s", f.Path, m.ID)
			return nil
		},
	}
	cmd.Flags().StringVar(&database, "database", "", "database to restore (instead of a file path)")
	cmd.Flags().StringVar(&confirm, "confirm", "", `database name (or "roles" for globals.sql), to skip the interactive confirmation`)
	return cmd
}

// pickRestoreFile finds the file to restore from a path or a database name.
func pickRestoreFile(m backup.Manifest, args []string, database string) (backup.File, error) {
	if len(args) == 1 {
		f, ok := m.File(args[0])
		if !ok {
			return f, fmt.Errorf("no file %q in backup %s — list them with `dokwalt backups %s`", args[0], m.ID, m.ID)
		}
		return f, nil
	}
	if database == "" {
		return backup.File{}, fmt.Errorf("pass a file or --database — list the files with `dokwalt backups %s`", m.ID)
	}
	var found []backup.File
	for _, f := range m.Files {
		if f.Kind == backup.KindPostgres && f.Database == database {
			found = append(found, f)
		}
	}
	switch len(found) {
	case 0:
		return backup.File{}, fmt.Errorf("no database %q in backup %s — list them with `dokwalt backups %s`", database, m.ID, m.ID)
	case 1:
		return found[0], nil
	}
	var paths []string
	for _, f := range found {
		paths = append(paths, f.Path)
	}
	return backup.File{}, fmt.Errorf("several services have a database %q — pass the file: %s", database, strings.Join(paths, ", "))
}

// backupSummary is the one-line backup state of `server info`.
func backupSummary(st api.BackupStatus) string {
	if !st.Configured {
		return ui.YellowS.Render("not set up") + ui.MutedS.Render("  dokwalt backup:setup")
	}
	where := ui.MutedS.Render(fmt.Sprintf("  → %s/%s, daily at %s", st.Bucket, st.Prefix, st.Time))
	if st.Last == nil {
		return "no run yet" + where
	}
	return strings.SplitN(lastRun(st.Last), "\n", 2)[0] + where
}
