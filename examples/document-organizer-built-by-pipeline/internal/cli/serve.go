package cli

import (
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/netip"
	"strconv"
	"strings"
	"time"

	"docs/internal/httpapi"
)

const (
	defaultAddr       = "127.0.0.1:8080"
	readHeaderTimeout = 5 * time.Second
	shutdownTimeout   = 30 * time.Second
)

// parseListenAddr validates that addr is a loopback host:port and returns it in canonical form.
func parseListenAddr(addr string) (string, error) {
	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		return "", usagef("invalid --addr %q: %v", addr, err)
	}
	if _, err := strconv.ParseUint(port, 10, 16); err != nil {
		return "", usagef("invalid --addr %q: port must be a number from 0 to 65535", addr)
	}
	if strings.EqualFold(host, "localhost") {
		host = "127.0.0.1"
	}
	ip, err := netip.ParseAddr(host)
	if err != nil || ip.Zone() != "" || ip.Is4In6() || !ip.IsLoopback() {
		return "", usagef("invalid --addr %q: host must be localhost, 127.0.0.1 or [::1]", addr)
	}
	return net.JoinHostPort(ip.String(), port), nil
}

func runServe(ctx context.Context, args []string, stderr io.Writer) error {
	fs, c := newFlagSet("serve")
	addr := fs.String("addr", defaultAddr, "listen address (loopback only)")
	pos, err := parse(fs, args)
	if err != nil {
		return err
	}
	if len(pos) > 0 {
		return usagef("unexpected argument %q", pos[0])
	}
	listen, err := parseListenAddr(*addr)
	if err != nil {
		return err
	}
	s, err := openStore(ctx, c)
	if err != nil {
		return err
	}
	ln, err := (&net.ListenConfig{}).Listen(ctx, "tcp", listen)
	if err != nil {
		return fmt.Errorf("listen: %w", err)
	}
	fmt.Fprintf(stderr, "docs: listening on http://%s\n", ln.Addr())
	return serve(ctx, ln, httpapi.NewHandler(s), shutdownTimeout)
}

// serve runs h on ln until ctx is cancelled, then shuts down gracefully within timeout.
// In-flight requests run on a context that cancellation does not affect, so they can finish.
func serve(ctx context.Context, ln net.Listener, h http.Handler, timeout time.Duration) error {
	srv := &http.Server{
		Handler:           h,
		ReadHeaderTimeout: readHeaderTimeout,
		BaseContext:       func(net.Listener) context.Context { return context.WithoutCancel(ctx) },
	}
	errc := make(chan error, 1)
	go func() { errc <- srv.Serve(ln) }()
	select {
	case err := <-errc:
		return fmt.Errorf("serve: %w", err)
	case <-ctx.Done():
	}
	sctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), timeout)
	defer cancel()
	err := srv.Shutdown(sctx)
	<-errc // Serve returns as soon as Shutdown closes the listener
	if err != nil {
		srv.Close() // drop connections that outlived the timeout
		return fmt.Errorf("shutdown: %w", err)
	}
	return nil
}
