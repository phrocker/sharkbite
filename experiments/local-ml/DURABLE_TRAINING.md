# Durable inventory training showcase

The source and review showcase uses a finite operator-registered fixture census.
Every correctness report enters through authenticated outcome admission and a
real engine-backed target inventory. An interrupted publication stays pending
and blocks export until an exact retry repairs it. Ordinary receipt reads remain
principal scoped; adjudication and export use the separately authorized registered
reader to inspect published reports across reporters.

Historical label bases retain their original inventory capture. A later report
does not rewrite that history. Current export compares the published receipt set
with the label's retained basis and quarantines a stale label. The pending and
published states deliberately differ: unresolved admission blocks the export;
a completed new report leaves an observable example with an unknown label until
fresh adjudication resolves it.

Each export owns its initial inventory capture. A second matching capture after
source, receipt, basis and history reads proves that the loaded inventory vector
remained consistent across that work. Both captures use two complete scans and
retain measured windows. This establishes an inventory cut under the documented
linearizable/no-rollback assumptions; source permissions and training grants are
checked separately. A snapshot does not claim freshness forever.

`prepare` writes `cohort-capture.json` alongside the dataset and pinned training
manifest. The capture proof retains the initial/final vector IDs, target pins and
measurement windows. Historical basis capture files are retained separately.
The runner checks those pins against every manifest row and includes their hash
and windows in its summary, alongside repeat-training and inquiry-restart results.

Run the commands in [ADJUDICATED_TRAINING.md](ADJUDICATED_TRAINING.md) with the current
binary and runner. Use a new output directory; prior retained evidence is preserved.
The compiled fixtures remain synthetic conformance examples, with local logical
roles rather than independently recruited human adjudicators. The source oracle
and review-reference oracle do not establish general threat-analysis accuracy.

This exercises durable admission, publication, historical provenance and local
training. It does not supply a deployed production registry, GitHub-wide coverage,
automatic tuning or promotion, or evidence of population-level LLM savings.
