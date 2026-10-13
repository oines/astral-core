// Package astral is an embeddable, durable agent SDK.
//
// Hosts supply models, storage, tools and optional execution environments. The
// SDK owns one execution loop, an append-only session history, context projection,
// tool dispatch, input queues and compaction. Runtime connections, workspace
// provisioning, permissions and user interfaces belong to the host.
//
// An Agent coordinates sessions in one process. The host must give that process
// exclusive execution ownership of its sessions; Store compare-and-append guards
// history writes, not external side effects. Live events are best-effort. Rollout
// entries are the durable source of truth.
package astral
