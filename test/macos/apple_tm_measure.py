# SPDX-License-Identifier: AGPL-3.0-only
"""Numeric geometry observations. No names, plist bodies, or file data exported."""
import os
from pathlib import Path
import plistlib
import stat
from xml.parsers.expat import ExpatError

GIB = 1 << 30
BAND_BYTES = 8 * GIB
BACKING_CAP = 24 * GIB
RAW_CAP = 24 * GIB
RESERVE = 20 * GIB
CIPHER_CAP = 26 * GIB
PREFLIGHT = 96 * GIB
CAPTURE_BUFFER = 32 << 20


def capacity(path):
    v = os.statvfs(path)
    return dict(total_bytes=v.f_blocks * v.f_frsize,
                free_bytes=v.f_bfree * v.f_frsize,
                available_bytes=v.f_bavail * v.f_frsize)


def geometry(directory):
    result = dict(parse='absent', bundle_count=0, band_size_bytes=None,
                  virtual_size_bytes=None, band_files=None, band_logical_bytes=None,
                  band_allocated_bytes=None, largest_band_bytes=None,
                  bands_over_4gib=None, target_band_geometry=False)
    bundles = [p for p in Path(directory).glob('*.sparsebundle') if p.is_dir() and not p.is_symlink()]
    result['bundle_count'] = len(bundles)
    if len(bundles) != 1:
        result['parse'] = 'absent' if not bundles else 'ambiguous'
        return result
    try:
        info = bundles[0] / 'Info.plist'
        if info.is_symlink() or info.stat().st_size > 1 << 20:
            raise ValueError('invalid metadata')
        with info.open('rb') as f:
            data = plistlib.load(f)
        for source, target in [('band-size', 'band_size_bytes'), ('size', 'virtual_size_bytes')]:
            value = data.get(source)
            if type(value) is not int or not 0 < value < 1 << 63:
                raise ValueError('invalid geometry')
            result[target] = value
        bands = bundles[0] / 'bands'
        if bands.is_symlink() or not bands.is_dir():
            raise ValueError('invalid bands')
        count = logical = allocated = largest = large = 0
        for p in bands.iterdir():
            s = p.lstat()
            if not stat.S_ISREG(s.st_mode):
                raise ValueError('invalid band entry')
            count += 1
            logical += s.st_size
            allocated += s.st_blocks * 512
            largest = max(largest, s.st_size)
            large += s.st_size > 4 * GIB
        result.update(parse='ok', band_files=count, band_logical_bytes=logical,
                      band_allocated_bytes=allocated, largest_band_bytes=largest,
                      bands_over_4gib=large, target_band_geometry=result['band_size_bytes'] == BAND_BYTES)
    except (OSError, ValueError, TypeError, AttributeError, ExpatError, plistlib.InvalidFileException):
        result['parse'] = 'invalid'
        result['target_band_geometry'] = False
    return result


def budget_admitted(free_bytes):
    # Ciphertext may be as large as raw; compression is not a capacity promise.
    return free_bytes >= PREFLIGHT and free_bytes >= BACKING_CAP + RAW_CAP + GIB + CIPHER_CAP + RESERVE


def initial_result():
    return dict(workload='time-machine', lifecycle='fresh', attempt=1,
                capability_stage='not-run', run_stage='not-run', cleanup_stage='not-run',
                retention_stage='not-run', measurement_stage='not-run',
                command_exit=None, command_timeout=None, elapsed_ms=None,
                harness_aborted=False, workload_stop_reason='not-started',
                selection_check='not-run', created_tree='not-run', restore='not-run',
                recovery='not-run', short_header_count=None, invalid_transport_count=None,
                server_log_scope='not-applicable', capture_health_valid=False,
                capture_completeness_proven=False, pairwise_exposure_match='unknown',
                protocol_match='unknown', source_fixture_seed=64,
                thin_provisioned=True, outer_virtual_bytes=1 << 50,
                physical_backing_cap_bytes=BACKING_CAP, capture_cap_bytes=RAW_CAP,
                physical_reserve_bytes=RESERVE, ciphertext_allowance_bytes=CIPHER_CAP,
                physical_preflight_bytes=PREFLIGHT, resource_guard_hit=False)
