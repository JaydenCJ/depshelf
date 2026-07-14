// The serve subcommand: bind a listener (loopback by default), wire the
// store + mirror + HTTP frontends, and run until interrupted.
package cli

import (
	"context"
	"errors"
	"flag"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/JaydenCJ/depshelf/internal/allowlist"
	"github.com/JaydenCJ/depshelf/internal/mirror"
	"github.com/JaydenCJ/depshelf/internal/server"
	"github.com/JaydenCJ/depshelf/internal/store"
	"github.com/JaydenCJ/depshelf/internal/version"
)

func runServe(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("serve", flag.ContinueOnError)
	fs.SetOutput(stderr)
	storeDir := fs.String("store", "./depshelf-store", "store directory (plain files)")
	listen := fs.String("listen", "127.0.0.1:8417", "listen address; loopback by default, never exposed unless you say so")
	offline := fs.Bool("offline", false, "serve only from the store; never touch the network")
	allowPath := fs.String("allowlist", "", "allowlist file; absent = allow everything, present = deny-by-default")
	npmUp := fs.String("npm-upstream", mirror.DefaultNpmUpstream, "npm registry to read through")
	pypiUp := fs.String("pypi-upstream", mirror.DefaultPypiUpstream, "PyPI simple index to read through")
	ttl := fs.Duration("metadata-ttl", 15*time.Minute, "how long cached metadata counts as fresh")
	publicURL := fs.String("public-url", "", "base URL written into rewritten links (default: per-request Host)")
	if err := fs.Parse(args); err != nil {
		return exitUsage
	}
	if fs.NArg() != 0 {
		fs.Usage()
		return exitUsage
	}

	st, err := store.Open(*storeDir)
	if err != nil {
		return runtimeErr(stderr, err)
	}
	var allow *allowlist.List
	if *allowPath != "" {
		if allow, err = allowlist.Load(*allowPath); err != nil {
			return runtimeErr(stderr, err)
		}
	}
	logger := log.New(stderr, "", log.LstdFlags)
	m := mirror.New(mirror.Options{
		Store:        st,
		NpmUpstream:  *npmUp,
		PypiUpstream: *pypiUp,
		Offline:      *offline,
		MetadataTTL:  *ttl,
		Logf:         logger.Printf,
	})
	h := server.New(server.Config{
		Mirror:    m,
		Store:     st,
		Allow:     allow,
		PublicURL: *publicURL,
		Logf:      logger.Printf,
	})

	ln, err := net.Listen("tcp", *listen)
	if err != nil {
		return runtimeErr(stderr, err)
	}
	mode := "read-through"
	if *offline {
		mode = "offline"
	}
	logger.Printf("depshelf %s listening on http://%s (store %s, %s mode)",
		version.Version, ln.Addr(), *storeDir, mode)
	logger.Printf("npm registry: http://%s/npm/ — pip index: http://%s/pypi/simple/", ln.Addr(), ln.Addr())

	srv := &http.Server{Handler: h, ReadHeaderTimeout: 10 * time.Second}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	errc := make(chan error, 1)
	go func() { errc <- srv.Serve(ln) }()
	select {
	case <-ctx.Done():
		logger.Printf("shutting down")
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		srv.Shutdown(shutdownCtx)
		return exitOK
	case err := <-errc:
		if errors.Is(err, http.ErrServerClosed) {
			return exitOK
		}
		return runtimeErr(stderr, err)
	}
}
