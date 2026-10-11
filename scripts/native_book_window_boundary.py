"""Exact DBN metadata boundary for a separately acquired closed BOOK window.

This validates metadata and daily identities only. The caller must separately
prove the original accepted batch acknowledgement, reservation, complete file
set, native EOF/count/hash, receive-time bounds and source stability. This
module cannot turn a constructed header or derived file into provider evidence.
The existing full-UTC-day audit and all its source-bound callers stay unchanged.
"""
from datetime import datetime, timezone
import hashlib

from batch_acquisition_proof import canonical_query, utc_ns

DAY_NS = 86_400_000_000_000


def window_binding(query, metadata, cut, max_age, adapter):
    if (type(cut) is not int or not 0 < cut < (1 << 63) or
            type(max_age) is not int or not 0 < max_age <= 30_000_000_000):
        raise ValueError('window clocks')
    canonical = canonical_query(query)
    if (query['dataset'], query['schema'], query['stype_in']) != ('OPRA.PILLAR', 'cmbp-1', 'raw_symbol'):
        raise ValueError('option BOOK window required')
    start, end = utc_ns(query['start']), utc_ns(query['end'])
    utc_start = cut // DAY_NS * DAY_NS
    if start != cut - max_age or end != cut + 1 or start < utc_start or end > utc_start + DAY_NS:
        raise ValueError('exact closed age window required')
    if (type(metadata.start) is not int or type(metadata.end) is not int or
            metadata.dataset != query['dataset'] or str(metadata.schema) != query['schema'] or
            str(metadata.stype_in) != 'raw_symbol' or str(metadata.stype_out) != 'instrument_id' or
            metadata.start != start or metadata.end != end):
        raise ValueError('native query range or scope differs')
    if (type(metadata.version) is not int or metadata.version not in (1, 2, 3) or
            type(metadata.ts_out) is not bool or
            (metadata.limit is not None and (type(metadata.limit) is not int or metadata.limit != 0))):
        raise ValueError('native codec or truncation')
    if (type(metadata.partial) is not list or type(metadata.not_found) is not list or
            metadata.partial or metadata.not_found or
            not adapter.metadata_symbols_match(list(metadata.symbols), query['symbols'])):
        raise ValueError('incomplete requested identity set')
    day = datetime.fromtimestamp(utc_start // 1_000_000_000, timezone.utc).date()
    active, reverse = adapter.active_mappings(metadata, day, set(query['symbols']))
    if set(active) != set(query['symbols']) or len(reverse) != len(query['symbols']):
        raise ValueError('complete dated mappings required')
    return dict(schema='AutoWheelNativeBookWindowBoundaryV1',
                query_hash=hashlib.sha256(canonical).hexdigest(),
                operation_id=hashlib.sha256(canonical + b'\0/batch.submit_job').hexdigest(),
                source_start_ns=start, source_end_exclusive_ns=end,
                source_index='ts_recv', closed_cut_ns=cut, max_age_nanos=max_age,
                utc_start_ns=utc_start, instruments=sorted(reverse), mappings=reverse,
                metadata_exactly_matches_query=True, complete_utc_day_source=False,
                acquisition_proof_verified=False, complete_native_verified=False,
                source_qualified=False, pending_order_coverage_proven=False,
                historical_performance_available=False)
