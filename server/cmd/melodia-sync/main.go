// Command melodia-sync is the self-hosted companion server for Melodia:
// account login and playlist sync across phones.
//
//	melodia-sync serve
//	melodia-sync user add|passwd|del <username>
//	melodia-sync user list
package main

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"golang.org/x/term"

	"github.com/sangregoriopaolo/melodiaapp/server/internal/api"
	"github.com/sangregoriopaolo/melodiaapp/server/internal/store"
)

func env(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

func main() {
	if err := run(os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}

const usage = `usage:
  melodia-sync serve
  melodia-sync user add <username>     (password read from stdin or prompted)
  melodia-sync user passwd <username>  (also logs out every device)
  melodia-sync user del <username>
  melodia-sync user list

environment:
  MELODIA_DB           database file (default ./melodia.db)
  MELODIA_ADDR         listen address (default :8080)
  MELODIA_TRUST_PROXY  1 when behind a reverse proxy (rate limit by X-Forwarded-For)`

func run(args []string) error {
	if len(args) == 0 {
		return errors.New(usage)
	}
	st, err := store.Open(env("MELODIA_DB", "melodia.db"))
	if err != nil {
		return err
	}
	defer st.Close()

	ctx := context.Background()
	switch {
	case args[0] == "serve":
		return serve(st)
	case args[0] == "user" && len(args) == 2 && args[1] == "list":
		users, err := st.ListUsers(ctx)
		if err != nil {
			return err
		}
		for _, u := range users {
			fmt.Printf("%s\t(created %s)\n", u.Username, u.CreatedAt.Format(time.DateOnly))
		}
		return nil
	case args[0] == "user" && len(args) == 3:
		name := args[2]
		switch args[1] {
		case "add":
			pw, err := readPassword()
			if err != nil {
				return err
			}
			if err := st.CreateUser(ctx, name, pw); err != nil {
				return err
			}
			fmt.Println("user created:", name)
			return nil
		case "passwd":
			pw, err := readPassword()
			if err != nil {
				return err
			}
			if err := st.SetPassword(ctx, name, pw); err != nil {
				return err
			}
			fmt.Println("password changed, all devices logged out:", name)
			return nil
		case "del":
			if err := st.DeleteUser(ctx, name); err != nil {
				return err
			}
			fmt.Println("user deleted:", name)
			return nil
		}
	}
	return errors.New(usage)
}

func readPassword() (string, error) {
	if term.IsTerminal(int(os.Stdin.Fd())) {
		fmt.Fprint(os.Stderr, "Password: ")
		a, err := term.ReadPassword(int(os.Stdin.Fd()))
		fmt.Fprintln(os.Stderr)
		if err != nil {
			return "", err
		}
		fmt.Fprint(os.Stderr, "Repeat password: ")
		b, err := term.ReadPassword(int(os.Stdin.Fd()))
		fmt.Fprintln(os.Stderr)
		if err != nil {
			return "", err
		}
		if string(a) != string(b) {
			return "", errors.New("passwords do not match")
		}
		return string(a), nil
	}
	line, err := bufio.NewReader(os.Stdin).ReadString('\n')
	if err != nil && line == "" {
		return "", errors.New("no password on stdin")
	}
	return strings.TrimRight(line, "\r\n"), nil
}

func serve(st *store.Store) error {
	log := slog.New(slog.NewTextHandler(os.Stdout, nil))
	srv := &http.Server{
		Addr:              env("MELODIA_ADDR", ":8080"),
		Handler:           api.New(st, env("MELODIA_TRUST_PROXY", "") == "1", log),
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       30 * time.Second,
		WriteTimeout:      30 * time.Second,
		IdleTimeout:       120 * time.Second,
		MaxHeaderBytes:    16 << 10,
	}
	errc := make(chan error, 1)
	go func() {
		log.Info("melodia-sync listening", "addr", srv.Addr)
		errc <- srv.ListenAndServe()
	}()
	sig := make(chan os.Signal, 1)
	signal.Notify(sig, syscall.SIGINT, syscall.SIGTERM)
	select {
	case err := <-errc:
		return err
	case <-sig:
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		return srv.Shutdown(ctx)
	}
}
