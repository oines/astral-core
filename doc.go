// Package astral is the execution core of an embeddable agent SDK.
//
// A Model and optional Tools are sufficient to run an Agent. One loop handles
// generation, retries, tool dispatch, cancellation, input boundaries and events.
// The default driver retains process-local conversation state. Package session
// composes this same executor with durable commits, recovery and context policy.
//
// Runtime connections, stores, model credentials, workspace provisioning,
// permissions and user interfaces belong to the host. Closing an Agent cancels
// and joins its work without closing host dependencies. Live events are
// nonblocking observations, not execution acknowledgements.
package astral
