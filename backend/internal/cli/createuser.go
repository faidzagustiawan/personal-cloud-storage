// Package cli holds the administrative subcommands.
//
// User creation lives here rather than behind an HTTP endpoint because there
// is no registration endpoint at all (decision D7). Creating an account
// requires shell access to the VPS, which is the correct bar for a personal
// cloud with exactly one user.
package cli

import (
	"bufio"
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"strings"

	"golang.org/x/term"

	"cloudapp/internal/config"
	"cloudapp/internal/store"
)

func CreateUser(ctx context.Context, cfg *config.Config, st *store.Store, args []string) error {
	fs := flag.NewFlagSet("createuser", flag.ContinueOnError)
	username := fs.String("username", "", "username to create (required)")
	quotaGB := fs.Int64("quota-gb", cfg.DefaultQuota>>30, "storage quota in GB")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *username == "" {
		fs.Usage()
		return errors.New("--username is required")
	}

	password, err := readPassword()
	if err != nil {
		return err
	}

	user, err := st.CreateUser(ctx, *username, password, *quotaGB<<30)
	if errors.Is(err, store.ErrUserExists) {
		return fmt.Errorf("user %q already exists", *username)
	}
	if err != nil {
		return err
	}

	fmt.Printf("Created user %q (id %d) with a %d GB quota.\n", user.Username, user.ID, *quotaGB)

	if n, err := st.CountUsers(ctx); err == nil && n > 1 {
		fmt.Printf("\nNote: this database now holds %d users. The spec is written for a single\n"+
			"user; per-user B2 key prefixes (§2.3) assume users/1/ and need revisiting.\n", n)
	}
	return nil
}

// readPassword prompts twice without echoing. When stdin is not a terminal it
// reads a single line instead, so the command still works from a script — but
// never from a command-line flag, which would put the password in shell
// history and in the process table.
func readPassword() (string, error) {
	if !term.IsTerminal(int(os.Stdin.Fd())) {
		line, err := bufio.NewReader(os.Stdin).ReadString('\n')
		if err != nil && line == "" {
			return "", fmt.Errorf("read password from stdin: %w", err)
		}
		return strings.TrimRight(line, "\r\n"), nil
	}

	fmt.Print("Password (at least 10 characters): ")
	first, err := term.ReadPassword(int(os.Stdin.Fd()))
	fmt.Println()
	if err != nil {
		return "", err
	}

	fmt.Print("Repeat password: ")
	second, err := term.ReadPassword(int(os.Stdin.Fd()))
	fmt.Println()
	if err != nil {
		return "", err
	}

	if string(first) != string(second) {
		return "", errors.New("the two passwords do not match")
	}
	return string(first), nil
}

// ChangePassword resets a password from the shell, for the case the web login
// is the thing that is locked out.
func ChangePassword(ctx context.Context, st *store.Store, args []string) error {
	fs := flag.NewFlagSet("passwd", flag.ContinueOnError)
	username := fs.String("username", "", "username (required)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *username == "" {
		fs.Usage()
		return errors.New("--username is required")
	}

	password, err := readPassword()
	if err != nil {
		return err
	}

	// Authenticate is the wrong tool here — the point is to recover without the
	// old password — so look the user up and set it directly.
	var id int64
	if err := st.R.QueryRowContext(ctx,
		`SELECT id FROM users WHERE username = ?`, strings.ToLower(*username)).Scan(&id); err != nil {
		return fmt.Errorf("user %q not found", *username)
	}
	if err := st.SetPassword(ctx, id, password); err != nil {
		return err
	}

	n, err := st.RevokeAllSessions(ctx, id)
	if err != nil {
		return err
	}
	fmt.Printf("Password updated for %q. %d active session(s) revoked.\n", *username, n)
	return nil
}
