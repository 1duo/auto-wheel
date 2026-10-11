#!/usr/bin/env python3
"""Generate synthetic Go decoder controls using pinned official DBN getters."""
import base64
import hashlib
import importlib.metadata
import json
from pathlib import Path
import sys


def main():
    if importlib.metadata.version("databento-dbn") != "0.71.0":
        raise ValueError("pinned decoder required")
    import databento_dbn as dbn
    fixtures = []
    for schema, dataset, constructor, level in [
        ("cmbp-1", "OPRA.PILLAR", dbn.CMBP1Msg,
         dbn.ConsolidatedBidAskPair(bid_px=-9223372036854775808, ask_px=9223372036854775807,
                                   bid_sz=4294967295, ask_sz=17, bid_pb=65535, ask_pb=41)),
        ("mbp-1", "EQUS.MINI", dbn.MBP1Msg,
         dbn.BidAskPair(bid_px=-9223372036854775808, ask_px=9223372036854775807,
                       bid_sz=4294967295, ask_sz=17, bid_ct=4294967295, ask_ct=19)),
    ]:
        for version in (1, 2, 3):
            for ts_out in (False, True):
                metadata = dbn.Metadata(dataset=dataset, start=1775433600000000000,
                                        end=1775520000000000000, stype_in=dbn.SType.RAW_SYMBOL,
                                        stype_out=dbn.SType.INSTRUMENT_ID,
                                        schema=dbn.Schema.CMBP_1 if schema == "cmbp-1" else dbn.Schema.MBP_1,
                                        symbols=["NFLX"], limit=None, ts_out=ts_out, version=version)
                header = metadata.encode()
                records = []
                for index in range(2):
                    kwargs = dict(publisher_id=65535, instrument_id=4294967295,
                                  ts_event=18446744073709551615 if index == 0 else 1775471400000000001,
                                  price=-9223372036854775808 if index == 0 else 9223372036854775807,
                                  size=4294967295, action=dbn.Action.TRADE if index == 0 else dbn.Action.MODIFY,
                                  side=dbn.Side.NONE if index == 0 else dbn.Side.ASK,
                                  ts_recv=1775471400000000002-index, flags=255,
                                  ts_in_delta=-2147483648 if index == 0 else 2147483647, levels=level)
                    if schema == "cmbp-1":
                        kwargs["rtype"] = 177
                    else:
                        kwargs.update(depth=255, sequence=4294967295)
                    if ts_out:
                        kwargs["ts_out"] = 1775471400000000003
                    encoded = bytes(constructor(**kwargs))
                    if ts_out and index == 0:
                        # The constructor uses UNDEF_TIMESTAMP to mean no
                        # appended field. Preserve an explicit wire sentinel
                        # in an already correctly sized WithTsOut record.
                        encoded = encoded[:80] + (18446744073709551615).to_bytes(8, "little")
                    records.append(encoded)
                raw = header + b"".join(records)
                decoder = dbn.DBNDecoder(upgrade_policy=dbn.VersionUpgradePolicy.AS_IS,
                                         compression=dbn.Compression.NONE)
                decoded = decoder.write_and_decode(raw)
                if decoder.buffer() or len(decoded) != 3 or decoded[0].version != version:
                    raise ValueError("synthetic AS_IS decoding differs")
                expected = []
                for index, row in enumerate(decoded[1:]):
                    value = dict(Index=index, Version=version, Length=len(records[index]),
                                 RType=int(row.rtype), PublisherID=row.publisher_id, InstrumentID=row.instrument_id,
                                 TsEvent=row.ts_event, Price=row.price, Size=row.size, Action=ord(str(row.action)),
                                 Side=ord(str(row.side)), Flags=row.flags, TsRecv=row.ts_recv, TsInDelta=row.ts_in_delta,
                                 BidPx=row.bid_px_00, AskPx=row.ask_px_00, BidSz=row.bid_sz_00, AskSz=row.ask_sz_00,
                                 HasTsOut=ts_out, TsOut=row.ts_out if ts_out else 0)
                    if schema == "cmbp-1":
                        value.update(BidPublisher=row.bid_pb_00, AskPublisher=row.ask_pb_00)
                    else:
                        value.update(BidCount=row.bid_ct_00, AskCount=row.ask_ct_00, Depth=row.depth, Sequence=row.sequence)
                    expected.append(value)
                fixtures.append(dict(schema=schema, version=version, ts_out=ts_out, metadata_bytes=len(header),
                                     native_base64=base64.b64encode(raw).decode(), records=expected))
    output = dict(generator_version="0.71.0", upgrade_policy="AS_IS", synthetic_only=True,
                  generator_sha256=hashlib.sha256(Path(__file__).read_bytes()).hexdigest(), fixtures=fixtures)
    Path(sys.argv[1]).write_text(json.dumps(output, indent=2) + "\n")
    print(json.dumps(dict(synthetic_fixtures=len(fixtures), reference_records=24)))


if __name__ == "__main__":
    main()
