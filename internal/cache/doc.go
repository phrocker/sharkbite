// Package cache holds the per-replica caches: tablet-location/file-list
// (exception-invalidated, sharkbite-style) and the RFile block cache (LRU,
// per-replica because shared caches defeat hedged reads).
package cache
