# Aster Relay Protocol

Document ID: aster-relay-protocol
Revision ID: r1
Valid from: 2026-01-10T00:00:00Z
Valid to: 2026-03-15T00:00:00Z

## Purpose

The Aster Relay is a component of the Aster Mesh. It carries sealed telemetry batches from the Juniper Agent to the Lumen Processor. Each batch is assigned to the Quartz Ring before delivery. The wire marker `μ7` identifies an Aster batch.

## Acknowledgement window

The Lumen Processor must acknowledge a batch within 40 seconds. If no acknowledgement arrives, the Aster Relay retries the same batch at most four times.

## Identity and ownership

The service identifier is `svc-aster-relay`. The queue identifier is `queue-quartz-ring`. The Finch Team owns the Aster Relay.
