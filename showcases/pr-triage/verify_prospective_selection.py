#!/usr/bin/env python3
# Licensed under the Apache License, Version 2.0. See LICENSE and NOTICE.
"""Rebuild deterministic sampling from retained inventory and full source manifest."""
import json
from pathlib import Path
from candidates import checked
from shadow import digest
import prospective_snapshot

# Actual V10 builder from ff3f679, verified against its Git source bytes.
# Accept its historical fingerprint only; recompute all semantics with current checks.
V10_BUILDER='65f306f43436c9939ea65f8b6ac872bf8bb14b8e0d76a7aa65941ee13816e71e'


def verify_inventory(root,inventory,protocol):
    raw_bytes=(root/'raw-inventory.json').read_bytes()
    if digest(raw_bytes)!=inventory['raw_inventory_sha256']:raise ValueError('raw inventory hash mismatch')
    raw=json.loads(raw_bytes);records={r['number']:r for r in raw}
    if len(records)!=len(raw):raise ValueError('duplicate raw PR inventory')
    candidates=inventory['candidates'];numbers=[c['pr'] for c in candidates]
    if numbers!=sorted(records):raise ValueError('summarized inventory differs from raw membership/order')
    eligible=[]
    for candidate in candidates:
        pr=candidate['pr'];r=records[pr]
        if r.get('state')!='open' or type(r.get('draft')) is not bool:
            raise ValueError('raw inventory must contain open PRs with explicit draft status')
        if candidate['head']!=r['head']['sha'] or candidate['created_at']!=r['created_at']:
            raise ValueError('candidate metadata differs from raw inventory')
        if r['draft']:expected='excluded_draft'
        elif pr in protocol['excluded_families']:expected='excluded_seen_family'
        else:
            files=json.loads((root/f'raw-files-{pr}.json').read_text())
            if not any(f['filename'].endswith('.go') for f in files):expected='excluded_no_go'
            elif len(eligible)==4:expected='excluded_cohort_limit'
            else:expected='selected';eligible.append(pr)
        if candidate['disposition']!=expected:raise ValueError('candidate eligibility disagrees with frozen protocol')
    if [s['pr'] for s in inventory['selected']]!=eligible:raise ValueError('selected families violate inventory order/eligibility')
    # Bind the selected revision pair to the recorded GitHub snapshot, not only
    # to the collected manifest. project() compares each selected pin's head and
    # base against the locally collected case, so those two agreeing proves only
    # that they agree with each other: a base substituted consistently in both
    # passes while disagreeing with what GitHub actually reported. The raw
    # inventory is the sole external anchor here, and it is already hash-bound
    # above, so the pin has to be checked against it directly.
    for s in inventory['selected']:
        r=records[s['pr']]
        if s['head']!=r['head']['sha'] or s['base']!=r['base']['sha']:
            raise ValueError('selected revision pair differs from raw inventory')


def compare_projection(full,protocol,inventory,picture,sample):
    current=digest(Path(prospective_snapshot.__file__).read_bytes())
    if picture['builder_sha256'] not in (current,V10_BUILDER):raise ValueError('unrecognized historical snapshot builder')
    expected_pic,expected_sample=prospective_snapshot.project(full,protocol,inventory)
    # Preserve a verified historical builder receipt without executing historical code.
    expected_pic.pop('id');expected_pic['builder_sha256']=picture['builder_sha256'];expected_pic['id']=digest(expected_pic)
    expected_sample.pop('id');expected_sample['picture_id']=expected_pic['id'];expected_sample['id']=digest(expected_sample)
    if picture!=expected_pic or sample!=expected_sample:
        raise ValueError('picture or sample differs from full-manifest reconstruction')


def verify_selection(root,protocol,inventory,picture,sample):
    verify_inventory(root,inventory,protocol)
    full=checked(root/'collected/manifest.json')
    for case in full['cases']:
        files=json.loads((root/f"raw-files-{case['pr']}.json").read_text())
        if {f['filename'] for f in files}!={f['path'] for f in case['files']}:
            raise ValueError('collected file inventory differs from GitHub inventory')
    compare_projection(full,protocol,inventory,picture,sample)
