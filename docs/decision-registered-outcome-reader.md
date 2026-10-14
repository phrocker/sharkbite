# Registered outcome receipt reader

Operational adjudication may need reports from several authenticated principals.
The ordinary outcome store's Read method remains principal scoped. A separate
registered reader requires explicit authority to inspect published receipts in a
specified target inventory snapshot.

The reader uses the actual caller's auth decision. It does not impersonate the
original reporter. Its authority must resolve each receipt's published membership,
coverage, canonical prediction and current cross-principal read purpose before
storage access. Original receipt attribution is validated against the caller's
authorization domain and the stored receipt's immutable scope and key binding.

Correction ancestry must stay inside the same target and inventory snapshot,
with compatible original reporter lineage, request, prediction and receipt-time
ordering. All reads finish before a final joint authority check. That check must
verify the exact original receipt commitments, current inventory generation,
source/evidence permissions and the caller's read purpose. Membership alone never
grants source access or training permission.

Denied or incomplete lookups return no receipts or counts. Verification receives
detached material; callback mutation cannot change the returned original receipt.
The reader performs no writes, inventory registration, admission or adjudication.
It provides one target snapshot's authorized material; a multi-target export must
separately establish a consistent inventory vector and current training rights.
