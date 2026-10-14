// Package session adds durable conversations and context policy to astral's
// executor. Manager owns loaded Sessions; each Session owns bounded working
// state, approval gates, input queues and a paged HistoryReader.
//
// Store commits atomically persist entries, exact state, indexes and receipts.
// Runtime checkpoints are not model summaries. SQLite provides bounded history
// reads; Memory and JSONL provide the same execution contract with in-memory
// historical indexes. A host must establish one execution owner per Session.
// Close cancels and joins owned work but never closes host models, stores or
// tool runtimes. Format version 2 deliberately has no legacy migration layer.
package session
