package cli

import (
	"fmt"
	"os"
	"os/exec"
	"strings"

	"github.com/charmbracelet/glamour"
	"github.com/spf13/cobra"
	"golang.org/x/term"

	"github.com/dokwalt/dokwalt/internal/docs"
	"github.com/dokwalt/dokwalt/internal/ui"
)

func docsCmd() *cobra.Command {
	var search string
	var noPager bool
	cmd := &cobra.Command{
		Use:     "docs [topic]",
		Short:   "Read the built-in documentation",
		Example: "  dokwalt docs\n  dokwalt docs raspberry-pi\n  dokwalt docs --search rollback",
		Args:    cobra.MaximumNArgs(1),
		ValidArgsFunction: func(*cobra.Command, []string, string) ([]string, cobra.ShellCompDirective) {
			var names []string
			for _, t := range docs.Topics() {
				names = append(names, t.Name)
			}
			return names, cobra.ShellCompDirectiveNoFileComp
		},
		RunE: func(cmd *cobra.Command, args []string) error {
			if search != "" {
				res := docs.Search(search)
				if len(res) == 0 {
					ui.Info("Nothing matches %q", search)
					return nil
				}
				printTopics(res)
				return nil
			}
			if len(args) == 0 {
				fmt.Println(ui.TitleS.Render("◆ DokWalt documentation"))
				fmt.Println()
				printTopics(docs.Topics())
				fmt.Println()
				ui.Hint("Read one: %s   ·   search: %s", ui.Code("dokwalt docs <topic>"), ui.Code("dokwalt docs --search <words>"))
				return nil
			}
			body, ok := docs.Get(args[0])
			if !ok {
				var close []string
				for _, t := range docs.Topics() {
					if strings.Contains(t.Name, args[0]) || strings.Contains(args[0], t.Name) {
						close = append(close, t.Name)
					}
				}
				msg := fmt.Sprintf("no topic %q", args[0])
				if len(close) > 0 {
					msg += " — did you mean " + strings.Join(close, ", ") + "?"
				}
				return fmt.Errorf("%s (see `dokwalt docs`)", msg)
			}
			return renderMarkdown(body, !noPager)
		},
	}
	cmd.Flags().StringVar(&search, "search", "", "find topics containing these words")
	cmd.Flags().BoolVar(&noPager, "no-pager", false, "print without a pager")
	return cmd
}

func printTopics(ts []docs.Topic) {
	var rows [][]string
	for _, t := range ts {
		rows = append(rows, []string{ui.AccentS.Render(t.Name), t.Summary})
	}
	fmt.Println(ui.Table([]string{"TOPIC", "ABOUT"}, rows))
}

func renderMarkdown(md string, pager bool) error {
	if !ui.IsTTY() {
		fmt.Print(md)
		return nil
	}
	width := ui.Width()
	if width > 110 {
		width = 110
	}
	r, err := glamour.NewTermRenderer(glamour.WithAutoStyle(), glamour.WithWordWrap(width-4))
	if err != nil {
		return err
	}
	out, err := r.Render(md)
	if err != nil {
		return err
	}
	_, h, _ := term.GetSize(int(os.Stdout.Fd()))
	if pager && strings.Count(out, "\n") > h {
		p := os.Getenv("PAGER")
		if p == "" {
			p = "less -R"
		}
		fields := strings.Fields(p)
		c := exec.Command(fields[0], fields[1:]...)
		c.Stdin = strings.NewReader(out)
		c.Stdout, c.Stderr = os.Stdout, os.Stderr
		if err := c.Run(); err == nil {
			return nil
		}
	}
	fmt.Print(out)
	return nil
}
