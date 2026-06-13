// Package plugin is the out-of-process connector transport (ADR-010): it lets a
// connector run EITHER in-process (registered directly with the hub) OR as a
// standalone gRPC server the hub dials, with identical behavior. The same
// [sdk.Connector] implementation works both ways unchanged.
//
// # Two adapters
//
// The package bridges [sdk.Connector] and the generated
// pluginv1.ConnectorPluginService in both directions:
//
//   - [NewServer] (and [ListenAndServe]) wrap an [sdk.Connector] as a
//     ConnectorPluginService, so a plugin author exposes their connector over
//     gRPC.
//   - [Dial] (and [NewClient]) present a remote ConnectorPluginService back as
//     an [sdk.Connector], so the hub drives a plugin exactly like a built-in
//     connector.
//
// Because the sync RPCs are server-streaming, Emit and Checkpoint — which are
// hub-side callbacks — are carried as stream events: the plugin streams a
// Document per Emit, a checkpoint cursor per Checkpoint, and a terminal Done
// carrying the final cursor. If a hub-side callback fails, the hub cancels the
// stream; the plugin observes the cancellation as a Send error and aborts the
// pass, preserving the in-process rule that an Emit/Checkpoint error is terminal
// for the current sync.
//
// # Shipping a third-party plugin
//
// A connector author implements [sdk.Connector] exactly as for an in-process
// connector, then ships a binary whose main serves it:
//
//	func main() {
//		ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
//		defer stop()
//
//		conn := mynotes.New() // implements sdk.Connector
//		addr := os.Getenv("ASKER_PLUGIN_ADDR") // e.g. "127.0.0.1:8090" or "unix:///run/asker/notes.sock"
//		if err := plugin.ListenAndServe(ctx, addr, conn); err != nil {
//			log.Fatalf("plugin: %v", err)
//		}
//	}
//
// ListenAndServe registers the connector on a fresh grpc.Server, serves on addr
// (TCP, or a unix socket when addr is "unix://…"), and GracefulStops when ctx is
// canceled — so SIGTERM drains in-flight syncs cleanly. The author chooses the
// transport credentials by configuring the server before serving when they need
// more than the package default; on the compose/cluster-internal network the
// hub and plugin trust the boundary (mTLS hardens it in M4, ADR-009).
//
// The hub side dials the plugin and adapts it:
//
//	cli, err := plugin.Dial(ctx, addr, grpc.WithTransportCredentials(insecure.NewCredentials()))
//	if err != nil { return err }
//	defer cli.Close()
//	// cli is an sdk.Connector; register it like any built-in connector.
//
// Dial caches the plugin's Spec at connect time (Spec must be cheap and stable),
// so cli.Spec() is a pure accessor with no live call.
//
// # Trust model (ADR-010)
//
// A plugin is mutually distrusted code. The hub never hands it the token vault,
// another tenant's data, or ambient credentials: each RPC carries only that
// instance's already-decrypted per-call token in Config.token and a tenant_id
// the SERVER re-validates with [tenancy.FromHeaderValue], failing closed on a
// malformed value. A plugin therefore cannot widen its blast radius beyond the
// single tenant+instance the hub is currently syncing, and a forged tenant_id
// never yields a usable tenancy.Context. Connectors must not log or persist the
// token (the [sdk] contract), and error messages crossing the transport are
// credential-free.
package plugin
