#!/usr/bin/env python3
# Licensed under the Apache License, Version 2.0. See LICENSE and NOTICE.
"""Measured source-ontology projection; does not resolve calls or enforce access."""
import argparse
from datetime import datetime, timezone
from pathlib import Path
from candidates import checked
from shadow import digest, write_new


def validate_observation_order(manifest, protocol, inventory):
    registered = datetime.fromisoformat(protocol['registered_at_utc'])
    observed = datetime.fromtimestamp(manifest['observed_at_unix'], timezone.utc)
    if registered.tzinfo is None or observed <= registered:
        raise ValueError('snapshot must be acquired after protocol registration')
    inventory_observed = datetime.fromisoformat(inventory['observed_at_utc'])
    if inventory_observed.tzinfo is None or not registered < inventory_observed <= observed:
        raise ValueError('inventory must follow registration and precede snapshot acquisition')
    return registered, observed


def project(manifest, protocol, inventory):
    registered, observed = validate_observation_order(manifest, protocol, inventory)
    if inventory['registered_protocol_id'] != protocol['id']:
        raise ValueError('inventory protocol mismatch')
    families = [c['pr'] for c in manifest['cases']]
    if len(families) != len(set(families)):
        raise ValueError('duplicate PR cases')
    selected = {x['pr']: x for x in inventory['selected']}
    if len(selected) != len(inventory['selected']) or list(selected) != families:
        raise ValueError('snapshot membership mismatch')
    candidates = {c['pr']:c for c in inventory['candidates']}
    if len(candidates)!=len(inventory['candidates']):raise ValueError('duplicate inventory candidates')
    if [c['pr'] for c in inventory['candidates']]!=sorted(candidates):
        raise ValueError('inventory candidates out of protocol order')
    eligible=[c['pr'] for c in inventory['candidates'] if c['disposition']=='selected']
    if list(selected)!=eligible or len(selected)>4:
        raise ValueError('selection differs from inventory dispositions or cohort bound')
    for case in manifest['cases']:
        pin=selected[case['pr']];candidate=candidates[case['pr']]
        created=datetime.fromisoformat(candidate['created_at'])
        if created.tzinfo is None:raise ValueError('inventory creation time requires timezone')
        stratum='created_after_registration' if created>registered else 'existing_at_registration'
        if candidate['head']!=pin['head'] or pin['stratum']!=stratum:
            raise ValueError('selection head or temporal stratum differs from inventory')
        if not any(f['path'].endswith('.go') for f in case['files']):
            raise ValueError('selected family has no changed Go files')
    nodes, edges, sample, counts = [], [], [], []
    for case in manifest['cases']:
        pin = selected[case['pr']]
        if case['pr'] in protocol['excluded_families'] or any(case[k] != pin[k] for k in ('head', 'base')):
            raise ValueError('seen family or changed revision')
        revision = digest({'base': case['base'], 'head': case['head']})
        nodes.append({'id': revision, 'type': 'revision_pair', 'base': case['base'], 'head': case['head']})
        for f in case['files']:
            fid = digest([revision, f['path']])
            nodes.append({'id': fid, 'type': 'changed_file', 'path': f['path'], 'disposition': f['disposition']})
            edges.append({'subject': fid, 'predicate': 'observed_in', 'object': revision, 'evidence': case['patch_sha256']})
        for u in case['units']:
            fid = digest([revision, u['path']])
            nodes.append({'id': u['id'], 'type': 'changed_declaration' if u['kind']=='function' else 'other_changed_unit',
                          'kind': u['kind'], 'symbol': u['symbol'], 'source_digest': digest({'before':u['before'],'after':u['after']})})
            edges.append({'subject': u['id'], 'predicate': 'declared_in', 'object': fid, 'evidence': manifest['id']})
        funcs = [u for u in case['units'] if u['kind'] == 'function']
        chosen = sorted(funcs, key=lambda u: digest(['shoal-v10-20261002', u['id']]))[:24]
        sample.extend(u['id'] for u in chosen)
        counts.append({'family':case['pr'],'stratum':pin['stratum'],'files_observed':len(case['files']),
                       'files_enumerated':len(case['files']),'parsed_go_files':sum(f['disposition']=='parsed_go' for f in case['files']),
                       'changed_units':len(case['units']),'functions':len(funcs),'sampled_functions':len(chosen),
                       'sample_inclusion_probability':len(chosen)/len(funcs) if funcs else None,
                       'dependency_coverage_denominator':None})
    picture = {'schema':1,'ontology':{'name':'observed-source-change','version':1,
        'node_types':['revision_pair','changed_file','changed_declaration','other_changed_unit'],
        'predicates':['observed_in','declared_in']},'protocol_id':protocol['id'],'manifest_id':manifest['id'],
        'inventory_id':inventory['id'],'observed_at_utc':observed.isoformat(),'registered_at_utc':registered.isoformat(),
        'builder_sha256':digest(Path(__file__).read_bytes()),'nodes':nodes,'relationships':edges,'coverage':counts,
        'sample_ids':sample,'visibility':'public GitHub source; no Shoal authorization receipt claimed',
        'gaps':['No resolved callgraph','No policy-consumer or identity-propagation edges','No authenticated Shoal graph export'],
        'optimization_enabled':False}
    picture['id']=digest(picture)
    subset={k:v for k,v in manifest.items() if k not in ('id','cases')}
    subset.update(parent_manifest_id=manifest['id'],picture_id=picture['id'],cases=[])
    for c in manifest['cases']:
        row={k:v for k,v in c.items() if k not in ('id','units')};row['units']=[u for u in c['units'] if u['id'] in sample]
        row['id']=digest(row);subset['cases'].append(row)
    subset['id']=digest(subset)
    return picture,subset


def main():
    p=argparse.ArgumentParser(description=__doc__)
    for k in ('manifest','protocol','inventory','output'):p.add_argument('--'+k,type=Path,required=True)
    a=p.parse_args();picture,sample=project(checked(a.manifest),checked(a.protocol),checked(a.inventory))
    a.output.mkdir();write_new(a.output/'picture.json',picture);write_new(a.output/'sampled-manifest.json',sample)


if __name__=='__main__':main()
