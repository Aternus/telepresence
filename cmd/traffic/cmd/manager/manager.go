package manager

import (
	"context"
	"fmt"
	"net/http"
	"os"
	"strings"
	"time"

	"golang.org/x/net/http2"
	"golang.org/x/net/http2/h2c"
	"google.golang.org/grpc"
	"google.golang.org/grpc/health/grpc_health_v1"

	"github.com/datawire/dlib/dgroup"
	"github.com/datawire/dlib/dlog"
	"github.com/datawire/dlib/dutil"
	rpc "github.com/telepresenceio/telepresence/rpc/v2/manager"
	"github.com/telepresenceio/telepresence/v2/pkg/version"
)

func Main(ctx context.Context, args ...string) error {
	dlog.Infof(ctx, "Traffic Manager %s [pid:%d]", version.Version, os.Getpid())

	env, err := LoadEnv(ctx)
	if err != nil {
		return err
	}

	g := dgroup.NewGroup(ctx, dgroup.GroupConfig{
		EnableSignalHandling: true,
	})
	mgr := NewManager(ctx, env)

	// Serve HTTP (including gRPC)
	g.Go("httpd", func(ctx context.Context) error {
		host := env.ServerHost
		port := env.ServerPort

		grpcHandler := grpc.NewServer()
		httpHandler := http.Handler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			fmt.Fprintf(w, "Hello World from: %s\n", r.URL.Path)
		}))
		server := &http.Server{
			Addr:     host + ":" + port,
			ErrorLog: dlog.StdLogger(ctx, dlog.LogLevelError),
			Handler: h2c.NewHandler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.ProtoMajor == 2 && strings.HasPrefix(r.Header.Get("Content-Type"), "application/grpc") {
					grpcHandler.ServeHTTP(w, r)
				} else {
					httpHandler.ServeHTTP(w, r)
				}
			}), &http2.Server{}),
		}

		rpc.RegisterManagerServer(grpcHandler, mgr)
		grpc_health_v1.RegisterHealthServer(grpcHandler, &HealthChecker{})

		return dutil.ListenAndServeHTTPWithContext(ctx, server)
	})

	g.Go("intercept-gc", func(ctx context.Context) error {
		// Loop calling Expire
		ticker := time.NewTicker(5 * time.Second)
		defer ticker.Stop()

		for {
			select {
			case <-ticker.C:
				mgr.expire()
			case <-ctx.Done():
				return nil
			}
		}
	})

	// This goroutine is responsible for informing System A of intercepts (and
	// relevant metadata like domains) that have been garbage collected. This
	// ensures System A doesn't list preview URLs + intercepts that no longer
	// exist.
	g.Go("systema-gc", func(ctx context.Context) error {
		for snapshot := range mgr.state.WatchIntercepts(ctx, nil) {
			for _, update := range snapshot.Updates {
				// Since all intercepts with a domain require a login, we can use
				// presence of the ApiKey in the interceptInfo to determine all
				// intercepts that we need to inform System A of their deletion
				if update.Delete && update.Value.ApiKey != "" {
					if sa, err := mgr.systema.Get(); err != nil {
						dlog.Errorln(ctx, "systema: acquire connection:", err)
					} else {
						// First we remove the PreviewDomain if it exists
						if update.Value.PreviewDomain != "" {
							err = mgr.reapDomain(ctx, sa, update)
							if err != nil {
								dlog.Errorln(ctx, "systema: remove domain:", err)
							}
						}
						// Now we inform SystemA of the intercepts removal
						dlog.Debugf(ctx, "systema: remove intercept: %q", update.Value.Id)
						err = mgr.reapIntercept(ctx, sa, update)
						if err != nil {
							dlog.Errorln(ctx, "systema: remove intercept:", err)
						}

						// Release the connection we got to delete the domain + intercept
						if err := mgr.systema.Done(); err != nil {
							dlog.Errorln(ctx, "systema: release management connection:", err)
						}
					}
					// Release the refcount on the proxy connection
					if err := mgr.systema.Done(); err != nil {
						dlog.Errorln(ctx, "systema: release proxy connection:", err)
					}
				}
			}
		}
		return nil
	})

	// Wait for exit
	return g.Wait()
}
