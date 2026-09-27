package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"sort"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	"github.com/klauspost/compress/zstd"
	"github.com/spf13/cobra"

	"github.com/ddahan/dokwalt/internal/api"
	"github.com/ddahan/dokwalt/internal/client"
	"github.com/ddahan/dokwalt/internal/ui"
)

func deployCommands() []*cobra.Command {
	return []*cobra.Command{deployCmd(), releasesCmd(), rollbackCmd(), pipelineEnableCmd(), pipelineDisableCmd(), promoteCmd()}
}

func deployCmd() *cobra.Command {
	var (
		message string
		noCache bool
		files   []string
	)
	cmd := &cobra.Command{
		Use:   "deploy",
		Short: "Build on this machine, upload and roll out with zero downtime",
		Long: `Build the compose project in this folder on this machine for the server's
architecture, upload only the images that changed, and roll out a new release:
the new version starts next to the old one, must pass its health checks, then
traffic switches over. If anything fails, the current version keeps serving.`,
		Example: "  dokwalt deploy\n  dokwalt deploy -m \"New pricing page\"\n  dokwalt deploy -s staging",
		Args:    cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := cmd.Context()
			s, app, stage, err := appStage(ctx)
			if err != nil {
				return err
			}
			defer s.Close()
			return deploy(ctx, s, app, stage, deployOpts{message: message, noCache: noCache, files: files})
		},
	}
	cmd.Flags().StringVarP(&message, "message", "m", "", "release description (default: last commit subject)")
	cmd.Flags().BoolVar(&noCache, "no-cache", false, "build without cache")
	cmd.Flags().StringArrayVarP(&files, "file", "f", nil, "compose file(s) (default: compose.yaml / docker-compose.yml)")
	return cmd
}

type deployOpts struct {
	message string
	noCache bool
	files   []string
}

func dockerCmd(ctx context.Context, env []string, args ...string) *exec.Cmd {
	c := exec.CommandContext(ctx, "docker", args...)
	c.Env = append(os.Environ(), env...)
	return c
}

func composeArgs(files []string, rest ...string) []string {
	args := []string{"compose"}
	for _, f := range files {
		args = append(args, "-f", f)
	}
	return append(args, rest...)
}

func deploy(ctx context.Context, s *session, app, stage string, o deployOpts) error {
	if _, err := exec.LookPath("docker"); err != nil {
		return errors.New("docker is needed on this machine to build images — install Docker Desktop or OrbStack")
	}
	wd, _ := os.Getwd()
	fmt.Println(ui.TitleS.Render("◆ Deploying "+label(app, stage)) + ui.MutedS.Render(fmt.Sprintf("  to %s (linux/%s)", s.name, s.remote.Arch)))

	// 1. Read the compose model WITHOUT local interpolation: config lives on the server.
	out, err := dockerCmd(ctx, nil, composeArgs(o.files, "config", "--no-interpolate", "--format", "json")...).Output()
	if err != nil {
		var ee *exec.ExitError
		if errors.As(err, &ee) {
			return fmt.Errorf("reading the compose file failed:\n%s", strings.TrimSpace(string(ee.Stderr)))
		}
		return err
	}
	var model map[string]any
	if err := json.Unmarshal(out, &model); err != nil {
		return err
	}
	services, _ := model["services"].(map[string]any)
	var built []string
	for name, sv := range services {
		if m, ok := sv.(map[string]any); ok {
			if _, ok := m["build"]; ok {
				built = append(built, name)
			}
		}
	}
	sort.Strings(built)

	sha, subject := gitInfo()
	desc := o.message
	if desc == "" {
		desc = subject
	}
	platform := "linux/" + s.remote.Arch
	images := map[string]api.Image{}

	_, err = ui.Operation(func(emit func(api.Event)) (api.Event, error) {
		if len(built) > 0 {
			if err := buildImages(ctx, app, platform, model, built, o.noCache, images, emit); err != nil {
				return api.Event{}, err
			}
			if err := uploadImages(ctx, s, platform, images, emit); err != nil {
				return api.Event{}, err
			}
		}
		req := struct {
			api.DeployRequest
			ProjectDir string `json:"project_dir"`
		}{api.DeployRequest{Compose: model, Images: images, Description: desc, GitSHA: sha}, wd}
		return s.c.Operation(ctx, "POST", client.StagePath(app, stage, "/deploy"), req, emit)
	})
	if err != nil {
		return err
	}
	var doms []api.Domain
	_ = s.c.Get(ctx, client.StagePath(app, stage, "/domains"), &doms)
	fmt.Println()
	if len(doms) == 0 {
		ui.Success("%s is live — no domain yet", label(app, stage))
		ui.Hint("Add one: %s", ui.Code("dokwalt domains:add example.com --service <service>"))
	} else {
		var urls []string
		for _, d := range doms {
			if d.RedirectTo == "" {
				urls = append(urls, ui.BlueS.Underline(true).Render("https://"+d.Hostname))
			}
		}
		ui.Success("%s is live at %s", label(app, stage), strings.Join(urls, "  "))
	}
	return nil
}

func gitInfo() (sha, subject string) {
	if out, err := exec.Command("git", "rev-parse", "--short", "HEAD").Output(); err == nil {
		sha = strings.TrimSpace(string(out))
		if st, err := exec.Command("git", "status", "--porcelain").Output(); err == nil && len(bytes.TrimSpace(st)) > 0 {
			sha += "-dirty"
		}
	}
	if out, err := exec.Command("git", "log", "-1", "--format=%s").Output(); err == nil {
		subject = strings.TrimSpace(string(out))
	}
	return sha, subject
}

// buildImages builds services with a build section for the server's
// platform, then tags each image by content (its ID) so unchanged images
// are recognized and never uploaded twice.
func buildImages(ctx context.Context, app, platform string, model map[string]any, built []string, noCache bool, images map[string]api.Image, emit func(api.Event)) error {
	emit(api.Event{Step: "build", Status: "start", Message: fmt.Sprintf("Building %s for %s", strings.Join(built, ", "), platform)})
	services := model["services"].(map[string]any)
	doc := map[string]any{"name": "dokwalt-build-" + app, "services": map[string]any{}}
	for _, name := range built {
		sv := services[name].(map[string]any)
		doc["services"].(map[string]any)[name] = map[string]any{
			"build":    sv["build"],
			"image":    fmt.Sprintf("dokwalt/%s-%s:build", app, name),
			"platform": platform,
		}
	}
	tmp, err := os.CreateTemp("", "dokwalt-build-*.json")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	_ = json.NewEncoder(tmp).Encode(doc)
	tmp.Close()

	args := []string{"compose", "-f", tmp.Name(), "--progress", "plain", "build"}
	if noCache {
		args = append(args, "--no-cache")
	}
	// No provenance attestations: they embed timestamps, so identical builds
	// would get new IDs and be uploaded again for nothing.
	c := dockerCmd(ctx, []string{"DOCKER_DEFAULT_PLATFORM=" + platform, "BUILDKIT_PROGRESS=plain", "BUILDX_NO_DEFAULT_ATTESTATIONS=1"}, args...)
	if wd, err := os.Getwd(); err == nil {
		c.Dir = wd
	}
	if err := streamCmd(c, func(l string) { emit(api.Event{Step: "build", Status: "log", Message: l}) }); err != nil {
		return fmt.Errorf("build failed: %w", err)
	}
	for _, name := range built {
		ref := fmt.Sprintf("dokwalt/%s-%s:build", app, name)
		out, err := exec.CommandContext(ctx, "docker", "image", "inspect", "--format", "{{.Id}}", ref).Output()
		if err != nil {
			return fmt.Errorf("inspect %s: %w", ref, err)
		}
		id := strings.TrimSpace(string(out))
		short := strings.TrimPrefix(id, "sha256:")
		if len(short) > 12 {
			short = short[:12]
		}
		tagged := fmt.Sprintf("dokwalt/%s-%s:%s", app, name, short)
		if err := exec.CommandContext(ctx, "docker", "tag", ref, tagged).Run(); err != nil {
			return fmt.Errorf("tag %s: %w", tagged, err)
		}
		images[name] = api.Image{Ref: tagged, ID: id}
	}
	emit(api.Event{Step: "build", Status: "done", Message: "Built " + strings.Join(built, ", ")})
	return nil
}

func streamCmd(c *exec.Cmd, line func(string)) error {
	pr, pw := io.Pipe()
	c.Stdout, c.Stderr = pw, pw
	var tail []string
	done := make(chan struct{})
	go func() {
		defer close(done)
		buf := make([]byte, 32<<10)
		var acc string
		for {
			n, err := pr.Read(buf)
			acc += string(buf[:n])
			for {
				i := strings.IndexAny(acc, "\n\r")
				if i < 0 {
					break
				}
				l := strings.TrimSpace(acc[:i])
				acc = acc[i+1:]
				if l == "" {
					continue
				}
				tail = append(tail, l)
				if len(tail) > 15 {
					tail = tail[1:]
				}
				line(l)
			}
			if err != nil {
				return
			}
		}
	}()
	err := c.Run()
	pw.Close()
	<-done
	if err != nil {
		return fmt.Errorf("%w\n%s", err, strings.Join(tail, "\n"))
	}
	return nil
}

// uploadImages streams `docker save` of missing images, zstd-compressed, to the daemon.
func uploadImages(ctx context.Context, s *session, platform string, images map[string]api.Image, emit func(api.Event)) error {
	refs := make([]string, 0, len(images))
	unique := map[string]bool{}
	for _, img := range images {
		if !unique[img.Ref] {
			unique[img.Ref] = true
			refs = append(refs, img.Ref)
		}
	}
	sort.Strings(refs)
	var have api.ImagesHaveResponse
	if err := s.c.Post(ctx, "/v1/images/have", api.ImagesHaveRequest{IDs: refs}, &have); err != nil {
		return err
	}
	var missing []string
	for _, r := range refs {
		if !have.Have[r] {
			missing = append(missing, r)
		}
	}
	if len(missing) == 0 {
		emit(api.Event{Step: "upload", Status: "done", Message: "Images unchanged — nothing to upload"})
		return nil
	}
	emit(api.Event{Step: "upload", Status: "start", Message: "Uploading " + strings.Join(missing, ", ")})
	start := time.Now()
	var sent atomic.Int64
	stop := make(chan struct{})
	go func() {
		t := time.NewTicker(500 * time.Millisecond)
		defer t.Stop()
		for {
			select {
			case <-stop:
				return
			case <-t.C:
				n := sent.Load()
				rate := float64(n) / time.Since(start).Seconds()
				emit(api.Event{Step: "upload", Status: "progress", Message: fmt.Sprintf("Uploading images — %s sent (%s/s, compressed)", ui.Bytes(uint64(n)), ui.Bytes(uint64(rate)))})
			}
		}
	}()
	defer close(stop)

	pr, pw := io.Pipe()
	saveArgs := append([]string{"save", "--platform", platform}, missing...)
	save := exec.CommandContext(ctx, "docker", saveArgs...)
	var saveErr bytes.Buffer
	save.Stderr = &saveErr
	stdout, err := save.StdoutPipe()
	if err != nil {
		return err
	}
	if err := save.Start(); err != nil {
		return err
	}
	go func() {
		enc, _ := zstd.NewWriter(pw, zstd.WithEncoderLevel(zstd.SpeedDefault))
		_, err := io.Copy(enc, stdout)
		if cerr := enc.Close(); err == nil {
			err = cerr
		}
		if werr := save.Wait(); werr != nil && err == nil {
			err = fmt.Errorf("docker save: %v: %s", werr, strings.TrimSpace(saveErr.String()))
		}
		pw.CloseWithError(err)
	}()
	body := &countReader{r: pr, n: &sent}
	if err := s.c.Upload(ctx, "/v1/images/load", body); err != nil {
		if strings.Contains(saveErr.String(), "platform") {
			return fmt.Errorf("docker save failed (%s) — update Docker Desktop to 28+ for --platform support", strings.TrimSpace(saveErr.String()))
		}
		return err
	}
	rate := float64(sent.Load()) / time.Since(start).Seconds()
	emit(api.Event{Step: "upload", Status: "done", Message: fmt.Sprintf("Uploaded %s (%s/s)", ui.Bytes(uint64(sent.Load())), ui.Bytes(uint64(rate)))})
	return nil
}

type countReader struct {
	r io.Reader
	n *atomic.Int64
}

func (c *countReader) Read(p []byte) (int, error) {
	n, err := c.r.Read(p)
	c.n.Add(int64(n))
	return n, err
}

func releasesCmd() *cobra.Command {
	var limit int
	cmd := &cobra.Command{
		Use: "releases", Short: "List releases (newest first)",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			s, app, stage, err := appStage(cmd.Context())
			if err != nil {
				return err
			}
			defer s.Close()
			var rels []api.Release
			if err := s.c.Get(cmd.Context(), client.StagePath(app, stage, "/releases?limit="+strconv.Itoa(limit)), &rels); err != nil {
				return err
			}
			if gf.json {
				return printJSON(rels)
			}
			if len(rels) == 0 {
				ui.Info("No releases yet — run %s", ui.Code("dokwalt deploy"))
				return nil
			}
			var rows [][]string
			for _, r := range rels {
				v := fmt.Sprintf("v%d", r.Version)
				if r.Current {
					v = ui.GreenS.Render("▶ " + v)
				} else {
					v = "  " + v
				}
				status := r.Status
				if r.Current {
					status = "live"
				}
				desc := r.Description
				if r.Error != "" {
					desc = ui.RedS.Render(firstLine(r.Error))
				}
				if len([]rune(desc)) > 60 {
					desc = string([]rune(desc)[:59]) + "…"
				}
				rows = append(rows, []string{v, ui.Status(status), desc, ui.MutedS.Render(r.GitSHA), ui.MutedS.Render(fmt.Sprintf("c%d", r.ConfigVersion)), ui.MutedS.Render(ui.Ago(r.CreatedAt))})
			}
			fmt.Println(ui.Table([]string{"RELEASE", "STATUS", "DESCRIPTION", "COMMIT", "CONFIG", "CREATED"}, rows))
			return nil
		},
	}
	cmd.Flags().IntVarP(&limit, "limit", "n", 15, "number of releases")
	return cmd
}

func firstLine(s string) string {
	l, _, _ := strings.Cut(s, "\n")
	return l
}

func rollbackCmd() *cobra.Command {
	var withConfig bool
	cmd := &cobra.Command{
		Use: "rollback [vN]", Short: "Roll back to a previous release (instant, no rebuild)",
		Long:    "Roll back to a previous release (default: the one before the current). Images are already on the server, so it's fast. Current config is kept unless --with-config.",
		Example: "  dokwalt rollback\n  dokwalt rollback v12 --with-config",
		Args:    cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			version := 0
			if len(args) == 1 {
				v, err := strconv.Atoi(strings.TrimPrefix(args[0], "v"))
				if err != nil {
					return fmt.Errorf("invalid release %q (use e.g. v12)", args[0])
				}
				version = v
			}
			s, app, stage, err := appStage(cmd.Context())
			if err != nil {
				return err
			}
			defer s.Close()
			fmt.Println(ui.TitleS.Render("◆ Rolling back " + label(app, stage)))
			ev, err := runOp(cmd.Context(), s, "POST", client.StagePath(app, stage, "/rollback"), api.RollbackRequest{Version: version, WithConfig: withConfig})
			if err != nil {
				return err
			}
			ui.Success("Rolled back — v%d is live", ev.Release)
			return nil
		},
	}
	cmd.Flags().BoolVar(&withConfig, "with-config", false, "also restore that release's config vars")
	return cmd
}

func pipelineEnableCmd() *cobra.Command {
	return &cobra.Command{
		Use: "pipeline:enable", Short: "Add a staging stage to the app",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			s, app, _, err := appStage(cmd.Context())
			if err != nil {
				return err
			}
			defer s.Close()
			if _, err := runOp(cmd.Context(), s, "POST", "/v1/apps/"+app+"/pipeline", map[string]bool{"enabled": true}); err != nil {
				return err
			}
			ui.Success("Pipeline enabled for %s: staging → production", app)
			ui.Hint("1. %s", ui.Code("dokwalt domains:add staging.example.com -s staging --service web"))
			ui.Hint("2. %s", ui.Code("dokwalt deploy -s staging"))
			ui.Hint("3. %s  (same images, production config, no rebuild)", ui.Code("dokwalt promote"))
			return nil
		},
	}
}

func pipelineDisableCmd() *cobra.Command {
	var confirm string
	cmd := &cobra.Command{
		Use: "pipeline:disable", Short: "Remove the staging stage (its containers, volumes, config, domains)",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			s, app, _, err := appStage(cmd.Context())
			if err != nil {
				return err
			}
			defer s.Close()
			if confirm != app && !ui.ConfirmName("This deletes the staging stage of "+app+": containers, volumes, config and domains.", app) {
				return errors.New("aborted")
			}
			if _, err := runOp(cmd.Context(), s, "POST", "/v1/apps/"+app+"/pipeline", map[string]bool{"enabled": false}); err != nil {
				return err
			}
			ui.Success("Pipeline disabled for %s", app)
			return nil
		},
	}
	cmd.Flags().StringVar(&confirm, "confirm", "", "app name, to skip the interactive confirmation")
	return cmd
}

func promoteCmd() *cobra.Command {
	return &cobra.Command{
		Use: "promote", Short: "Release staging's current images to production (no rebuild)",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			s, app, _, err := appStage(cmd.Context())
			if err != nil {
				return err
			}
			defer s.Close()
			fmt.Println(ui.TitleS.Render("◆ Promoting "+app) + ui.MutedS.Render("  staging → production"))
			ev, err := runOp(cmd.Context(), s, "POST", "/v1/apps/"+app+"/promote", nil)
			if err != nil {
				return err
			}
			ui.Success("Promoted — production is now v%d", ev.Release)
			return nil
		},
	}
}
