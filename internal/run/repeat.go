package run

import "context"

// repeatedContextKey is the context.Value key WithRepeated and Repeated
// share, unexported so nothing outside this package can collide with it.
type repeatedContextKey struct{}

// WithRepeated carries repeated — typically a Lifecycle's own Repeated()
// channel — on ctx, so a Runner's bounded wait for an interactive child's
// process group (osrun's drainGroup) and an interactive child's kill-after
// grace can select on a second SIGINT or SIGTERM (never SIGHUP, which a
// hangup often delivers twice) without either package importing the other: internal/lifecycle
// stays the only thing in the process that calls signal.Notify, and
// internal/run/osrun never has to import it to hear about a repeat signal
// (the two cannot import each other — see internal/lifecycle/hold.go's own
// Quiescer for the same constraint the other way round). Mirrors
// internal/lock's WithHeld/Inherited: the owner publishes what it has, ctx
// carries it to whoever needs it.
func WithRepeated(ctx context.Context, repeated <-chan struct{}) context.Context {
	return context.WithValue(ctx, repeatedContextKey{}, repeated)
}

// Repeated is the channel WithRepeated carried on ctx, or nil if none was:
// selecting on a nil channel simply never fires, so a wait that was never
// given one just runs out its own bound instead of ending early.
func Repeated(ctx context.Context) <-chan struct{} {
	repeated, _ := ctx.Value(repeatedContextKey{}).(<-chan struct{})
	return repeated
}
