package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"charm.land/huh/v2"
	"charm.land/lipgloss/v2"
	"golang.org/x/term"

	"github.com/agentwireprotocol/awp/internal/bootstrap"
)

var (
	styleOK    = lipgloss.NewStyle().Foreground(lipgloss.Color("#3DD68C")).Bold(true)
	styleErr   = lipgloss.NewStyle().Foreground(lipgloss.Color("#FF5F87")).Bold(true)
	styleDim   = lipgloss.NewStyle().Foreground(lipgloss.Color("#8A8A8A"))
	styleTitle = lipgloss.NewStyle().Bold(true)
)

func cmdBootstrap(ctx context.Context, args []string) error {
	f := newFlags("bootstrap", "", "Install awp into the agent harnesses on this machine, so their next sessions\ncan message other agents. Detects Claude Code, opencode, Codex, Cursor,\nGemini CLI, Copilot CLI, grok and pi, and asks which to set up (or use --all / --harness).\nEach gets what it supports: the skill, the MCP server, and hooks that bring\ninbound messages into the model's context. Safe to run again; --uninstall undoes it.")
	only := f.StringSlice("harness", nil, "harnesses to set up: "+strings.Join(bootstrap.IDs(), ", "))
	all := f.Bool("all", false, "set up every detected harness without asking")
	yes := f.BoolP("yes", "y", false, "do not ask (with no --harness, the same as --all)")
	uninstall := f.Bool("uninstall", false, "remove awp from the chosen harnesses")
	list := f.Bool("list", false, "show the detected harnesses and what awp has installed, then exit")
	dry := f.Bool("dry-run", false, "show what would change, change nothing")
	binFlag := f.String("bin", "", "awp binary to wire in (default: this one)")
	if err := f.Parse(args); err != nil {
		return err
	}
	bin, err := resolveBin(*binFlag)
	if err != nil {
		return err
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return err
	}
	env := bootstrap.NewEnv(home, bin)
	env.DryRun = *dry
	found := bootstrap.Detect(ctx, env)

	if *list || *f.json {
		return printDetected(found, *f.json)
	}
	if len(found) == 0 {
		fmt.Println("No agent harnesses found (looked for " + strings.Join(bootstrap.IDs(), ", ") + ").")
		return nil
	}

	var chosen []bootstrap.Found
	switch {
	case len(*only) > 0:
		for _, id := range *only {
			if bootstrap.Lookup(id) == nil {
				return fmt.Errorf("unknown harness %q (known: %s)", id, strings.Join(bootstrap.IDs(), ", "))
			}
			i := slices.IndexFunc(found, func(fd bootstrap.Found) bool { return fd.ID == id })
			if i < 0 {
				return fmt.Errorf("%s is not on this machine (no executable on PATH, no ~/%s)", id, bootstrap.Lookup(id).Dir)
			}
			chosen = append(chosen, found[i])
		}
	case *all || *yes:
		for _, fd := range found {
			if !*uninstall || fd.Status.Any() {
				chosen = append(chosen, fd)
			}
		}
	case term.IsTerminal(int(os.Stdin.Fd())) && term.IsTerminal(int(os.Stdout.Fd())):
		if chosen, err = pickHarnesses(found, *uninstall); err != nil {
			if errors.Is(err, huh.ErrUserAborted) {
				fmt.Println("Cancelled; nothing changed.")
				return nil
			}
			return err
		}
	default:
		return errors.New("no terminal to ask on: pass --all, or --harness with a list")
	}

	verb := "Installing awp into"
	if *uninstall {
		verb = "Removing awp from"
	}
	if *dry {
		verb = "Dry run: " + strings.ToLower(verb[:1]) + verb[1:]
	}
	lipgloss.Println(styleTitle.Render(fmt.Sprintf("%s %d harness(es)", verb, len(chosen))))
	failed := 0
	for _, fd := range chosen {
		var did []string
		if *uninstall {
			did, err = fd.Uninstall(ctx, env)
		} else {
			did, err = fd.Install(ctx, env)
		}
		if err != nil {
			failed++
			lipgloss.Println(styleErr.Render("✗ "+fd.Name), err.Error())
			continue
		}
		if len(did) == 0 {
			did = []string{"nothing to do"}
		}
		lipgloss.Println(styleOK.Render("✓ " + fd.Name))
		for _, d := range did {
			lipgloss.Println("   " + styleDim.Render(d))
		}
	}
	if !*uninstall && !*dry && failed < len(chosen) {
		fmt.Println()
		fmt.Println("Next: start a new session in any of them and ask it to run `awp up` (or run it")
		fmt.Println("yourself). Sessions that were already open pick up the change after a restart.")
		if !onPath(bin) {
			lipgloss.Println(styleDim.Render("note: " + filepath.Dir(bin) + " is not on your PATH. The skill tells agents the full path, but add it for your own shell."))
		}
	}
	if failed > 0 {
		return exitCode(1)
	}
	return nil
}

// pickHarnesses asks which detected harnesses to set up (or clean up).
func pickHarnesses(found []bootstrap.Found, uninstall bool) ([]bootstrap.Found, error) {
	var opts []huh.Option[string]
	for _, fd := range found {
		label := fd.Name
		if fd.Version != "" {
			label += "  " + fd.Version
		}
		label += "  ·  " + fd.Gets
		if fd.Status.Any() {
			label += "  (installed)"
		}
		if uninstall && !fd.Status.Any() {
			continue
		}
		opts = append(opts, huh.NewOption(label, fd.ID).Selected(true))
	}
	if len(opts) == 0 {
		return nil, nil
	}
	title, desc := "Install awp into which agent harnesses?", "Found on this machine. Space toggles, enter confirms, ctrl+c cancels."
	if uninstall {
		title = "Remove awp from which harnesses?"
	}
	var picked []string
	err := huh.NewForm(huh.NewGroup(
		huh.NewMultiSelect[string]().
			Title(title).
			Description(desc).
			Options(opts...).
			Value(&picked).
			Validate(func(v []string) error {
				if len(v) == 0 {
					return errors.New("pick at least one, or press ctrl+c to cancel")
				}
				return nil
			}),
	)).WithTheme(huh.ThemeFunc(huh.ThemeCharm)).Run()
	if err != nil {
		return nil, err
	}
	var chosen []bootstrap.Found
	for _, fd := range found {
		if slices.Contains(picked, fd.ID) {
			chosen = append(chosen, fd)
		}
	}
	return chosen, nil
}

func printDetected(found []bootstrap.Found, asJSON bool) error {
	if asJSON {
		type row struct {
			ID      string           `json:"id"`
			Name    string           `json:"name"`
			Exe     string           `json:"exe,omitempty"`
			Version string           `json:"version,omitempty"`
			Gets    string           `json:"gets"`
			Status  bootstrap.Status `json:"installed"`
		}
		rows := []row{}
		for _, fd := range found {
			rows = append(rows, row{fd.ID, fd.Name, fd.Exe, fd.Version, fd.Gets, fd.Status})
		}
		return printJSON(rows)
	}
	if len(found) == 0 {
		fmt.Println("No agent harnesses found.")
		return nil
	}
	cell := lipgloss.NewStyle().Width(7)
	mark := func(on, supported bool) string {
		switch {
		case !supported:
			return cell.Render(styleDim.Render("n/a"))
		case on:
			return cell.Render(styleOK.Render("✓"))
		}
		return cell.Render(styleDim.Render("·"))
	}
	lipgloss.Println(styleTitle.Render(fmt.Sprintf("%-20s %-28s %-7s%-7s%s", "HARNESS", "VERSION", "SKILL", "MCP", "HOOKS")))
	for _, fd := range found {
		version := fd.Version
		if version == "" {
			version = "-"
		}
		if len(version) > 27 {
			version = version[:26] + "…"
		}
		lipgloss.Printf("%-20s %-28s %s%s%s\n", fd.Name, version, mark(fd.Status.Skill, true), mark(fd.Status.MCP, fd.MCP), mark(fd.Status.Hooks, fd.Hooks))
	}
	return nil
}

// resolveBin returns the absolute path of the awp binary to wire into
// harness configs: --bin, or this executable with symlinks resolved.
func resolveBin(flag string) (string, error) {
	p := flag
	if p == "" {
		exe, err := os.Executable()
		if err != nil {
			return "", err
		}
		p = exe
	}
	if r, err := filepath.EvalSymlinks(p); err == nil {
		p = r
	}
	return filepath.Abs(p)
}

// onPath reports whether bin is what `awp` resolves to on PATH.
func onPath(bin string) bool {
	for _, dir := range filepath.SplitList(os.Getenv("PATH")) {
		cand := filepath.Join(dir, "awp")
		if r, err := filepath.EvalSymlinks(cand); err == nil && r == bin {
			return true
		}
	}
	return false
}
