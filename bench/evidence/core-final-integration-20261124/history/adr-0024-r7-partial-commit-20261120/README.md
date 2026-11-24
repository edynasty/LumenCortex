# ADR-0024 R7 workspace evidence

The [Core handoff](../../../wt/core-adr-0024-r7-20261120/docs/reports/adr-0024-r7-partial-commit.md) records the transaction contract, source comparisons, four findings and Carbon's replay commands. The [portable evidence package](../../../wt/core-adr-0024-r7-20261120/docs/reports/evidence/adr-0024-r7/) travels with the R7 branch.

This directory retains the 22 original R6 probe files and historical transcripts, including absolute overlay mappings captured on the original machine. Verify their bytes here with `(cd private-r6-originals && shasum -a 256 -c ../ORIGINALS.sha256)`. For a new checkout use the portable replay script, which builds its own temporary mappings.

`logs/r6-f02-original.log` records a test whose success condition is observing the R6 defect. `logs/sol-r6-replay.log` records the reproduced append, JSON and path failures. `logs/claude-2.1.72.log` records the separate public CLI observation.
