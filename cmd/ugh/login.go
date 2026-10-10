package main

// `ugh login codex`: the independent Codex OAuth login (§3.1) — browser PKCE
// or device flow. --bg detaches the flow into a background process that
// survives this shell and its children, so a human can authorize later while
// the caller continues; --status reports the persisted flow state and the
// exact user action needed.

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"github.com/unix/ugh/internal/auth"
)

func loginCmd(args []string) error {
	if len(args) > 0 && args[0] != "codex" {
		return loginKey(args)
	}
	if len(args) == 0 || args[0] != "codex" {
		return errors.New("usage: ugh login codex [--device] [--bg] [--status] | ugh login opencode | ugh login <provider>")
	}
	fs := flag.NewFlagSet("login codex", flag.ContinueOnError)
	device := fs.Bool("device", false, "device-code flow instead of the browser callback")
	bg := fs.Bool("bg", false, "detach into the background and print the authorization URL")
	status := fs.Bool("status", false, "print the login status and exit")
	if err := fs.Parse(args[1:]); err != nil {
		return err
	}
	if fs.NArg() > 0 {
		return fmt.Errorf("login codex: unexpected argument %q", fs.Arg(0))
	}
	switch {
	case *status:
		st, err := auth.ReadLoginStatus()
		if err != nil {
			return errors.New("login codex: no login status yet (start one with `ugh login codex --bg`)")
		}
		printLoginStatus(st)
		return nil
	case *bg:
		return loginBackground(*device)
	default:
		return loginRun(*device)
	}
}

// loginRun executes the flow in this process (foreground: blocks until the
// owner authorizes or the flow times out).
func loginRun(device bool) error {
	st, err := openStore()
	if err != nil {
		return err
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	open := func(url, code string) {
		if code != "" {
			fmt.Printf("authorize at: %s\nuser code: %s\n", url, code)
		} else {
			fmt.Printf("authorize at: %s\n", url)
		}
		// Opening the owner's browser is pre-authorized; best effort only.
		if os.Getenv("DISPLAY") != "" || os.Getenv("WAYLAND_DISPLAY") != "" {
			_ = exec.Command("xdg-open", url).Start()
		}
	}
	if device {
		return auth.LoginDevice(ctx, st, func(url, code string) error { open(url, code); return nil })
	}
	return auth.LoginBrowser(ctx, st, func(url string) error { open(url, ""); return nil })
}

// loginBackground spawns loginRun detached (new session, survives this
// process), then reports the authorization URL as soon as the flow has one.
func loginBackground(device bool) error {
	_ = auth.WriteLoginStatus(auth.LoginStatus{State: "starting", Detail: "spawning background login"})
	exe, err := os.Executable()
	if err != nil {
		return err
	}
	args := []string{"login", "codex"}
	if device {
		args = append(args, "--device")
	}
	logPath := filepath.Join(auth.RuntimeDir(), "codex-login.log")
	logf, err := os.OpenFile(logPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0600)
	if err != nil {
		return err
	}
	cmd := exec.Command(exe, args...)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	cmd.Stdin, cmd.Stdout, cmd.Stderr = nil, logf, logf
	if err := cmd.Start(); err != nil {
		logf.Close()
		return err
	}
	pid := cmd.Process.Pid
	_ = cmd.Process.Release()
	_ = logf.Close()
	for i := 0; i < 50; i++ {
		st, err := auth.ReadLoginStatus()
		if err == nil && st.State != "" && st.State != "starting" {
			printLoginStatus(st)
			fmt.Printf("background login pid %d survives this process; check `ugh login codex --status`\n", pid)
			return nil
		}
		time.Sleep(100 * time.Millisecond)
	}
	return fmt.Errorf("login codex: background flow did not start; see %s", logPath)
}

func printLoginStatus(st auth.LoginStatus) {
	fmt.Printf("codex login: %s (%s)\n", st.State, st.Mode)
	if st.URL != "" {
		fmt.Println("authorize at:", st.URL)
	}
	if st.UserCode != "" {
		fmt.Println("user code:", st.UserCode)
	}
	if st.Detail != "" {
		fmt.Println(st.Detail)
	}
	if st.At > 0 {
		fmt.Println("updated:", time.UnixMilli(st.At).Format(time.RFC3339))
	}
}
