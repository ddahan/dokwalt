package cli

import (
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"sort"
	"strings"

	"github.com/spf13/cobra"
	"github.com/spf13/pflag"
	"golang.org/x/term"

	"github.com/ddahan/dokwalt/internal/api"
	"github.com/ddahan/dokwalt/internal/ui"
)

// Command annotations read by the dashboard's command bar.
const (
	annNoApp      = "dokwalt/no-app"     // -a is not added: the command is not about the selected app
	annAppArg     = "dokwalt/app-arg"    // the first argument is an existing app (tab completes app names)
	annFullScreen = "dokwalt/fullscreen" // the command owns the screen: no "press a key" after it
)

// candidate is one completion shown under the command bar.
type candidate struct{ value, desc string }

// splitLine splits a command line into words, honoring '…', "…" and \.
func splitLine(s string) ([]string, error) {
	var words []string
	var b strings.Builder
	in, quote, esc := false, rune(0), false
	for _, r := range s {
		switch {
		case esc:
			b.WriteRune(r)
			esc = false
		case r == '\\' && quote != '\'':
			esc, in = true, true
		case quote != 0:
			if r == quote {
				quote = 0
			} else {
				b.WriteRune(r)
			}
		case r == '\'' || r == '"':
			quote, in = r, true
		case r == ' ' || r == '\t':
			if in {
				words = append(words, b.String())
				b.Reset()
				in = false
			}
		default:
			b.WriteRune(r)
			in = true
		}
	}
	if quote != 0 {
		return nil, errors.New("unclosed quote")
	}
	if in {
		words = append(words, b.String())
	}
	return words, nil
}

// stripProgram drops a leading "dokwalt", typed out of habit.
func stripProgram(words []string) []string {
	if len(words) > 0 && words[0] == "dokwalt" {
		return words[1:]
	}
	return words
}

// resolve finds the command named by the first words; n is how many it used.
func resolve(root *cobra.Command, words []string) (c *cobra.Command, n int) {
	c = root
	for n < len(words) {
		sub := findSub(c, words[n])
		if sub == nil {
			break
		}
		c = sub
		n++
	}
	return c, n
}

func findSub(c *cobra.Command, name string) *cobra.Command {
	if strings.HasPrefix(name, "-") {
		return nil
	}
	for _, s := range c.Commands() {
		if s.Name() == name || s.HasAlias(name) {
			return s
		}
	}
	return nil
}

// lookupFlag finds a flag of c by "-a", "--app" or "--app=x".
func lookupFlag(c *cobra.Command, w string) *pflag.Flag {
	name, _, _ := strings.Cut(strings.TrimLeft(w, "-"), "=")
	for _, fs := range []*pflag.FlagSet{c.Flags(), c.InheritedFlags()} {
		if strings.HasPrefix(w, "--") {
			if f := fs.Lookup(name); f != nil {
				return f
			}
		} else if len(name) == 1 {
			if f := fs.ShorthandLookup(name); f != nil {
				return f
			}
		}
	}
	return nil
}

// takesValue reports whether word w is a flag whose value is the next word.
func takesValue(c *cobra.Command, w string) bool {
	if !strings.HasPrefix(w, "-") || strings.Contains(w, "=") {
		return false
	}
	f := lookupFlag(c, w)
	return f != nil && f.NoOptDefVal == ""
}

// positionals returns the arguments of c among words (flags and their values skipped).
func positionals(c *cobra.Command, words []string) []string {
	var out []string
	for i := 0; i < len(words); i++ {
		switch w := words[i]; {
		case w == "--":
			return append(out, words[i+1:]...)
		case strings.HasPrefix(w, "-"):
			if takesValue(c, w) {
				i++
			}
		default:
			out = append(out, w)
		}
	}
	return out
}

func filterPrefix(cands []candidate, prefix string) []candidate {
	var out []candidate
	for _, c := range cands {
		if strings.HasPrefix(c.value, prefix) {
			out = append(out, c)
		}
	}
	return out
}

func appCandidates(apps []string, prefix string) []candidate {
	var out []candidate
	for _, a := range apps {
		out = append(out, candidate{value: prefix + a})
	}
	return out
}

// currentWord splits the line into the words before the cursor and the word
// being typed (empty after a space). ok is false inside quotes.
func currentWord(line string) (before []string, cur string, ok bool) {
	start := strings.LastIndexAny(line, " \t") + 1
	cur = line[start:]
	if strings.ContainsAny(cur, `'"\`) {
		return nil, "", false
	}
	before, err := splitLine(line[:start])
	return before, cur, err == nil
}

// completions lists what can be typed at the end of line: commands, then
// apps (after -a, or as the argument of link and apps:destroy), flags, or the
// values a command declares. hint describes the command when nothing is listed.
func completions(root *cobra.Command, line string, apps []string) (cands []candidate, hint string) {
	before, cur, ok := currentWord(line)
	if !ok {
		return nil, ""
	}
	before = stripProgram(before)
	c, n := resolve(root, before)
	if c == root && n < len(before) {
		return nil, fmt.Sprintf("unknown command %q", before[0])
	}
	if n == len(before) && c.HasAvailableSubCommands() && !strings.HasPrefix(cur, "-") {
		for _, s := range c.Commands() {
			if s.IsAvailableCommand() {
				cands = append(cands, candidate{value: s.Name(), desc: s.Short})
			}
		}
		if cands = filterPrefix(cands, cur); len(cands) == 0 {
			return nil, fmt.Sprintf("no command starts with %q", cur)
		}
		return cands, ""
	}
	args := before[n:]
	prev := ""
	if len(args) > 0 {
		prev = args[len(args)-1]
	}
	hint = strings.TrimPrefix(c.UseLine(), root.Name()+" ") + "   " + c.Short
	switch {
	case prev != "" && takesValue(c, prev):
		if f := lookupFlag(c, prev); f != nil && f.Name == "app" {
			return filterPrefix(appCandidates(apps, ""), cur), ""
		}
		return nil, hint
	case strings.HasPrefix(cur, "--app="):
		return filterPrefix(appCandidates(apps, "--app="), cur), ""
	case strings.HasPrefix(cur, "-"):
		seen := map[string]bool{}
		for _, fs := range []*pflag.FlagSet{c.Flags(), c.InheritedFlags()} {
			fs.VisitAll(func(f *pflag.Flag) {
				if !f.Hidden && !seen[f.Name] {
					seen[f.Name] = true
					cands = append(cands, candidate{value: "--" + f.Name, desc: f.Usage})
				}
			})
		}
		sort.Slice(cands, func(i, j int) bool { return cands[i].value < cands[j].value })
		return filterPrefix(cands, cur), ""
	}
	pos := positionals(c, args)
	if c.Annotations[annAppArg] != "" && len(pos) == 0 {
		return filterPrefix(appCandidates(apps, ""), cur), ""
	}
	if c.ValidArgsFunction != nil {
		vals, _ := c.ValidArgsFunction(c, pos, cur)
		for _, v := range vals {
			value, desc, _ := strings.Cut(v, "\t")
			cands = append(cands, candidate{value: value, desc: desc})
		}
		if cands = filterPrefix(cands, cur); len(cands) > 0 {
			return cands, ""
		}
	}
	return nil, hint
}

// completeLine extends the word being typed as far as all candidates agree,
// and adds a space when only one is left.
func completeLine(line string, cands []candidate) string {
	if len(cands) == 0 {
		return line
	}
	_, cur, ok := currentWord(line)
	if !ok {
		return line
	}
	common := cands[0].value
	for _, c := range cands[1:] {
		for !strings.HasPrefix(c.value, common) {
			common = common[:len(common)-1]
		}
	}
	if len(common) < len(cur) {
		return line
	}
	line = line[:len(line)-len(cur)] + common
	if len(cands) == 1 {
		line += " "
	}
	return line
}

// commandArgs turns a command bar line into dokwalt's arguments, adding
// -a/-s for the selected app (unless given) and --server when the dashboard
// was started with one. Flags go right after the command name, so that
// `exec -- ls` keeps them out of the container command.
func commandArgs(root *cobra.Command, line string, sel *row, server string) ([]string, *cobra.Command, error) {
	words, err := splitLine(line)
	if err != nil {
		return nil, nil, err
	}
	words = stripProgram(words)
	if len(words) == 0 {
		return nil, nil, nil
	}
	c, n := resolve(root, words)
	if c == root {
		return nil, nil, fmt.Errorf("unknown command %q — press tab to list them", words[0])
	}
	given := func(names ...string) bool {
		for _, w := range words[n:] {
			if w == "--" {
				return false
			}
			for _, name := range names {
				if w == name || strings.HasPrefix(w, name+"=") || (len(name) == 2 && strings.HasPrefix(w, name) && !strings.HasPrefix(w, "--")) {
					return true
				}
			}
		}
		return false
	}
	var add []string
	if sel != nil && c.Annotations[annNoApp] == "" && topLevel(c).GroupID != "server" && !given("-a", "--app") {
		add = append(add, "-a", sel.app)
		if sel.stage != api.DefaultStage && !given("-s", "--stage") {
			add = append(add, "-s", sel.stage)
		}
	}
	if server != "" && !given("--server") {
		add = append(add, "--server", server)
	}
	args := append(append(append([]string{}, words[:n]...), add...), words[n:]...)
	return args, c, nil
}

// topLevel is the root's child that c belongs to.
func topLevel(c *cobra.Command) *cobra.Command {
	for c.HasParent() && c.Parent().HasParent() {
		c = c.Parent()
	}
	return c
}

// quoteArgs prints args the way a shell would need them.
func quoteArgs(args []string) string {
	out := make([]string, len(args))
	for i, a := range args {
		if a == "" || strings.ContainsAny(a, " \t'\"\\$`") {
			a = "'" + strings.ReplaceAll(a, "'", `'\''`) + "'"
		}
		out[i] = a
	}
	return strings.Join(out, " ")
}

// barRun runs a dokwalt command on the real terminal while the dashboard is
// suspended, then waits for a key so its output can be read.
type barRun struct {
	cmd   *exec.Cmd
	title string
	pause bool
}

// The command uses the real terminal, not bubbletea's wrappers, so that it
// sees a TTY.
func (r *barRun) SetStdin(io.Reader)  {}
func (r *barRun) SetStdout(io.Writer) {}
func (r *barRun) SetStderr(io.Writer) {}

func (r *barRun) Run() error {
	r.cmd.Stdin, r.cmd.Stdout, r.cmd.Stderr = os.Stdin, os.Stdout, os.Stderr
	fmt.Println("\n" + ui.AccentS.Render("$ ") + r.title)
	err := r.cmd.Run()
	if r.pause {
		fmt.Print("\n" + ui.FaintS.Render("press any key to return to the dashboard"))
		waitKey()
		fmt.Println()
	}
	return err
}

func waitKey() {
	fd := int(os.Stdin.Fd())
	if !term.IsTerminal(fd) {
		return
	}
	if st, err := term.MakeRaw(fd); err == nil {
		defer func() { _ = term.Restore(fd, st) }()
	}
	var b [16]byte
	_, _ = os.Stdin.Read(b[:])
}
