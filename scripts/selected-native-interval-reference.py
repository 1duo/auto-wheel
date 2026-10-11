#!/usr/bin/env python3
"""Independent selected-output reconstruction; no acquisition or qualification.

The outer harness must attest metadata/mappings, consume the entire original
source, check its digest and decompressor EOF, and publish only after stability
checks. This module compares projected packets while reducing EVERY record.
Masks alone do not establish causal selection or continuous order coverage.
"""
from pathlib import Path
from types import SimpleNamespace

import importlib.util
import json


ROOT = Path(__file__).resolve().parents[1]
COMMON_PATH = ROOT / "scripts/verify-native-execution-intervals.py"
STOCK_PATH = ROOT / "scripts/verify-native-full-study-GFD-stock-intervals.py"


def module(name, path):
    spec = importlib.util.spec_from_file_location(name, path)
    value = importlib.util.module_from_spec(spec)
    spec.loader.exec_module(value)
    return value


common = module("selected_interval_common", COMMON_PATH)
stock = module("selected_interval_stock", STOCK_PATH)
old = stock.old
MAX_OBSERVATIONS = 1 << 22
MAX_WINDOWS = 86400
MAX_INSTRUMENTS = 2000


def integer(value, low, high):
    if type(value) is not int or not low <= value <= high:
        raise ValueError("bounded integer required")
    return value


def normalized_plan(plan, reverse, start, max_age):
    """Validate before any source/packet reads; copy masks and window values."""
    integer(start, 1, old.MAX_UINT64 - old.DAY_NANOS)
    integer(max_age, 0, old.DAY_NANOS)
    if start % old.DAY_NANOS or not isinstance(reverse, dict) or not 1 <= len(reverse) <= MAX_INSTRUMENTS:
        raise ValueError("complete universe or UTC scope differs")
    for ident, symbol in reverse.items():
        integer(ident, 1, old.MAX_UINT32)
        if not isinstance(symbol, str) or not symbol:
            raise ValueError("complete mapped identity required")
    if not isinstance(plan, list) or not 1 <= len(plan) <= MAX_WINDOWS:
        raise ValueError("bounded nonempty plan required")
    total = 0
    # Preflight the total before allocating any copied masks.
    for entry in plan:
        if not isinstance(entry, dict) or set(entry) != {"Window", "Instruments"}:
            raise ValueError("selected entry shape differs")
        ids = entry["Instruments"]
        if not isinstance(ids, list) or not 1 <= len(ids) <= len(reverse):
            raise ValueError("bounded nonempty mask required")
        total += len(ids)
        if total > MAX_OBSERVATIONS:
            raise ValueError("selected observation budget exceeded")
    windows, masks = [], []
    for entry in plan:
        window = entry["Window"]
        if not isinstance(window, dict) or set(window) != {"From", "Through"}:
            raise ValueError("window shape differs")
        first = integer(window["From"], start, start + old.DAY_NANOS - 1)
        through = integer(window["Through"], first + 1, start + old.DAY_NANOS - 1)
        if through - first > common.SECOND or windows and first < windows[-1][1]:
            raise ValueError("overlapping or oversized window")
        ids = entry["Instruments"]
        for ident in ids:
            integer(ident, 1, old.MAX_UINT32)
            if ident not in reverse:
                raise ValueError("mask outside complete universe")
        if len(set(ids)) != len(ids):
            raise ValueError("duplicate selected identity")
        windows.append((first, through))
        masks.append(tuple(sorted(ids)))
    return tuple(windows), tuple(masks)


class Reference:
    """Full-stream independent Python state, with selected packet comparison."""

    # Reuse only the accepted independent clock/status/folding rules. Avoid
    # inheriting a second public record path with different schema validation.
    status = stock.Reference.status
    start_window = stock.Reference.start_window
    fold = common.Reference.fold
    advance = common.Reference.advance

    def __init__(self, produced, plan, start, max_age, metadata, reverse):
        self.windows, self.masks = normalized_plan(plan, reverse, start, max_age)
        self.schema = str(metadata.schema)
        if ((metadata.dataset, self.schema) not in (("OPRA.PILLAR", "cmbp-1"), ("EQUS.MINI", "mbp-1"))
                or type(metadata.ts_out) is not bool or type(metadata.version) is not int
                or metadata.version not in (1, 2, 3)):
            raise ValueError("native schema or version differs")
        self.dataset = metadata.dataset
        self.old, self.produced = old, produced
        self.start, self.max_age = start, max_age
        self.metadata = SimpleNamespace(version=metadata.version, ts_out=metadata.ts_out)
        self.reverse = dict(reverse)
        self.ids = sorted(self.reverse)
        self.index, self.active = 0, False
        self.states = {ident: dict(book=None, trade=None, avail=0, ambiguous=False,
                                  has_update=False, last_update=0, last_touched=0,
                                  pending=False, changed=False, working=None,
                                  working_ambiguous=False) for ident in self.ids}
        self.counts = dict(windows=0, observations=0, raw_valid_observations=0,
                           receive_fresh_observations=0, event_fresh_observations=0,
                           minimum_present_observations=0)
        self.previous_receive = None
        self.records_consumed = self.records_reduced = 0
        self.failed = self.finished = False

    def finish_window(self):
        first, through = self.windows[self.index]
        covered = through >= self.max_age and through - self.max_age >= self.start
        rows = []
        # State/validation remain complete-universe; the mask changes output only.
        for ident in self.masks[self.index]:
            state = self.states[ident]
            self.fold(state, through)
            endpoint = [ident, state["book"] is not None, state["book"], state["avail"],
                        state["trade"] is not None, state["trade"], state["pending"],
                        self.status(state, through), state["has_update"], state["last_update"],
                        covered, covered and (not state["has_update"] or state["last_update"] < through - self.max_age)]
            facts = state["facts"]
            rows.append([ident, *facts, endpoint])
            for key, value in zip(("minimum_present_observations", "raw_valid_observations",
                                   "receive_fresh_observations", "event_fresh_observations"),
                                  (facts[0], facts[3], facts[4], facts[5])):
                self.counts[key] += int(value)
        expected = [self.dataset, self.start, first, through, self.max_age, rows]
        canonical = lambda value: json.dumps(value, ensure_ascii=False, separators=(",", ":"), allow_nan=False)
        if canonical(self.produced.next()) != canonical(expected):
            raise ValueError("independent selected interval packet differs")
        self.counts["windows"] += 1
        self.counts["observations"] += len(rows)
        self.index += 1
        self.active = False

    def _apply(self, vector):
        # Same independently audited multipart state rules as the stock
        # reference; schema validation and level comparison apply to both wires.
        recv, ident = vector[5], vector[4]
        old._validate_vector(vector, self.records_consumed, self.metadata.version, self.reverse,
                             self.schema, self.metadata.ts_out, self.start, self.start + old.DAY_NANOS)
        if self.previous_receive is not None and recv < self.previous_receive:
            raise ValueError("complete receive order regressed")
        self.advance(recv)
        state = self.states[ident]
        if self.active and recv > max(self.windows[self.index][0], state["last_touched"]):
            self.fold(state, recv)
        if not state["pending"]:
            state["changed"] = False
            state["working_ambiguous"] = state["ambiguous"]
        state["pending"] = True
        if vector[10] == "T":
            state["trade"] = vector
            base = state["working"] if state["changed"] else state["book"]
            if base is not None and base[10] != "R" and not old._same_levels(base, vector, self.schema):
                state["working_ambiguous"] = True
        else:
            state.update(working=vector, changed=True, working_ambiguous=False,
                         has_update=True, last_update=recv)
        if vector[12] & old.LAST_FLAG:
            if state["changed"]:
                state["book"], state["avail"] = state["working"], recv
            state["ambiguous"] = state["working_ambiguous"]
            state["pending"], state["changed"] = False, False
        state["last_touched"] = recv
        if self.active and not state["pending"]:
            self.fold(state, recv)
        self.previous_receive = recv
        self.records_consumed += 1
        self.records_reduced += int(self.active)

    def consume_chunk(self, raw, base_index, length):
        if self.failed or self.finished:
            raise ValueError("failed or completed reconstruction")
        try:
            if (base_index != self.records_consumed or length != 80 + 8 * self.metadata.ts_out
                    or len(raw) % length):
                raise ValueError("native record boundary differs")
            decode = common.native_vector if self.schema == "cmbp-1" else stock.native_vector
            for offset in range(0, len(raw), length):
                self._apply(decode(raw, offset, self.records_consumed, self.metadata, self.reverse))
        except Exception:
            self.failed = True
            raise

    def finish_reconstruction(self, expected_records):
        """Return packet comparison counts, NOT native/source acceptance."""
        if self.failed or self.finished:
            raise ValueError("failed or completed reconstruction")
        try:
            integer(expected_records, 0, old.MAX_UINT64)
            if self.records_consumed != expected_records:
                raise ValueError("complete record count differs")
            self.advance(self.start + old.DAY_NANOS)
            if self.index != len(self.windows):
                raise ValueError("selected schedule incomplete")
            self.produced.eof()
            self.finished = True
            return dict(self.counts, records_consumed=self.records_consumed,
                        records_reduced_within_windows=self.records_reduced,
                        causal_selection_qualified=False, continuous_pending_order_coverage_qualified=False,
                        full_source_qualification=False, historical_performance_available=False)
        except Exception:
            self.failed = True
            raise
