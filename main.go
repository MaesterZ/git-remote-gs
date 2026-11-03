// Command git-remote-gs is a git remote helper that stores a repository in a Google Cloud Storage bucket.
//
//	git remote add origin gs://my-bucket/my-repo
//	git push -u origin main
//	git clone gs://my-bucket/my-repo
package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/signal"
	"runtime/debug"
	"strconv"
	"syscall"
	"time"

	"github.com/MaesterZ/git-remote-gs/internal/git"
	"github.com/MaesterZ/git-remote-gs/internal/protocol"
	"github.com/MaesterZ/git-remote-gs/internal/store"
)

const (
	exitFatal = 1
	exitUsage = 2

	usage = "usage: git-remote-gs <remote-name> gs://BUCKET/PREFIX\n" +
		"git runs this helper for gs:// remotes, e.g. git clone gs://my-bucket/my-repo\n"
)

// version is set at release time with -ldflags "-X main.version=..."; otherwise the module version is used.
var version = ""

var (
	errBadTTL = errors.New("invalid GIT_REMOTE_GS_LOCK_TTL")
	errNested = errors.New("git-remote-gs was started by one of its own git commands, a lazy fetch in a partial " +
		"clone: --filter isn't supported, clone without it (git 2.44+ avoids this)")
)

func main() {
	os.Exit(run(os.Args[1:], os.Stdin, os.Stdout, os.Stderr))
}

func run(args []string, stdin io.Reader, stdout, stderr io.Writer) int {
	if len(args) == 1 && (args[0] == "--version" || args[0] == "version") {
		_, _ = fmt.Fprintln(stdout, "git-remote-gs", versionString())

		return 0
	}

	if len(args) != 2 { //nolint:mnd // <remote-name> <url>
		_, _ = io.WriteString(stderr, usage)

		return exitUsage
	}

	err := serve(args[1], stdin, stdout, stderr)
	if errors.Is(err, store.ErrNoCredentials) {
		err = store.ErrNoCredentials // one line, however deep the cause; -v logs the details
	}

	if err != nil {
		_, _ = fmt.Fprintf(stderr, "fatal: %v\n", err)

		return exitFatal
	}

	return 0
}

func serve(url string, stdin io.Reader, stdout, stderr io.Writer) error {
	// Git before 2.44 ignores GIT_NO_LAZY_FETCH: stop the recursion here instead of looping forever.
	if os.Getenv(git.NestedEnv) != "" {
		return errNested
	}

	loc, err := store.ParseURL(url)
	if err != nil {
		return err //nolint:wrapcheck // already names the URL and the expected format
	}

	ttl, err := lockTTL(os.Getenv("GIT_REMOTE_GS_LOCK_TTL"))
	if err != nil {
		return err
	}

	log := protocol.NewLogger(stderr, os.Getenv("GIT_REMOTE_GS_VERBOSE") != "")
	log.Logf("remote %s, lock TTL %s", loc, ttl)

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	// After the first signal, restore the default handling so a second Ctrl-C kills a stuck lock release.
	go func() {
		<-ctx.Done()
		stop()
	}()

	gcs, err := store.NewGCS(ctx, loc.Bucket)
	if err != nil {
		log.Logf("storage client: %v", errors.Unwrap(err))

		return err //nolint:wrapcheck // the credentials message is the whole fatal line
	}

	defer func() { _ = gcs.Close() }()

	helper := protocol.New(protocol.Config{
		Store:    gcs,
		Location: loc,
		Git:      &git.Git{Logf: log.Logf},
		Log:      log,
		LockTTL:  ttl,
	})

	err = helper.Run(ctx, stdin, stdout)
	if credErr, ok := errors.AsType[*store.CredentialsError](err); ok {
		log.Logf("credentials: %v", credErr.Err)
	}

	return err //nolint:wrapcheck // protocol errors name the ref and object
}

// lockTTL parses GIT_REMOTE_GS_LOCK_TTL, in whole seconds.
func lockTTL(value string) (time.Duration, error) {
	if value == "" {
		return protocol.DefaultLockTTL, nil
	}

	seconds, err := strconv.Atoi(value)
	if err != nil || seconds < 1 {
		return 0, fmt.Errorf("%w %q: want a whole number of seconds, at least 1", errBadTTL, value)
	}

	return time.Duration(seconds) * time.Second, nil
}

func versionString() string {
	if version != "" {
		return version
	}

	if info, ok := debug.ReadBuildInfo(); ok && info.Main.Version != "" && info.Main.Version != "(devel)" {
		return info.Main.Version
	}

	return "dev"
}
