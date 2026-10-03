# SPDX-License-Identifier: AGPL-3.0-only
import json
from pathlib import Path
import tempfile
import unittest
from publish_diagnostic import publish


class PublishTest(unittest.TestCase):
    def test_no_native_app_system_auth_or_failure_payload_export(self):
        with tempfile.TemporaryDirectory() as root:
            root = Path(root)
            private = root / 'private'
            private.mkdir()
            sentinel = 'NEVER-EXPORT-AUTH-PAYLOAD'
            (private / 'commands.jsonl').write_text(json.dumps(dict(argv=['security', '-w', sentinel],
                start='2026-01-01', end='2026-01-02', exit=0)) + '\n')
            (private / 'acceptance.jsonl').write_text(json.dumps(dict(event='time-machine-progress', time='2026',
                native_status=sentinel, free_bytes=123)) + '\n')
            (private / 'failure.txt').write_text(sentinel)
            (private / 'application-1-initialize.log').write_text(json.dumps(dict(time='2026-01-01T00:00:00Z',
                msg='short client packet header', password=sentinel)))
            (private / '0010-log.log').write_text(json.dumps([dict(timestamp='2026',
                eventMessage=sentinel + ' BACKUP_FAILED_DISCONNECTED_NETWORK (26)')]))
            (private / 'private.pcap').write_text(sentinel)
            publish(private, root / 'public')
            all_bytes = b''.join(p.read_bytes() for p in (root / 'public').rglob('*') if p.is_file())
            self.assertNotIn(sentinel.encode(), all_bytes)
            self.assertIn(b'short client packet header', all_bytes)
            self.assertIn(b'BACKUP_FAILED_DISCONNECTED_NETWORK', all_bytes)
            self.assertFalse((root / 'public/failure.txt').exists())


if __name__ == '__main__':
    unittest.main()
