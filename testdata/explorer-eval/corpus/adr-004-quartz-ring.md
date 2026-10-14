# ADR-004: Use Quartz Ring for relay buffering

Document ID: adr-004-quartz-ring
Revision ID: r1
Valid from: 2025-12-18T00:00:00Z
Valid to: open
Status: Accepted

## Context

The Aster Relay needs a durable handoff between the Juniper Agent and the Lumen Processor. Processor restarts must not force agents to resend sealed batches.

## Decision

The Aster Relay will place each sealed telemetry batch on the Quartz Ring. The Lumen Processor will consume batches from that ring and acknowledge them through the relay protocol.

## Consequences

The Quartz Ring remains online during Lumen Processor restarts. The Amber Lag runbook may pause Juniper Agent intake without draining the ring. Operators must monitor queue age as well as queue depth.
